package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/filerevision"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CommitFileAction settles only the finite journal's proven native effects. It
// runs in the journal settlement transaction and never reads or changes bytes.
// In particular, it never expands a deletion or move from an index subtree.
func commitFileAction(ctx context.Context, tx pgx.Tx, op fileops.Operation, native *storage.MutationResult) (json.RawMessage, error) {
	var request fileops.ActionRequest
	if json.Unmarshal(op.Intent, &request) != nil || request.Kind != op.Kind || request.Root != op.Root {
		return nil, fileops.E("invalid_intent", nil)
	}
	switch op.Kind {
	case "mkdir", "copy", "move", "delete":
	default:
		return nil, fileops.E("invalid_intent", nil)
	}
	if _, err := tx.Exec(ctx, configureNameComparisonSQL, op.Root.OwnerID, nameComparisonVersion(op.Root.CaseSensitive)); err != nil {
		return nil, fmt.Errorf("configure root name comparison: %w", err)
	}
	if op.Kind == "move" && len(op.Entries) == 1 && op.Entries[0].Action == "move" && op.Entries[0].SourceEntry != nil && op.Entries[0].SourceEntry.Kind == storage.Folder {
		if op.Entries[0].State != "applied" {
			return nil, nil
		}
		return CommitNativeTreeMove(ctx, tx, op, native)
	}
	type proven struct {
		effect fileops.EntryEffect
		result storage.MutationResult
	}
	applied := make([]proven, 0, len(op.Entries))
	results := make(map[storage.Path]storage.Entry)
	deletes := make(map[storage.Path]fileops.EntryEffect)
	seen := make(map[int]bool)
	for _, effect := range op.Entries {
		if seen[effect.Number] {
			return nil, fileops.E("invalid_intent", nil)
		}
		seen[effect.Number] = true
		if effect.State != "applied" {
			continue
		}
		var result storage.MutationResult
		if json.Unmarshal(effect.Result, &result) != nil || len(result.Evidence) == 0 || !bytes.Equal(effect.Evidence, result.Evidence) {
			return nil, fileops.E("invalid_native_result", nil)
		}
		if storage.ValidatePath(effect.Source) != nil || storage.ValidatePath(effect.Target) != nil {
			return nil, fileops.E("invalid_intent", nil)
		}
		if effect.SourceEntry != nil && (effect.SourceEntry.Path != effect.Source || !bytes.Equal(effect.SourceEntry.Revision, effect.SourceRevision)) {
			return nil, fileops.E("invalid_native_result", nil)
		}
		switch effect.Action {
		case "delete":
			// SourceRevision is captured unchanged-source proof, not a fresh DB lookup.
			// A marker-less prefix folder has no native object, so there is no revision
			// to prove; fileops asks the driver for Conditions.ImplicitFolder and the
			// driver settles the removal only after the folder's last child is gone
			// (objectmutation.removeImplicitFolder). Every other delete still has to
			// name the exact native state it removed: a revision-less file, or a folder
			// whose marker existed, would otherwise be recorded gone on the driver's
			// word alone.
			if effect.Source == "" || effect.Target != "" || result.Entry != nil || len(effect.SourceRevision) == 0 && !implicitFolderRemoval(effect) {
				return nil, fileops.E("invalid_native_result", nil)
			}
			deletes[effect.Source] = effect
		case "mkdir", "copy", "move":
			entry := result.Entry
			if entry == nil || entry.Path != effect.Target || entry.Path == "" || entry.Size < 0 || entry.Size > 9007199254740991 || (entry.Kind != storage.File && entry.Kind != storage.Folder) || (entry.Kind == storage.Folder && entry.Size != 0) {
				return nil, fileops.E("invalid_native_result", nil)
			}
			if effect.Action == "mkdir" {
				if effect.Source != "" || entry.Kind != storage.Folder {
					return nil, fileops.E("invalid_native_result", nil)
				}
			} else if effect.Source == "" || effect.Source == effect.Target || len(effect.SourceRevision) == 0 || (entry.Kind == storage.File && entry.Size != effect.Size) {
				return nil, fileops.E("invalid_native_result", nil)
			}
			if _, duplicate := results[entry.Path]; duplicate {
				return nil, fileops.E("invalid_intent", nil)
			}
			results[entry.Path] = *entry
		default:
			return nil, fileops.E("invalid_intent", nil)
		}
		applied = append(applied, proven{effect, result})
	}
	sort.Slice(applied, func(i, j int) bool { return applied[i].effect.Number < applied[j].effect.Number })
	removedFolders := make(map[storage.Path]bool)
	var unknownFolders []string
	for source, effect := range deletes {
		if effect.SourceEntry != nil {
			removedFolders[source] = effect.SourceEntry.Kind == storage.Folder
			continue
		}
		unknownFolders = append(unknownFolders, string(source))
	}
	if len(unknownFolders) > 0 {
		folders, err := foldersAt(ctx, tx, op.Root.OwnerID, unknownFolders)
		if err != nil {
			return nil, err
		}
		for _, path := range unknownFolders {
			removedFolders[storage.Path(path)] = folders[path]
		}
	}
	var removedPaths []string
	for source, folder := range removedFolders {
		if folder {
			removedPaths = append(removedPaths, string(source))
		}
	}

	// The sources a move relinks, resolved in one round trip. This was one point query
	// per entry inside the owner's lock -- 35,374 of them for a move of 12k files.
	movedFrom := make([]storage.Path, len(applied))
	var resolve []string
	if op.Kind == "move" {
		for i, item := range applied {
			movedFrom[i] = moveSource(request, item.effect)
			if movedFrom[i] != "" {
				resolve = append(resolve, string(movedFrom[i]))
			}
		}
	}
	targets := make([]string, 0, len(applied))
	for _, item := range applied {
		if item.effect.Action != "delete" {
			targets = append(targets, string(item.result.Entry.Path))
		}
	}
	ids, err := resolveIDs(ctx, tx, op.Root.OwnerID, resolve)
	if err != nil {
		return nil, err
	}
	sourceIDs := make(map[storage.Path]string)
	sourceRevisions := make(map[storage.Path]*string)
	if op.Kind == "move" {
		for i, item := range applied {
			source := movedFrom[i]
			if source == "" {
				continue
			}
			if id, ok := ids[string(source)]; ok {
				sourceIDs[source] = id
			}
			if item.effect.SourceEntry != nil && item.effect.SourceEntry.Kind == storage.File {
				revision, err := lookupContentRevision(ctx, tx, op.Root.OwnerID, fileRootNamespace(op.Root), *item.effect.SourceEntry)
				if err != nil {
					return nil, err
				}
				sourceRevisions[source] = revision
			}
		}
	}
	// Only the outermost removed folders need expanding: a nested one is inside another
	// root, and the result below is "everything under any removed folder", so expanding it
	// again would return the same rows once per ancestor (56,648 rows for 12,322 distinct
	// descendants on a 12k-file move).
	removedRoots := topmostPaths(removedPaths)
	removedDescendantIDs, err := subtreeIDs(ctx, tx, op.Root.OwnerID, removedRoots)
	if err != nil {
		return nil, err
	}
	// A newly created native folder was absent before this effect. Retained
	// descendant index rows at that destination are stale, not copied contents.
	var created []string
	for _, item := range applied {
		if item.effect.Action == "mkdir" {
			created = append(created, string(item.effect.Target))
		}
	}
	if err := deleteSubtrees(ctx, tx, op.Root.OwnerID, created); err != nil {
		return nil, err
	}
	// Settlement is about to replace the index entries that own these paths, so a content
	// revision keyed by a path (`node_id IS NULL`) that describes the earlier incarnation is
	// stale, and leaving it would collide with the path key a replacement writes. It has to
	// happen here rather than per entry: see purgeStaleRevisions.
	purged := make(map[string]bool, len(targets)+len(removedPaths))
	for _, path := range targets {
		purged[path] = true
	}
	for _, path := range removedPaths {
		purged[path] = true
	}
	if err := purgeStaleRevisions(ctx, tx, op.Root, purged); err != nil {
		return nil, err
	}
	// A copy/delete move retains an old private ID only when both halves are
	// proven. A copied destination whose source survives is an independent entry.
	movedSources := make(map[storage.Path]bool)
	movedTargets := make(map[storage.Path]bool)
	if op.Kind == "move" {
		for _, item := range applied {
			effect := item.effect
			if effect.Action != "mkdir" && effect.Action != "copy" && effect.Action != "move" {
				continue
			}
			source := effect.Source
			if effect.Action != "move" {
				if effect.Target != request.To && !strings.HasPrefix(string(effect.Target), string(request.To)+"/") {
					continue
				}
				source = storage.Path(string(request.From) + strings.TrimPrefix(string(effect.Target), string(request.To)))
				deletion, ok := deletes[source]
				if !ok {
					continue
				}
				if effect.Action == "copy" && (effect.Source != source || !bytes.Equal(effect.SourceRevision, deletion.SourceRevision)) {
					return nil, fileops.E("invalid_native_result", nil)
				}
			}
			if err := settleMovedEntry(ctx, tx, op.Root, source, sourceIDs[source], sourceRevisions[source], *item.result.Entry); err != nil {
				return nil, err
			}
			movedSources[source] = true
			movedTargets[effect.Target] = true
		}
	}
	for _, item := range applied {
		effect := item.effect
		if effect.Action == "delete" {
			if movedSources[effect.Source] {
				continue
			}
			if _, err := tx.Exec(ctx, `delete from nodes where owner_id=$1::uuid and id=file_tree_resolve($1::uuid,$2)`, op.Root.OwnerID, string(effect.Source)); err != nil {
				return nil, fmt.Errorf("settle deleted index entry: %w", err)
			}
			if err := deleteContentRevision(ctx, tx, op.Root, effect.SourceEntry); err != nil {
				return nil, err
			}
			continue
		}
		if movedTargets[effect.Target] {
			continue
		}
		if err := settleNewEntry(ctx, tx, op.Root, *item.result.Entry); err != nil {
			return nil, err
		}
		if err := saveContentRevision(ctx, tx, op.Root, *item.result.Entry, filerevision.Content(fileRootNamespace(op.Root), *item.result.Entry)); err != nil {
			return nil, err
		}
	}
	// A proven removed folder cannot contain live native descendants. Drop stale
	// index-only descendants too, without expanding the captured native effects.
	// Their path-keyed content revisions went with purgeStaleRevisions above; bound
	// ones cascade with the rows.
	keep := make(map[string]bool, len(movedSources))
	for moved := range movedSources {
		keep[sourceIDs[moved]] = true
	}
	var staleIDs []string
	for _, root := range removedRoots {
		for _, id := range removedDescendantIDs[root] {
			if !keep[id] {
				staleIDs = append(staleIDs, id)
			}
		}
	}
	if len(staleIDs) > 0 {
		if _, err := tx.Exec(ctx, `delete from nodes where owner_id=$1::uuid and id=any($2::uuid[])`, op.Root.OwnerID, staleIDs); err != nil {
			return nil, err
		}
	}
	for _, effect := range op.Entries {
		if effect.State != "applied" {
			return nil, nil
		}
	}
	if op.Kind == "delete" {
		return json.Marshal(fc.DeleteResult{Deleted: len(applied) > 0})
	}
	entry, ok := results[request.To]
	var observation *fileops.ActionObservation
	if native != nil && native.Entry != nil {
		final := native.Entry
		if len(native.Evidence) == 0 || final.Path != request.To || storage.ValidatePath(final.Path) != nil || final.Size < 0 || final.Size > 9007199254740991 || (final.Kind != storage.File && final.Kind != storage.Folder) || (final.Kind == storage.Folder && final.Size != 0) {
			return nil, fileops.E("invalid_native_result", nil)
		}
		if ok && (entry.Kind != final.Kind || entry.Size != final.Size || !bytes.Equal(entry.Revision, final.Revision) || !bytes.Equal(entry.NativeRef, final.NativeRef)) {
			return nil, fileops.E("invalid_native_result", nil)
		}
		if !ok && !(op.Kind == "mkdir" && len(op.Entries) == 0 && final.Kind == storage.Folder) {
			return nil, fileops.E("invalid_native_result", nil)
		}
		var observed fileops.ActionObservation
		if json.Unmarshal(native.Evidence, &observed) != nil || !sameObservedEntry(observed.Entry, *final) {
			return nil, fileops.E("invalid_native_result", nil)
		}
		observation = &observed
		entry, ok = *final, true
		// Descendant creation changes a native folder's mtime after its own mkdir
		// result. The durable final observation is authoritative for that field.
		if err := settleIndexEntry(ctx, tx, op.Root, entry, len(op.Entries) == 0); err != nil {
			return nil, err
		}
	}
	if !ok {
		return nil, nil
	}
	membership := make([]string, 0)
	if entry.Kind == storage.Folder {
		children := results
		if observation != nil {
			children = make(map[storage.Path]storage.Entry, len(observation.Children))
			for _, child := range observation.Children {
				if child.Path == entry.Path || storage.ValidatePath(child.Path) != nil || paths.ParentOf(string(child.Path)) != string(entry.Path) || (child.Kind != storage.File && child.Kind != storage.Folder) {
					return nil, fileops.E("invalid_native_result", nil)
				}
				if _, duplicate := children[child.Path]; duplicate {
					return nil, fileops.E("invalid_native_result", nil)
				}
				children[child.Path] = child
			}
		}
		for _, child := range children {
			if paths.ParentOf(string(child.Path)) == string(entry.Path) {
				membership = append(membership, string(child.Kind)+":"+string(child.Path))
			}
		}
	}
	metadata, err := settledMetadata(ctx, tx, op.Root, entry, membership)
	if err != nil {
		return nil, err
	}
	return json.Marshal(metadata)
}

