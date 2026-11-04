package nodes

import (
	"context"
	"fmt"
	"time"

	"everythingabout.me/backend/internal/filechanges"
	"everythingabout.me/backend/internal/fileops"
	"github.com/jackc/pgx/v5"
)

type scanChangeCapture struct {
	path   string
	before capturedFileState
}

// settleScanChanges records observed index transitions, not an attributed native
// operation. All index writes finish before taking the owner-head lock. Bounded
// after-state reads then retain committed ancestor snapshots without native calls
// or acquiring additional index write locks.
func settleScanChanges(ctx context.Context, tx pgx.Tx, root fileops.Root, captures []scanChangeCapture) error {
	if len(captures) == 0 {
		return nil
	}
	if _, err := filechanges.EnsureHead(ctx, tx, root.OwnerID); err != nil {
		return err
	}
	var lockedHead int64
	if err := tx.QueryRow(ctx, `select head_position from file_change_heads where owner_id=$1::uuid for update`, root.OwnerID).Scan(&lockedHead); err != nil {
		return err
	}
	type pending struct {
		change filechanges.Change
		ids    []string
	}
	items := make([]pending, 0, len(captures))
	at := time.Now().UTC().Truncate(time.Microsecond)
	var previous *time.Time
	if err := tx.QueryRow(ctx, `select (select last_changed_at from file_change_markers
 where owner_id=$1::uuid and entry_id=(select id from file_identity_roots where owner_id=$1::uuid))`, root.OwnerID).Scan(&previous); err != nil {
		return err
	}
	if previous != nil && previous.After(at) {
		at = previous.UTC()
	}
	scanID := changeID()
	for _, capture := range captures {
		after, err := captureFileState(ctx, tx, root.OwnerID, fileRootNamespace(root), capture.path)
		if err != nil {
			return err
		}
		if after.metadata == nil {
			return fmt.Errorf("scanned entry %q is missing after import", capture.path)
		}
		before := capture.before
		kind := "updated"
		if before.metadata == nil {
			kind = "created"
			before.ancestors = nil
		} else if before.metadata.ID != after.metadata.ID {
			return fmt.Errorf("scanned entry %q changed identity during import", capture.path)
		}
		id := changeID()
		entryID := after.metadata.ID
		after.metadata.ChangeToken = id
		after.metadata.LastChangedAt = &at
		ids := []string{entryID}
		for _, ancestor := range before.ancestors {
			ids = append(ids, ancestor.ID)
		}
		for _, ancestor := range after.ancestors {
			ids = append(ids, ancestor.ID)
		}
		change := filechanges.Change{
			ID: id, OwnerID: root.OwnerID, EffectKey: "scan:" + scanID + ":" + entryID,
			Kind: kind, Coverage: "entry", EntryID: &entryID,
			Before: before.metadata, After: after.metadata,
			BeforeAncestors: before.ancestors, AfterAncestors: after.ancestors,
			Actor: filechanges.Actor{Kind: "external"}, Origin: "external", ObservedAt: at,
		}
		seenFolders := map[string]bool{}
		for _, ancestor := range append(append([]filechanges.Ancestor{}, before.ancestors...), after.ancestors...) {
			if seenFolders[ancestor.ID] {
				continue
			}
			seenFolders[ancestor.ID] = true
			var record *FileBrowseRecord
			if ancestor.Path.Relative() == "" {
				// fileBrowseMetadata's root convenience path can create a root.
				// Settlement already has one and must remain read-only here.
				record = &FileBrowseRecord{Row: Row{Node: Node{IsDir: true}}}
				err := tx.QueryRow(ctx, `select id::text,array['membership:'||membership_revision::text]
 from file_identity_roots where owner_id=$1::uuid`, root.OwnerID).Scan(&record.Row.ID, &record.Membership)
				if err == pgx.ErrNoRows {
					continue
				}
				if err != nil {
					return err
				}
			} else {
				var err error
				record, err = fileBrowseMetadata(ctx, tx, root.OwnerID, fileRootNamespace(root), ancestor.Path.Relative(), nil)
				if err != nil {
					return err
				}
			}
			if record == nil || record.Row.ID != ancestor.ID || !record.Row.IsDir {
				continue
			}
			metadata, err := IndexedMetadata(fileRootNamespace(root), *record)
			if err != nil {
				return err
			}
			metadata.LastChangedAt = &at
			metadata.ChangeToken = id
			change.AffectedFolders = append(change.AffectedFolders, metadata)
		}
		items = append(items, pending{change: change, ids: ids})
	}
	for _, item := range items {
		position, err := filechanges.Append(ctx, tx, item.change)
		if err != nil {
			return err
		}
		if err := filechanges.MarkChangedWithToken(ctx, tx, root.OwnerID, item.ids, position, at, item.change.ID); err != nil {
			return err
		}
	}
	return nil
}
