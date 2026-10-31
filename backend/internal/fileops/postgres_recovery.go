package fileops

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// ResolveNotApplied accepts trusted driver evidence that every final request is
// terminal and could not apply later. The service must Inspect and Discard first.
// A lease timeout or missing index entry can never supply this proof.
func (p *Postgres) ResolveNotApplied(ctx context.Context, l Lease, state State, evidence []byte, problem json.RawMessage) error {
	if state != Failed && state != Cancelled || len(evidence) == 0 {
		return E("invalid_request", nil)
	}
	if len(problem) > 0 {
		var err error
		problem, err = canonicalJSON(problem)
		if err != nil {
			return err
		}
	}
	return p.leased(ctx, l, func(tx pgx.Tx, op *Operation, now time.Time) error {
		if op.Terminal() || op.Phase != EffectStarted || op.NativeResult != nil || uncertainEntries(op) || anyApplied(op) {
			return E("recovery_required", nil)
		}
		if err := appendCheckpoint(ctx, tx, op, nil, evidence); err != nil {
			return err
		}
		var logical, physical int64
		err := tx.QueryRow(ctx, `select logical_reserved,physical_reserved from file_reservations where operation_id=$1 for update`, op.ID).Scan(&logical, &physical)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_budgets set logical_reserved=logical_reserved-$2,physical_reserved=physical_reserved-$3 where owner_id=$1`, op.Root.OwnerID, logical, physical)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update file_reservations set logical_reserved=0,physical_reserved=0,settled=true where operation_id=$1`, op.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `delete from file_credit_claims where operation_id=$1 and owner_id=$2`, op.ID, op.Root.OwnerID)
		if err != nil {
			return err
		}
		op.RecoveryEvidence = append([]byte(nil), evidence...)
		op.Problem = problem
		finish(op, state, now)
		return releaseGuards(ctx, tx, op)
	})
}