// implicitFolderRemoval reports the one delete a manifest may prove without a
// revision: capture saw a folder and no native revision for it. On an object store
// such a folder is only a prefix — it exists while its children do and has no marker
// object to name — so fileops expresses its removal as Conditions.ImplicitFolder
// (fileops executeAction) and the driver settles it without a native request
// (objectmutation removeImplicitFolder), returning evidence only. Settlement has to
// accept that receipt, or a move of any folder without a marker can never settle: the
// bytes have moved, every entry stays applied, the journal retries forever and the
// old path reads as native_absent. A file, or a folder that did carry a revision,
// still requires one.
func implicitFolderRemoval(effect fileops.EntryEffect) bool {
	return effect.SourceEntry != nil && effect.SourceEntry.Kind == storage.Folder && len(effect.SourceRevision) == 0
}

func fileRootNamespace(root fileops.Root) string { return root.Namespace() }

func actualEntryKey(root fileops.Root, entry storage.Entry) string {
	if entry.Kind == storage.Folder {
		return paths.DirKeyFor(string(entry.Path), root.OwnerID)
	}
	return paths.KeyFor(string(entry.Path), root.OwnerID)
}

// moveSource names the index path a move's entry leaves behind: the effect's own source, or,
// for the folder the move creates at its destination, the same folder at the origin.
func moveSource(request fileops.ActionRequest, effect fileops.EntryEffect) storage.Path {
	if effect.Action == "mkdir" && (effect.Target == request.To || strings.HasPrefix(string(effect.Target), string(request.To)+"/")) {
		return storage.Path(string(request.From) + strings.TrimPrefix(string(effect.Target), string(request.To)))
	}
	return effect.Source
}

