package fileops

import (
	"context"
	"encoding/json"
	"time"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// AcceptPrefixDelete makes a recursive deletion true before the operation answers: the
// index loses the subtree, the change feed records it, the quota is refunded and the
// guard that fenced the acceptance goes away with the rows. What remains is a byte
// purge that can take minutes without the user being able to notice, because nothing a
// read can reach still points at it.
//
// This is deliberately one transaction: if it commits, the client's `202` means the
// deletion happened, not that it was scheduled. If it fails, nothing was deleted and
// the caller answers with the error.
func (p *Postgres) AcceptPrefixDelete(ctx context.Context, l Lease, path storage.Path, commit PrefixDeleteCommit) (Operation, error) {
	var out Operation
	if commit == nil || storage.ValidatePath(path) != nil || path == "" {
		return out, E("invalid_request", nil)
	}
	err := p.leased(ctx, l, func(tx pgx.Tx, op *Operation, now time.Time) error {
		if op.Terminal() || op.Kind != "delete" || op.Purge != nil || len(op.Entries) > 0 || len(op.Checkpoint) > 0 {
			return E("invalid_transition", nil)
		}
		var request ActionRequest
		if json.Unmarshal(op.Intent, &request) != nil || request.From != path {
			return E("invalid_intent", nil)
		}
		removed, err := commit(ctx, tx, *op)
		if err != nil {
			return err
		}
		if removed < 0 {
			return E("invalid_native_result", nil)
		}
		u, err := budget(ctx, tx, op.Root)
		if err != nil {
			return err
		}
		// A refund subtracts what the rows held. PhysicalDelta is the effect of replacing
		// old bytes with new ones, so removing bytes adds its (negative) value.
		logical, physical := u.LogicalBytes-removed, u.PhysicalBytes+PhysicalDelta(op.Root, 0, removed)
		// A budget that never charged a row still has to accept its removal: a refund is
		// not a reservation, so the floor is zero rather than an error.
		logical, physical = max(logical, 0), max(physical, 0)
		if _, err = tx.Exec(ctx, `update file_budgets set logical_bytes=$2,physical_bytes=$3 where owner_id=$1`, op.Root.OwnerID, logical, physical); err != nil {
			return err
		}
		op.Purge = &PurgeState{Path: path, AcceptedAt: now}
		op.Phase = NativeApplied
		op.State = Running
		// No Result here: the accepted deletion is final for every read, but the operation
		// has not answered yet — the purge has. Publishing `{deleted:true}` at acceptance
		// put a finished-looking payload next to `state:"running"` for the whole purge.
		//
		// The subtree is gone from the index, so the guard has nothing left to protect and
		// would only keep the namespace closed while the purge runs. A created-under path
		// is an ordinary create again; the purge's live-row test keeps it.
		if err = releaseGuards(ctx, tx, op); err != nil {
			return err
		}
		out = *op
		return nil
	})
	return out, err
}

// SavePurgeProgress records how far the byte purge got, so a restarted worker and a
// waiting client both see progress instead of a counter that starts over. The counters
// are cumulative and only move forward. `total` is the extent of the walk that is running
// now, so it may shrink between walks — an attempt that dies after deleting objects
// leaves the next one a smaller tree to count — but never below what is already settled.
func (p *Postgres) SavePurgeProgress(ctx context.Context, l Lease, deleted, adopted, total int64) (Operation, error) {
	var out Operation
	if deleted < 0 || adopted < 0 || total < 0 || total < deleted+adopted {
		return out, E("invalid_request", nil)
	}
	err := p.leased(ctx, l, func(_ pgx.Tx, op *Operation, _ time.Time) error {
		if op.Terminal() || op.Purge == nil {
			return E("invalid_transition", nil)
		}
		op.Purge.Deleted, op.Purge.Adopted, op.Purge.Total = deleted, adopted, total
		out = *op
		return nil
	})
	return out, err
}

// FinishPurge ends a prefix deletion. The index settlement already happened at
// acceptance, so there is no reservation to settle and no entry to replay: this only
// records how far the native purge got and whether it finished.
func (p *Postgres) FinishPurge(ctx context.Context, l Lease, state State, result, problem json.RawMessage) (Operation, error) {
	var out Operation
	if state != Succeeded && state != Failed && state != Cancelled {
		return out, E("invalid_request", nil)
	}
	var err error
	if len(result) > 0 {
		if result, err = canonicalJSON(result); err != nil {
			return out, err
		}
	}
	if len(problem) > 0 {
		if problem, err = canonicalJSON(problem); err != nil {
			return out, err
		}
	}
	err = p.leased(ctx, l, func(tx pgx.Tx, op *Operation, now time.Time) error {
		if op.Terminal() || op.Purge == nil {
			return E("invalid_transition", nil)
		}
		if len(result) > 0 {
			op.Result = result
		}
		op.Problem = problem
		finish(op, state, now)
		if err := releaseGuards(ctx, tx, op); err != nil {
			return err
		}
		out = *op
		return nil
	})
	return out, err
}
