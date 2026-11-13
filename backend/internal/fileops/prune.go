package fileops

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// pruneBatch bounds one prune transaction. A batch is read into memory whole (document plus
// promoted columns), so the bound is rows × document, the same budget the pending scan
// documents in PendingPageMax: ordinary documents measure 2–12 KB. It is not a limit on what
// one invocation may delete — the walk pages until the candidate queue is empty.
const pruneBatch = 200

// PruneResult reports what one invocation did. Repaired and Skipped count the same rows: a
// candidate whose columns disagreed with its document is repaired in place and kept this run,
// never deleted.
type PruneResult struct {
	Deleted  int64
	Repaired int64
	Skipped  int64
}

// PruneExpired deletes terminal operations whose retention window has passed, and nothing
// else. The rule, and the reason for each clause:
//
//   - state terminal and phase settled: a non-terminal operation may still land a native
//     effect, so it is never a candidate whatever its retain_until says.
//   - retain_until < now: RetainUntil is the window workers and clients were promised for
//     reading a settled operation back, and this command is its first reader.
//   - no guard row: a guard is what makes a terminal operation still fence a path. Settlement
//     releases guards, so a remaining row means something is still in flight.
//   - no change fact in the feed: a change row names the operation that produced it, and a
//     client replaying that cursor must still be able to read the operation. Facts expire on
//     their own (90 days), so this delays a prune; it does not leak rows.
//   - no credit claim: a deletion claim records that a native revision was removed and is
//     deliberately retained after the operation's guards are released (sql/017); deleting
//     it to free the operation would let the same revision be refunded twice. A
//     replacement claim is released at settlement (credit.go), so it never delays a
//     prune by itself; one that still points at a terminal replacement is a row written
//     before that rule, and keeping it here is what lets admission adopt it later.
//   - no live child session: an upload session in state 'open' may still commit, and a session
//     inside its own expires_at window is still the idempotent answer to a client retry.
//
// The document is checked per candidate as well, through divergence: the columns pick the
// candidate, the document confirms it. A disagreeing row is repaired from the document and
// kept, so the direction that could destroy work — the column says terminal while the
// operation still runs — can never be acted on.
//
// Each batch is one transaction under the owner write lock, the same lock every settlement
// takes, so a settle cannot race a delete. A crash or Ctrl-C rolls the current batch back and
// leaves earlier batches committed; re-running deletes only what still matches, which makes an
// interrupted run safe to repeat and never partial within one operation's child rows.
func (p *Postgres) PruneExpired(ctx context.Context, owner string, now time.Time, dryRun bool) (PruneResult, error) {
	var result PruneResult
	if !validID(owner) || now.IsZero() {
		return result, E("invalid_request", fmt.Errorf("prune requires a canonical owner UUID and a timestamp, got owner %q", owner))
	}
	cursor := pruneCursor{}
	for {
		last, read, err := p.pruneBatch(ctx, owner, now.UTC(), dryRun, cursor, &result)
		if err != nil {
			return result, err
		}
		if read < pruneBatch {
			return result, nil
		}
		cursor = last
	}
}

// pruneCursor pages by (retain_until, id), the keys the prunable index holds. A dry run
// deletes nothing, so without the cursor it would read the same first page forever; a real
// run uses it too so that a skipped candidate (repaired, or kept by one of the clauses above)
// is not re-examined by the same invocation.
type pruneCursor struct {
	retainUntil time.Time
	id          string
}