// resolveIDs maps each path to the id of the index row currently at it, in one round trip.
// A path that resolves to nothing is absent from the result.
func resolveIDs(ctx context.Context, tx pgx.Tx, owner string, list []string) (map[string]string, error) {
	out := make(map[string]string, len(list))
	if len(list) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `select p.path,file_tree_resolve($1::uuid,p.path)::text from unnest($2::text[]) as p(path)`, owner, list)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var id *string
		if err := rows.Scan(&path, &id); err != nil {
			return nil, err
		}
		if id != nil {
			out[path] = *id
		}
	}
	return out, rows.Err()
}

// foldersAt reports which of the given paths are folders in the current index.
func foldersAt(ctx context.Context, tx pgx.Tx, owner string, list []string) (map[string]bool, error) {
	out := make(map[string]bool, len(list))
	if len(list) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `select p.path from unnest($2::text[]) as p(path) where exists(
 select 1 from nodes n where n.owner_id=$1::uuid and n.id=file_tree_resolve($1::uuid,p.path) and n.is_dir)`, owner, list)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out[path] = true
	}
	return out, rows.Err()
}

// subtreeIDs returns, for every root path that resolves, the ids of the index rows strictly
// below it, keyed by the root path. A root that resolves to nothing contributes nothing.
//
// This is the shape every "which index rows are under this path" question has to take.
// file_entries derives each row's current path with file_tree_path(), so `path LIKE 'x/%'`
// against the view cannot use an index: it read the whole tree and walked every row's
// ancestry — 2.7 s and 2.0 M buffer hits on an 89k-row index — and settlement asked that
// question once per removed folder and twice per created folder. Ancestry answers the same
// question with one index probe per child: 22 ms for the same 10k-row subtree.
//
// `union` rather than `union all` bounds a corrupt index: ancestry that cycles is a bug, and
// a walk that never ends would be the worse one. Each root is excluded from its own result
// because the caller handles the root row itself; a root that also sits below another root is
// still handled by its own entry.
func subtreeIDs(ctx context.Context, tx pgx.Tx, owner string, roots []string) (map[string][]string, error) {
	out := make(map[string][]string, len(roots))
	if len(roots) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `with recursive roots as (
 select p.path,file_tree_resolve($1::uuid,p.path) as id from unnest($2::text[]) as p(path)
), sub as (
 select path,id from roots where id is not null
 union
 select s.path,n.id from sub s join nodes n on n.owner_id=$1::uuid and n.parent_id=s.id
)
select path,id::text from sub where id not in(select id from roots where id is not null)`, owner, roots)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path, id string
		if err := rows.Scan(&path, &id); err != nil {
			return nil, err
		}
		out[path] = append(out[path], id)
	}
	return out, rows.Err()
}

