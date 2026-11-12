package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/filerevision"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// folderAtPathSQL answers "is a folder indexed at this relative path", and it resolves the
// path through canonical identity instead of filtering the file_entries view by path.
//
// The view derives every row's current path with file_tree_path(), a PL/pgSQL walk up the
// ancestry, so `where owner_id=$1 and path=$2` against it can use no index: it is a
// sequential scan with one ancestry walk per row. Measured on a 1.3k-entry tree, one such
// lookup cost 40 ms and 45,578 buffer hits, and it is paid once per admission and once per
// settlement while the owner's advisory lock is held — which made settlement the ceiling of
// an import at ~5 files/s, falling as the tree grows, because the scan is the whole tree.
// file_tree_resolve walks down instead: one index probe per path segment on
// nodes_owner_parent_comparison_uniq, then the row on nodes_pkey. The same answer costs 12
// buffer hits and 0.19 ms on a 2.2k-entry tree.
//
// The one behavioural difference is a row whose current path cannot be resolved from identity
// while its legacy path cache still carries one: the view would report it and this query does
// not. Identity is the authority for where an entry is, so a row no identity can address is
// not a destination — a stale cache must not answer a creation question.
const folderAtPathSQL = `select is_dir from nodes where owner_id=$1::uuid and id=file_tree_resolve($1::uuid,$2)`

