package filechanges

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"github.com/jackc/pgx/v5"
)

const changeColumns = `c.id::text,c.owner_id::text,c.position,c.effect_key,c.kind,c.coverage,c.entry_id::text,
 c.before_snapshot,c.after_snapshot,c.before_ancestors,c.after_ancestors,c.impact_paths,c.affected_folders,c.actor,
 c.operation_id,c.request_id,c.origin,c.observed_at,c.recorded_at,c.expires_at`

// Read is an internal owner window for trusted consumers. Public feeds must use
// ReadScoped and Project: raw rows contain concealed endpoints and actor data.
func Read(ctx context.Context, q Queryer, owner string, after, through int64, limit int) ([]Change, error) {
	if !validWindow(owner, after, through, limit) {
		return nil, ErrInvalidChange
	}
	rows, err := q.Query(ctx, `select `+changeColumns+` from file_changes c
 where c.owner_id=$1::uuid and c.position>$2 and c.position<=$3 order by c.position limit $4`, owner, after, through, limit)
	if err != nil {
		return nil, err
	}
	return readChanges(rows)
}

// ReadScoped filters native-aware visible candidates BEFORE LIMIT, including
// retained delete/move endpoints. Hidden account activity cannot create sparse
// empty pages or public scan-progress counts. limit may be 1001 for lookahead.
// The caller authorizes the scope and binds epoch/policy/window in opaque cursors.
// Match rules and Project must use the same admitted case sensitivity.
func ReadScoped(ctx context.Context, q Queryer, owner string, scope fc.Path, caseSensitive bool, after, through int64, limit int) ([]Change, error) {
	if !validWindow(owner, after, through, limit) || fc.ValidatePath(string(scope)) != nil {
		return nil, ErrInvalidChange
	}
	version := "nfc-sensitive-v1"
	if !caseSensitive {
		version = "nfc-fold-v1"
	}
	rows, err := q.Query(ctx, `select `+changeColumns+` from file_changes c
 where c.owner_id=$1::uuid and c.position>$2 and c.position<=$3 and (
   coalesce(file_changes_within($5,c.before_snapshot->>'path',$6),false)
   or coalesce(file_changes_within($5,c.after_snapshot->>'path',$6),false)
   or (c.coverage='subtree' and (
     coalesce(file_changes_within(c.before_snapshot->>'path',$5,$6),false)
     or coalesce(file_changes_within(c.after_snapshot->>'path',$5,$6),false)))
   or (c.kind='invalidated' and exists(select 1 from jsonb_array_elements_text(c.impact_paths) affected(path)
     where file_changes_within($5,affected.path,$6)
       or (c.coverage='subtree' and file_changes_within(affected.path,$5,$6))))
 ) order by c.position limit $4`, owner, after, through, limit, string(scope), version)
	if err != nil {
		return nil, err
	}
	return readChanges(rows)
}

func validWindow(owner string, after, through int64, limit int) bool {
	return validUUID(owner) && after >= 0 && through >= after && limit >= 1 && limit <= 1001
}

func readChanges(rows pgx.Rows) ([]Change, error) {
	defer rows.Close()
	items := []Change{}
	for rows.Next() {
		var c Change
		var before, after, ancestorsBefore, ancestorsAfter, impacts, folders, actor []byte
		if err := rows.Scan(&c.ID, &c.OwnerID, &c.Position, &c.EffectKey, &c.Kind, &c.Coverage, &c.EntryID,
			&before, &after, &ancestorsBefore, &ancestorsAfter, &impacts, &folders, &actor, &c.OperationID, &c.RequestID, &c.Origin,
			&c.ObservedAt, &c.RecordedAt, &c.ExpiresAt); err != nil {
			return nil, err
		}
		if len(before) != 0 {
			if err := json.Unmarshal(before, &c.Before); err != nil {
				return nil, err
			}
		}
		if len(after) != 0 {
			if err := json.Unmarshal(after, &c.After); err != nil {
				return nil, err
			}
		}
		for _, pair := range []struct {
			data   []byte
			target any
		}{
			{ancestorsBefore, &c.BeforeAncestors}, {ancestorsAfter, &c.AfterAncestors}, {impacts, &c.ImpactPaths}, {folders, &c.AffectedFolders}, {actor, &c.Actor},
		} {
			if err := json.Unmarshal(pair.data, pair.target); err != nil {
				return nil, fmt.Errorf("decode file change: %w", err)
			}
		}
		c.ObservedAt = c.ObservedAt.UTC()
		c.RecordedAt = c.RecordedAt.UTC()
		c.ExpiresAt = c.ExpiresAt.UTC()
		items = append(items, c)
	}
	return items, rows.Err()
}

// PruneExpired removes only an expired contiguous prefix under the same owner
// head lock. Durable effect receipts are retained. It is an explicit maintenance
// primitive; no request starts a background purge or silently resets an epoch.
func PruneExpired(ctx context.Context, tx pgx.Tx, owner string, now time.Time) (int64, error) {
	if tx == nil || !validUUID(owner) || now.IsZero() {
		return 0, ErrInvalidChange
	}
	if _, err := EnsureHead(ctx, tx, owner); err != nil {
		return 0, err
	}
	var head, previous int64
	if err := tx.QueryRow(ctx, `select head_position,retained_after from file_change_heads where owner_id=$1::uuid for update`, owner).Scan(&head, &previous); err != nil {
		return 0, err
	}
	var floor int64
	if err := tx.QueryRow(ctx, `select coalesce(min(position)-1,$3::bigint) from file_changes where owner_id=$1::uuid and expires_at>$2`, owner, now.UTC(), head).Scan(&floor); err != nil {
		return 0, err
	}
	if floor <= previous {
		return 0, nil
	}
	var discarded *time.Time
	if err := tx.QueryRow(ctx, `select max(recorded_at) from file_changes where owner_id=$1::uuid and position<=$2`, owner, floor).Scan(&discarded); err != nil {
		return 0, err
	}
	result, err := tx.Exec(ctx, `delete from file_changes where owner_id=$1::uuid and position<=$2`, owner, floor)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `update file_change_heads set retained_after=$2,retained_since=greatest(retained_since,coalesce($3::timestamptz,retained_since)),updated_at=clock_timestamp() where owner_id=$1::uuid`, owner, floor, discarded); err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}
