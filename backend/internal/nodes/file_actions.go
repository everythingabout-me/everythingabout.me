package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/filerevision"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
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
		if effect.SourceEntry != nil && (effect.SourceEntry.Path != effect.Source || !bytes.Equal(effect.SourceEntry.Version, effect.SourceVersion)) {
			return nil, fileops.E("invalid_native_result", nil)
		}
		switch effect.Action {
		case "delete":
			// SourceVersion is captured unchanged-source proof, not a fresh DB lookup.
			if effect.Source == "" || effect.Target != "" || len(effect.SourceVersion) == 0 || result.Entry != nil {
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
			} else if effect.Source == "" || effect.Source == effect.Target || len(effect.SourceVersion) == 0 || (entry.Kind == storage.File && entry.Size != effect.Size) {
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
	for source, effect := range deletes {
		if effect.SourceEntry != nil {
			removedFolders[source] = effect.SourceEntry.Kind == storage.Folder
		} else {
			var folder bool
			if err := tx.QueryRow(ctx, `select exists(select 1 from file_entries where owner_id=$1::uuid and path=$2 and is_dir)`, op.Root.OwnerID, string(source)).Scan(&folder); err != nil {
				return nil, err
			}
			removedFolders[source] = folder
		}
	}

	sourceIDs := make(map[storage.Path]string)
	sourceRevisions := make(map[storage.Path]*string)
	if op.Kind == "move" {
		for _, item := range applied {
			source := item.effect.Source
			if item.effect.Action == "mkdir" && (item.effect.Target == request.To || strings.HasPrefix(string(item.effect.Target), string(request.To)+"/")) {
				source = storage.Path(string(request.From) + strings.TrimPrefix(string(item.effect.Target), string(request.To)))
			}
			if source == "" {
				continue
			}
			var id string
			err := tx.QueryRow(ctx, `select id::text from nodes where owner_id=$1::uuid and id=file_tree_resolve($1::uuid,$2)`, op.Root.OwnerID, string(source)).Scan(&id)
			if err != nil && err != pgx.ErrNoRows {
				return nil, err
			}
			if err == nil {
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
	removedDescendantIDs := make(map[storage.Path][]string)
	for source, folder := range removedFolders {
		if !folder {
			continue
		}
		rows, err := tx.Query(ctx, `select id::text from file_entries where owner_id=$1::uuid and path like $2`, op.Root.OwnerID, EscapeLike(string(source))+"/%")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			removedDescendantIDs[source] = append(removedDescendantIDs[source], id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	// A newly created native folder was absent before this effect. Retained
	// descendant index rows at that destination are stale, not copied contents.
	for _, item := range applied {
		if item.effect.Action != "mkdir" {
			continue
		}
		pattern := EscapeLike(string(item.effect.Target)) + "/%"
		if _, err := tx.Exec(ctx, `delete from nodes where id in(select id from file_entries where owner_id=$1::uuid and path like $2)`, op.Root.OwnerID, pattern); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `delete from file_content_generations where owner_id=$1::uuid and root_namespace=$2 and node_id is null and path like $3`, op.Root.OwnerID, fileRootNamespace(op.Root), pattern); err != nil {
			return nil, err
		}
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
				if effect.Action == "copy" && (effect.Source != source || !bytes.Equal(effect.SourceVersion, deletion.SourceVersion)) {
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
	for source, folder := range removedFolders {
		if !folder {
			continue
		}
		keep := make(map[string]bool)
		for moved := range movedSources {
			keep[sourceIDs[moved]] = true
		}
		var staleIDs []string
		for _, id := range removedDescendantIDs[source] {
			if !keep[id] {
				staleIDs = append(staleIDs, id)
			}
		}
		pattern := EscapeLike(string(source)) + "/%"
		if _, err := tx.Exec(ctx, `delete from nodes where owner_id=$1::uuid and id=any($2::uuid[])`, op.Root.OwnerID, staleIDs); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `delete from file_content_generations where owner_id=$1::uuid and root_namespace=$2 and node_id is null and (path=$3 or path like $4)`, op.Root.OwnerID, fileRootNamespace(op.Root), string(source), pattern); err != nil {
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
		if ok && (entry.Kind != final.Kind || entry.Size != final.Size || !bytes.Equal(entry.Version, final.Version) || !bytes.Equal(entry.NativeRef, final.NativeRef)) {
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

func fileRootNamespace(root fileops.Root) string { return root.Namespace() }

func actualEntryKey(root fileops.Root, entry storage.Entry) string {
	if entry.Kind == storage.Folder {
		return paths.DirKeyFor(string(entry.Path), root.OwnerID)
	}
	return paths.KeyFor(string(entry.Path), root.OwnerID)
}

// settleNewEntry is used only after native absence/create proof. A retained
// index row describes an earlier incarnation and must not donate its identity.
func retireStaleDestination(ctx context.Context, tx pgx.Tx, root fileops.Root, path storage.Path) error {
	pattern := EscapeLike(string(path)) + "/%"
	if _, err := tx.Exec(ctx, `delete from nodes where id in(select id from file_entries where owner_id=$1::uuid and (path=$2 or path like $3))`, root.OwnerID, string(path), pattern); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `delete from file_content_generations where owner_id=$1::uuid and root_namespace=$2 and node_id is null and (path=$3 or path like $4)`, root.OwnerID, fileRootNamespace(root), string(path), pattern)
	return err
}

func settleNewEntry(ctx context.Context, tx pgx.Tx, root fileops.Root, entry storage.Entry) error {
	if err := retireStaleDestination(ctx, tx, root, entry.Path); err != nil {
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
	var alreadyLinked bool
	if sourceID != "" {
		if err := tx.QueryRow(ctx, `select coalesce(file_tree_resolve($1::uuid,$2)=$3::uuid,false)`, root.OwnerID, string(entry.Path), sourceID).Scan(&alreadyLinked); err != nil {
			return err
		}
	}
	if !alreadyLinked {
		if err := retireStaleDestination(ctx, tx, root, entry.Path); err != nil {
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