// CommitStagedFile is the index half of a proven native file publication. It is
// called inside the journal's settlement transaction, while path guards are held.
// It never touches bytes, deletes a file, or claims an index write published data.
func commitStagedFile(ctx context.Context, tx pgx.Tx, op fileops.Operation, result *storage.MutationResult) (json.RawMessage, error) {
	var intent storage.WriteIntent
	if json.Unmarshal(op.Intent, &intent) != nil || result == nil || result.Entry == nil {
		return nil, fileops.E("invalid_native_result", nil)
	}
	entry := result.Entry
	if entry.Kind != storage.File || entry.Path != intent.Target || entry.Size != intent.Size || storage.ValidatePath(entry.Path) != nil {
		return nil, fileops.E("invalid_native_result", nil)
	}
	if _, err := tx.Exec(ctx, configureNameComparisonSQL, op.Root.OwnerID, nameComparisonVersion(op.Root.CaseSensitive)); err != nil {
		return nil, fmt.Errorf("configure root name comparison: %w", err)
	}
	p := string(entry.Path)
	// The bytes are in place, so the folders their path names are real: materialize the
	// ancestors a parents:true upload promised, then require them as usual. Doing this at
	// settlement rather than at admission is what keeps every row backed by the tree — a
	// commit that never publishes leaves no folder behind.
	parent := paths.ParentOf(p)
	if intent.Parents && parent != "" {
		if _, err := ensureUploadParents(ctx, tx, op.Root.OwnerID, p); err != nil {
			return nil, fmt.Errorf("materialize upload parents: %w", err)
		}
	}
	if parent != "" {
		var folder bool
		err := tx.QueryRow(ctx, folderAtPathSQL, op.Root.OwnerID, parent).Scan(&folder)
		if err != nil || !folder {
			return nil, fileops.E("parent_changed", err)
		}
	}
	var modified *time.Time
	if !entry.ModifiedAt.IsZero() {
		v := entry.ModifiedAt.UTC()
		modified = &v
	}
	if intent.Conditions.Absent {
		if err := retireStaleDestination(ctx, tx, op.Root, entry.Path); err != nil {
			return nil, fmt.Errorf("retire stale file identity: %w", err)
		}
	}
	tag, err := tx.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime,source_modified_at)
 values($1::uuid,$2,$3,$4,$5,false,$6,nullif($7,''),$8)
 on conflict(owner_id,parent_id,comparison_name) do update set path=excluded.path,key=excluded.key,parent=excluded.parent,name=excluded.name,
 size=excluded.size,mime=excluded.mime,source_modified_at=coalesce(excluded.source_modified_at,nodes.source_modified_at),updated_at=now()
 where nodes.is_dir=false`, op.Root.OwnerID, paths.KeyFor(p, op.Root.OwnerID), p, parent, paths.NameOf(p), entry.Size, entry.MediaType, modified)
	if err != nil {
		return nil, fmt.Errorf("settle file index: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, fileops.E("parent_changed", nil)
	}
	if err := saveContentRevision(ctx, tx, op.Root, *entry, filerevision.Content(fileRootNamespace(op.Root), *entry)); err != nil {
		return nil, err
	}
	metadata, err := settledMetadata(ctx, tx, op.Root, *entry, nil)
	if err != nil {
		return nil, err
	}
	return json.Marshal(metadata)
}

// CheckUploadBinding is the index half of freezing an upload session: it verifies, inside
// the freeze transaction, that what the session bound when it was opened is still what the
// index holds. The session fenced nothing while it was open, so a delete, a move or a
// re-creation may have settled since; this is where a session that can no longer publish
// where it was told to learns so, before any native effect.
//
// The binding is identity, not existence. A folder deleted and created again under the same
// name is a different folder, and a session opened against the first one must not land in
// the second (docs/api/api.md: a concurrent move or recreation cannot silently retarget an
// upload). So the captured ParentID has to resolve to the captured ParentPath, and for a
// replacement the captured EntryID to the target. A session without a captured ParentID
// still needs a live folder row at its parent path, because index settlement requires one
// (commitStagedFile): refusing here costs nothing, refusing there leaves a published object
// the index cannot hold.
//
// The answers are fileops codes the freeze turns into a terminal session: parent_missing
// when no folder is indexed at the destination's parent path, target_changed when one is
// but it is not the folder (or, for a replacement, the file) the session bound. Details
// name only the session's own paths, never where an entry went, because the caller's scope
// may not reach there.
func CheckUploadBinding(ctx context.Context, tx pgx.Tx, op fileops.Operation) error {
	var intent storage.WriteIntent
	if json.Unmarshal(op.Intent, &intent) != nil || intent.Target == "" {
		return fileops.E("invalid_intent", nil)
	}
	parent := string(intent.ParentPath)
	if parent == "" {
		parent = paths.ParentOf(string(intent.Target))
	}
	folder := "/" + parent
	// What is indexed at the parent path now, whichever identity it has. The root has no
	// row and always exists.
	occupied := parent == ""
	if !occupied {
		var isFolder bool
		err := tx.QueryRow(ctx, folderAtPathSQL, op.Root.OwnerID, parent).Scan(&isFolder)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		occupied = err == nil && isFolder
	}
	if !occupied && !intent.Parents {
		// A promised parent is materialized by the settlement that publishes the file, so a
		// session admitted against a destination that does not exist yet is still fenced by
		// its captured parent identity (below) rather than by a folder row it cannot have.
		return fileops.E("parent_missing", fmt.Errorf("no folder is indexed at %q (it was deleted or moved, or it exists only outside this service and has not been reconciled)", folder))
	}
	if intent.ParentID != "" {
		resolved, found, err := resolveFileID(ctx, tx, op.Root.OwnerID, intent.ParentID)
		if err != nil {
			return err
		}
		if !found {
			return fileops.E("target_changed", fmt.Errorf("the folder %q was deleted and created again, so it is not the folder this upload was opened for", folder))
		}
		if resolved != parent {
			return fileops.E("target_changed", fmt.Errorf("the folder this upload was opened for was moved, and %q is now a different folder", folder))
		}
	}
	if intent.EntryID != "" {
		resolved, found, err := resolveFileID(ctx, tx, op.Root.OwnerID, intent.EntryID)
		if err != nil {
			return err
		}
		if !found || resolved != string(intent.Target) {
			return fileops.E("target_changed", fmt.Errorf("the file being replaced, %q, was moved or deleted", "/"+string(intent.Target)))
		}
	}
	return nil
}