// topmostPaths drops every path that already sits under another path in the set. Expanding
// a nested root returns rows the outer root's expansion already returned.
func topmostPaths(list []string) []string {
	sorted := append([]string(nil), list...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) < len(sorted[j]) })
	kept := make([]string, 0, len(sorted))
	for _, path := range sorted {
		nested := false
		for _, root := range kept {
			if strings.HasPrefix(path, root+"/") {
				nested = true
				break
			}
		}
		if !nested {
			kept = append(kept, path)
		}
	}
	return kept
}

// deleteSubtrees removes every index row strictly below each root path.
func deleteSubtrees(ctx context.Context, tx pgx.Tx, owner string, roots []string) error {
	if len(roots) == 0 {
		return nil
	}
	descendants, err := subtreeIDs(ctx, tx, owner, roots)
	if err != nil {
		return err
	}
	var ids []string
	for _, list := range descendants {
		ids = append(ids, list...)
	}
	if len(ids) == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, `delete from nodes where owner_id=$1::uuid and id=any($2::uuid[])`, owner, ids)
	return err
}

// purgeStaleRevisions removes the path-keyed content revisions that describe index entries
// settlement is about to replace. `list` is every path this settlement writes (each effect's
// destination and each removed folder), and a revision under one of them cannot describe the
// entry that will own the path: the path is the revision's unique key, so leaving it would
// collide with the row a replacement writes.
//
// It is one pass over the owner's unbound revisions because the per-entry alternative — a
// `path LIKE 'dir/%'` delete for each settled entry — cannot use the unique path index; it
// scanned the whole table (81k rows) 35,374 times, and again for every removed folder. Bound
// revisions (`node_id IS NOT NULL`) are not here: they cascade with their node row.
func purgeStaleRevisions(ctx context.Context, tx pgx.Tx, root fileops.Root, list map[string]bool) error {
	if len(list) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `select path from file_content_generations where owner_id=$1::uuid and root_namespace=$2 and node_id is null and path is not null`, root.OwnerID, fileRootNamespace(root))
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		if coversPath(list, path) {
			stale = append(stale, path)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, `delete from file_content_generations where owner_id=$1::uuid and root_namespace=$2 and node_id is null and path=any($3::text[])`, root.OwnerID, fileRootNamespace(root), stale)
	return err
}