func (p *Postgres) pruneBatch(ctx context.Context, owner string, now time.Time, dryRun bool, after pruneCursor, result *PruneResult) (pruneCursor, int, error) {
	last := after
	var afterAt, afterID any
	if after.id != "" {
		afterAt, afterID = after.retainUntil, after.id
	}
	read := 0
	err := p.transaction(ctx, owner, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select id::text,snapshot,`+journalColumnList+`
			from file_operations o
			where o.owner_id=$1::uuid
			  and o.state in ('succeeded','failed','cancelled')
			  and o.phase='settled'
			  and o.retain_until < $2
			  and ($3::timestamptz is null or (o.retain_until,o.id) > ($3::timestamptz,$4::uuid))
			  and not exists (select 1 from file_operation_guards g where g.operation_id=o.id)
			  and not exists (select 1 from file_changes c where c.owner_id=o.owner_id and c.operation_id=o.id::text)
			  and not exists (select 1 from file_credit_claims cc where cc.operation_id=o.id)
			  and not exists (select 1 from file_uploads u where u.operation_id=o.id and (u.state='open' or u.expires_at > $2))
			order by o.retain_until,o.id
			for update of o skip locked
			limit $5`, owner, now, afterAt, afterID, pruneBatch)
		if err != nil {
			return err
		}
		type candidate struct {
			id      string
			op      Operation
			columns journalColumns
		}
		var candidates []candidate
		for rows.Next() {
			var id string
			var b []byte
			var columns journalColumns
			if err := rows.Scan(append([]any{&id, &b}, journalColumnTargets(&columns)...)...); err != nil {
				rows.Close()
				return err
			}
			read++
			last = pruneCursor{retainUntil: columns.RetainUntil, id: id}
			var op Operation
			if err := json.Unmarshal(b, &op); err != nil {
				rows.Close()
				return fmt.Errorf("operation %s: %w", id, err)
			}
			if err := canonicalOperation(&op); err != nil {
				rows.Close()
				return fmt.Errorf("operation %s: %w", id, err)
			}
			candidates = append(candidates, candidate{id: id, op: op, columns: columns})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// The loop above only reads. A repair writes, and pgx allows one statement at a time
		// on a connection: running it while the cursor is open fails with "conn busy", which
		// would make a disagreeing row abort the whole batch instead of being repaired.
		var ids []string
		for _, c := range candidates {
			if diverged := divergence(c.op, c.columns); len(diverged) > 0 {
				// The document is the operation; repair the columns and keep the row this run.
				if !dryRun {
					if err := saveOperation(ctx, tx, c.op); err != nil {
						return err
					}
				}
				result.Repaired++
				result.Skipped++
				continue
			}
			if !c.op.Terminal() || c.op.Phase != Settled || !c.op.RetainUntil.Before(now) {
				result.Skipped++
				continue
			}
			ids = append(ids, c.id)
		}
		if dryRun || len(ids) == 0 {
			if dryRun {
				result.Deleted += int64(len(ids))
			}
			return nil
		}
		// Child rows are pure derivation of the operation: a checkpoint only resumes work, a
		// binding/charge/reservation only settles it, and a terminal session with the row's own
		// retention passed is an acknowledgement nobody can still read. They go before the
		// parent because each references it. Guards, credit claims and change facts are not
		// touched: the candidate filter already excluded their operations.
		for _, statement := range []string{
			`delete from file_operation_checkpoints where operation_id = any($1::uuid[]) and owner_id=$2`,
			`delete from file_action_bindings where operation_id = any($1::uuid[]) and owner_id=$2`,
			`delete from file_reservations where operation_id = any($1::uuid[]) and owner_id=$2`,
			`delete from file_entry_charges where operation_id = any($1::uuid[]) and owner_id=$2`,
			`delete from file_upload_parts where operation_id = any($1::uuid[]) and owner_id=$2`,
			`delete from file_uploads where operation_id = any($1::uuid[]) and owner_id=$2`,
		} {
			if _, err := tx.Exec(ctx, statement, ids, owner); err != nil {
				return err
			}
		}
		// Re-check every clause that could make the row ineligible, so a future change to the
		// candidate query cannot turn the delete into a blind one.
		tag, err := tx.Exec(ctx, `delete from file_operations
			where id = any($1::uuid[]) and owner_id=$2
			  and state in ('succeeded','failed','cancelled') and phase='settled'
			  and retain_until < $3
			  and not exists (select 1 from file_operation_guards g where g.operation_id=file_operations.id)
			  and not exists (select 1 from file_changes c where c.owner_id=$2 and c.operation_id=file_operations.id::text)
			  and not exists (select 1 from file_credit_claims cc where cc.operation_id=file_operations.id)
			  and not exists (select 1 from file_uploads u where u.operation_id=file_operations.id and u.state='open')`,
			ids, owner, now)
		if err != nil {
			return err
		}
		result.Deleted += tag.RowsAffected()
		return nil
	})
	return last, read, err
}
