package fileops

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// CleanupCandidates exposes terminal recorded temporary identities only. It never
// treats index absence as evidence or enumerates content paths for garbage collection.
// Only upload_commit has a registered staged cleanup worker in P3; generic entry
// allocation cleanup must register its own known-identity handler in P4.
func (p *Postgres) CleanupCandidates(ctx context.Context, limit int) ([]Operation, error) {
	if limit < 1 || limit > 1000 {
		return nil, E("invalid_request", nil)
	}
	// Scheduling attempts rotate durably even when an active late body makes
	// Discard busy. Selecting only the oldest finished rows would starve all later
	// owners behind one batch of stalled receivers. This mark releases no liability.
	rows, err := p.pool.Query(ctx, `with candidates as (
 select id from file_operations
 where action='upload_commit' and snapshot->>'State' in ('succeeded','failed','cancelled')
 and (snapshot->'CleanedAt' is null or snapshot->'CleanedAt'='null'::jsonb)
 order by coalesce((snapshot->>'CleanupAttemptedAt')::timestamptz,(snapshot->>'FinishedAt')::timestamptz),id
 for update skip locked limit $1
 ) update file_operations o
 set snapshot=jsonb_set(o.snapshot,'{CleanupAttemptedAt}',to_jsonb(clock_timestamp()))
 from candidates c where o.id=c.id returning o.snapshot`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Operation
	for rows.Next() {
		var b []byte
		var op Operation
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &op); err != nil {
			return nil, err
		}
		if err = canonicalOperation(&op); err != nil {
			return nil, err
		}
		result = append(result, op)
	}
	return result, rows.Err()
}

// MarkCleaned is called only after service-side Discard proves temporary allocation
// cleanup. It releases retained physical reservation, never charged native usage or
// logical liability, and preserves all journal/checkpoint/result provenance.
func (p *Postgres) MarkCleaned(ctx context.Context, root Root, a Actor, id string) error {
	if err := validateRoot(root); err != nil {
		return err
	}
	if !validID(id) {
		return E("not_found", nil)
	}
	return p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		op, err := readOperation(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = authorizeOperation(op, root, a, true); err != nil {
			return err
		}
		if !op.Terminal() || uncertainEntries(&op) {
			return E("recovery_required", nil)
		}
		if op.CleanedAt != nil {
			return nil
		}
		var logical, physical int64
		err = tx.QueryRow(ctx, `select logical_reserved,physical_reserved from file_reservations where operation_id=$1 for update`, op.ID).Scan(&logical, &physical)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_budgets set physical_reserved=physical_reserved-$2 where owner_id=$1`, root.OwnerID, physical)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_reservations set physical_reserved=0 where operation_id=$1`, op.ID)
		if err != nil {
			return err
		}
		if logical == 0 {
			if err = releaseGuards(ctx, tx, &op); err != nil {
				return err
			}
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		op.CleanedAt = &now
		return saveOperation(ctx, tx, op)
	})
}
