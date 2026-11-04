package nodes

import (
	"context"

	fc "everythingabout.me/backend/internal/filecontract"
	"github.com/jackc/pgx/v5"
)

// CaptureFileSnapshot reads a complete bounded indexed baseline in the caller's
// transaction. The selected entry comes first, followed by its direct children or
// all descendants in canonical path order. It never observes native storage or
// issues a query per result. Callers capture the feed head in the same transaction.
func CaptureFileSnapshot(ctx context.Context, tx pgx.Tx, owner, namespace, selectedPath string, recursive bool, limit int, hiddenRoots ...string) ([]fc.Metadata, error) {
	if tx == nil || limit < 1 || limit > 100000 {
		return nil, fc.NewProblem(400, "invalid_request", "The snapshot entry bound is invalid.")
	}
	if _, err := fc.FromRelative(selectedPath); err != nil {
		return nil, err
	}
	root, err := fileBrowseMetadata(ctx, tx, owner, namespace, selectedPath, hiddenRoots)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fc.NewProblem(404, "not_found", "The snapshot target was not found.")
	}
	metadata, err := IndexedMetadata(namespace, *root)
	if err != nil {
		return nil, err
	}
	items := []fc.Metadata{metadata}
	if !root.Row.IsDir {
		return items, nil
	}
	rows, err := tx.Query(ctx, `select `+browseColumns+` from file_entries nodes
 where owner_id=$1::uuid and `+browseVisible+` and id<>$4::uuid
 and (($5::boolean and file_changes_within('/'||$6,'/'||path,`+browseVersion+`))
 or (not $5 and parent_id=$4::uuid))
 order by normalize(path,NFC) collate "C",id limit $7`, owner, namespace, hiddenRoots, root.Row.ID, recursive, selectedPath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanBrowseRecord(rows)
		if err != nil {
			return nil, err
		}
		if len(items) >= limit {
			return nil, fc.NewProblem(503, "snapshot_too_large", "The complete snapshot exceeds the supported entry limit.")
		}
		entry, err := IndexedMetadata(namespace, *record)
		if err != nil {
			return nil, err
		}
		items = append(items, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