// purgeStaleRevisionsUnder is purgeStaleRevisions for one path and everything below it. It
// keeps the standalone retireStaleDestination callers (a native tree move, an upload
// publication) correct without making them load the owner's whole set.
func purgeStaleRevisionsUnder(ctx context.Context, tx pgx.Tx, root fileops.Root, path string) error {
	pattern := EscapeLike(path) + "/%"
	_, err := tx.Exec(ctx, `delete from file_content_generations where owner_id=$1::uuid and root_namespace=$2 and node_id is null and (path=$3 or path like $4)`, root.OwnerID, fileRootNamespace(root), path, pattern)
	return err
}

// coversPath reports whether path, or any ancestor directory of it, is in the set: what
// `path LIKE 'dir/%'` meant for a fixed set of directories, without scanning for it.
func coversPath(set map[string]bool, path string) bool {
	if set[path] {
		return true
	}
	for parent := paths.ParentOf(path); parent != ""; parent = paths.ParentOf(parent) {
		if set[parent] {
			return true
		}
	}
	return false
}

// settleNewEntry is used only after native absence/create proof. A retained
// index row describes an earlier incarnation and must not donate its identity.
//
// The rows to retire are found by ancestry, not by filtering the file_entries view by path.
// That view derives every row's current path with file_tree_path(), so `path=$2 or path like
// $3` could use no index: it read the whole tree and walked every row's ancestry, which cost
// 93,914 buffer hits to select the 2,178 rows of one folder subtree, and it runs once per
// settlement inside the owner's lock. A subtree is exactly what a prefix meant for a path
// that is derived from identity, so resolving the destination once and walking parent_id
// deletes the same rows for one index probe per child: 6,415 buffer hits on that subtree, 12
// for a file path, whose subtree is itself.
//
// `union` rather than `union all` bounds a corrupt index: ancestry that cycles is a bug, and
// a delete that loops forever over it would be the worse one.
func retireStaleDestination(ctx context.Context, tx pgx.Tx, root fileops.Root, path storage.Path) error {
	if err := retireStaleRows(ctx, tx, root, path); err != nil {
		return err
	}
	return purgeStaleRevisionsUnder(ctx, tx, root, string(path))
}

