package fileops

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// RetireStalled ends an operation the caller has classified as unable to make progress,
// after the caller has decided that no live worker owns it any more.
//
// It is deliberately not FinishWithoutEffect. That transition refuses EffectStarted and
// NativeApplied, because a pre-effect failure may not describe a publication that has
// already landed; this transition exists for exactly the case where the effect is unknown,
// so it must not pretend to prove absence. It also does not inspect the phase, native
// result or entries: the caller classified the case, and requiring a proof of absence here
// would force every caller to claim one it does not have. What this method guarantees is
// narrower and honest: the operation becomes terminal, the path is fenced no longer, the
// reservation/credit are released, the upload session is marked aborted, and the state says
// the effect is unknown. It never records a success or settles index/quota.
//
// It refuses a live lease so a worker that is still running is never force-failed from
// outside; operation_in_progress is the answer the caller leaves for the next attempt.
// A terminal operation is returned unchanged, which makes a repeated retirement idempotent.
func (p *Postgres) RetireStalled(ctx context.Context, root Root, actor Actor, id string, state State, problem json.RawMessage) (Operation, error) {
	var out Operation
	if err := validateRoot(root); err != nil {
		return out, err
	}
	if !validID(id) {
		return out, E("not_found", nil)
	}
	if state != Failed && state != Unresolved {
		return out, E("invalid_request", nil)
	}
	if len(problem) > 0 {
		var err error
		problem, err = canonicalJSON(problem)
		if err != nil {
			return out, err
		}
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		op, _, err := readOperation(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = authorizeOperation(op, root, actor, true); err != nil {
			return err
		}
		if op.Terminal() {
			out = op
			return nil
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		// A live worker, not a failed attempt, owns the row: retirement would overwrite a
		// transition that may be in flight. The caller tries again on a later attempt.
		if op.Lease.Token != "" && op.Lease.ExpiresAt.After(now) {
			return E("operation_in_progress", nil)
		}
		var logical, physical int64
		err = tx.QueryRow(ctx, `select logical_reserved,physical_reserved from file_reservations where operation_id=$1 for update`, op.ID).Scan(&logical, &physical)
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
		if err = releaseGuards(ctx, tx, &op); err != nil {
			return err
		}
		op.Problem = problem
		finish(&op, state, now)
		if err = saveOperation(ctx, tx, op); err != nil {
			return err
		}
		// The session becomes a tombstone a new upload may replace. An open or committing
		// row would otherwise keep the destination's client key pointing at an operation
		// that can never publish.
		if _, err = tx.Exec(ctx, `update file_uploads set state='aborted' where operation_id=$1 and owner_id=$2 and state in ('open','committing')`, op.ID, op.Root.OwnerID); err != nil {
			return err
		}
		out = op
		return nil
	})
	return out, err
}

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
		uncertain, err := operationEntriesUncertain(ctx, tx, op)
		if err != nil {
			return err
		}
		applied, err := operationEntriesApplied(ctx, tx, op)
		if err != nil {
			return err
		}
		if op.Terminal() || op.Phase != EffectStarted || op.NativeResult != nil || uncertain || applied {
			return E("recovery_required", nil)
		}
		if err := appendCheckpoint(ctx, tx, op, nil, evidence); err != nil {
			return err
		}
		var logical, physical int64
		err = tx.QueryRow(ctx, `select logical_reserved,physical_reserved from file_reservations where operation_id=$1 for update`, op.ID).Scan(&logical, &physical)
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
