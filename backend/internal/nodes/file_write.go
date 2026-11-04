package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/filerevision"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

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
	// Parents must have been admitted/captured; index settlement never invents a
	// native folder or runs the old EnsureParents side effect.
	parent := paths.ParentOf(p)
	if parent != "" {
		var folder bool
		err := tx.QueryRow(ctx, `select is_dir from file_entries where owner_id=$1::uuid and path=$2`, op.Root.OwnerID, parent).Scan(&folder)
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