// retireStaleRows is the index half of retireStaleDestination. Settlement calls it directly
// because it purges every path-keyed revision in one pass (purgeStaleRevisions), and the
// per-path `path LIKE 'dir/%'` delete cannot use the unique path index: it scanned the whole
// revisions table for each of the 35,374 settled entries.
func retireStaleRows(ctx context.Context, tx pgx.Tx, root fileops.Root, path storage.Path) error {
	_, err := tx.Exec(ctx, `with recursive stale as (
 select id from nodes where owner_id=$1::uuid and id=file_tree_resolve($1::uuid,$2)
 union
 select n.id from nodes n join stale s on n.parent_id=s.id where n.owner_id=$1::uuid
) delete from nodes where id in (select id from stale)`, root.OwnerID, string(path))
	return err
}

func settleNewEntry(ctx context.Context, tx pgx.Tx, root fileops.Root, entry storage.Entry) error {
	if err := retireStaleRows(ctx, tx, root, entry.Path); err != nil {
		return err
	}
	return settleActualEntry(ctx, tx, root, entry)
}

func settleActualEntry(ctx context.Context, tx pgx.Tx, root fileops.Root, entry storage.Entry) error {
	return settleIndexEntry(ctx, tx, root, entry, false)
}

func settleIndexEntry(ctx context.Context, tx pgx.Tx, root fileops.Root, entry storage.Entry, preserveUnchanged bool) error {
	_, err := tx.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime,source_modified_at)
 values($1::uuid,$2,$3,$4,$5,$6,$7,nullif($8,''),$9)
 on conflict(owner_id,parent_id,comparison_name) do update set path=excluded.path,key=excluded.key,parent=excluded.parent,name=excluded.name,
 is_dir=excluded.is_dir,size=excluded.size,mime=excluded.mime,source_modified_at=excluded.source_modified_at,updated_at=now()
 where not $10 or (nodes.key,nodes.parent,nodes.name,nodes.is_dir,nodes.size,nodes.mime,nodes.source_modified_at)
 is distinct from (excluded.key,excluded.parent,excluded.name,excluded.is_dir,excluded.size,excluded.mime,excluded.source_modified_at)`,
		root.OwnerID, actualEntryKey(root, entry), string(entry.Path), paths.ParentOf(string(entry.Path)), paths.NameOf(string(entry.Path)), entry.Kind == storage.Folder, entry.Size, entry.MediaType, nativeModified(entry), preserveUnchanged)
	if err != nil {
		return fmt.Errorf("settle native index entry: %w", err)
	}
	return nil
}

func settleMovedEntry(ctx context.Context, tx pgx.Tx, root fileops.Root, source storage.Path, sourceID string, revision *string, entry storage.Entry) error {
	// Resolved per entry, not up front: a moved folder relinks its descendants' ancestry,
	// so the answer for a child changes as its parent settles. A child already hanging
	// off the destination parent resolves there and must not be retired as stale.
	var alreadyLinked bool
	if sourceID != "" {
		if err := tx.QueryRow(ctx, `select coalesce(file_tree_resolve($1::uuid,$2)=$3::uuid,false)`, root.OwnerID, string(entry.Path), sourceID).Scan(&alreadyLinked); err != nil {
			return err
		}
	}
	if !alreadyLinked {
		if err := retireStaleRows(ctx, tx, root, entry.Path); err != nil {
			return err
		}
	}
	if sourceID == "" {
		if err := settleActualEntry(ctx, tx, root, entry); err != nil {
			return err
		}
	} else {
		_, err := tx.Exec(ctx, `update nodes set key=$3,path=$4,parent=$5,name=$6,is_dir=$7,size=$8,mime=nullif($9,''),source_modified_at=$10,updated_at=now()
 where owner_id=$1::uuid and id=$2::uuid`, root.OwnerID, sourceID, actualEntryKey(root, entry), string(entry.Path), paths.ParentOf(string(entry.Path)), paths.NameOf(string(entry.Path)), entry.Kind == storage.Folder, entry.Size, entry.MediaType, nativeModified(entry))
		if err != nil {
			return fmt.Errorf("settle moved index entry: %w", err)
		}
	}
	if entry.Kind == storage.File {
		if revision == nil {
			revision = filerevision.Content(fileRootNamespace(root), entry)
		}
		if err := saveContentRevision(ctx, tx, root, entry, revision); err != nil {
			return err
		}
	}
	return nil
}

func nativeModified(entry storage.Entry) *time.Time {
	if entry.ModifiedAt.IsZero() {
		return nil
	}
	modified := entry.ModifiedAt.UTC()
	return &modified
}

func settledMetadata(ctx context.Context, tx pgx.Tx, root fileops.Root, entry storage.Entry, _ []string) (fc.Metadata, error) {
	record, err := fileBrowseMetadata(ctx, tx, root.OwnerID, fileRootNamespace(root), string(entry.Path), nil)
	if err != nil {
		return fc.Metadata{}, err
	}
	if record == nil {
		return fc.Metadata{}, fileops.E("invalid_native_result", nil)
	}
	return IndexedMetadata(fileRootNamespace(root), *record)
}

// CommitPrefixDelete settles an accepted recursive deletion in the transaction that
// accepts it: the subtree leaves the index, the change feed records one subtree
// deletion, the content revisions and stale generations go with the rows, and the bytes
// it removed are returned so the journal refunds them in the same commit.
//
// Doing this before the bytes leave the provider is what makes the deletion final at
// `202`: every read resolves through these rows, so once they commit the subtree is
// gone for the user even while the purge is still running. It is also what frees the
// namespace: there is no concealed-but-live row for a re-creation to collide with.
func CommitPrefixDelete(ctx context.Context, tx pgx.Tx, op fileops.Operation) (int64, error) {
	var request fileops.ActionRequest
	if json.Unmarshal(op.Intent, &request) != nil || request.Kind != "delete" || request.From == "" {
		return 0, fileops.E("invalid_intent", nil)
	}
	namespace := fileRootNamespace(op.Root)
	if _, err := filechanges.BindRoot(ctx, tx, op.Root.OwnerID, namespace); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, configureNameComparisonSQL, op.Root.OwnerID, nameComparisonVersion(op.Root.CaseSensitive)); err != nil {
		return 0, fmt.Errorf("configure root name comparison: %w", err)
	}
	path := string(request.From)
	before, err := captureFileState(ctx, tx, op.Root.OwnerID, namespace, path)
	if err != nil {
		return 0, err
	}
	if before.metadata == nil {
		return 0, storage.NewError("not_found", request.From, nil)
	}
	pattern := EscapeLike(path) + "/%"
	rows, err := tx.Query(ctx, `select id::text, size from file_entries where owner_id=$1::uuid and path=$2`, op.Root.OwnerID, path)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, 64)
	var removed int64
	for rows.Next() {
		var id string
		var size int64
		if err = rows.Scan(&id, &size); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		removed += size
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	descendants, err := tx.Query(ctx, `select id::text, size from file_entries where owner_id=$1::uuid and path like $2`, op.Root.OwnerID, pattern)
	if err != nil {
		return 0, err
	}
	for descendants.Next() {
		var id string
		var size int64
		if err = descendants.Scan(&id, &size); err != nil {
			descendants.Close()
			return 0, err
		}
		ids = append(ids, id)
		removed += size
	}
	descendants.Close()
	if err = descendants.Err(); err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		if _, err = tx.Exec(ctx, `delete from nodes where owner_id=$1::uuid and id=any($2::uuid[])`, op.Root.OwnerID, ids); err != nil {
			return 0, fmt.Errorf("settle prefix deletion: %w", err)
		}
	}
	if _, err = tx.Exec(ctx, `delete from file_content_generations where owner_id=$1::uuid and root_namespace=$2 and (path=$3 or path like $4)`, op.Root.OwnerID, namespace, path, pattern); err != nil {
		return 0, err
	}
	// One subtree fact, not one per object: the feed's contract already lets a single
	// change retire a whole folder, and a deletion does not need to page the feed for
	// every file inside it.
	if err = settleFileChanges(ctx, tx, op, []fileEffectCapture{{key: op.ID + ":subtree", source: path, subtree: true, before: before}}); err != nil {
		return 0, err
	}
	// No result document: the operation's `result` belongs to the purge, which writes it
	// when the bytes are actually gone. A `{deleted:true}` from here would be published
	// next to `state: "running"` and would read as the operation's answer.
	return removed, nil
}

// LiveFilePaths answers which of these paths the index still holds. The purge must not
// delete a path a user re-created after the deletion was accepted, and the provider's
// own clock is not authoritative for the user's intent.
func LiveFilePaths(ctx context.Context, pool *pgxpool.Pool, owner string, paths []storage.Path) ([]bool, error) {
	live := make([]bool, len(paths))
	if len(paths) == 0 {
		return live, nil
	}
	values := make([]string, len(paths))
	for i, path := range paths {
		values[i] = string(path)
	}
	rows, err := pool.Query(ctx, `select path from file_entries where owner_id=$1::uuid and path=any($2)`, owner, values)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := make(map[string]bool, len(paths))
	for rows.Next() {
		var path string
		if err = rows.Scan(&path); err != nil {
			return nil, err
		}
		found[path] = true
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i, path := range paths {
		live[i] = found[string(path)]
	}
	return live, nil
}
