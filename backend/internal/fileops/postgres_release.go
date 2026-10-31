package fileops

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Release hands off worker ownership without changing native certainty, guards,
// quota or operation state. The same fence may release an already expired lease;
// an older epoch/token cannot release its successor's lease.
func (p *Postgres) Release(ctx context.Context, l Lease) error {
	if !validID(l.OperationID) || l.Token == "" || l.Epoch < 1 {
		return E("lease_lost", nil)
	}
	var owner string
	err := p.pool.QueryRow(ctx, `select owner_id::text from file_operations where id=$1`, l.OperationID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return E("not_found", nil)
	}
	if err != nil {
		return err
	}
	return p.transaction(ctx, owner, func(tx pgx.Tx) error {
		op, err := readOperation(ctx, tx, l.OperationID)
		if err != nil {
			return err
		}
		if op.Lease.Token != l.Token || op.Lease.Epoch != l.Epoch {
			return E("lease_lost", nil)
		}
		if op.Terminal() {
			return nil
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		op.Lease.ExpiresAt = now
		return saveOperation(ctx, tx, op)
	})
}
