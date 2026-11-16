package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PendingPageMax is the largest page of pending work one query may return.
//
// A page is read into memory whole, so the bound is rows × snapshot. The snapshots of
// ordinary operations measure 2–12 KB of JSON (an upload commit about 5 KB), which makes
// 4096 rows roughly 20–50 MB decoded: an amount one scan may hold. It does not bound a
// page of large manifests — a single 100,000-entry delete outweighs this whole budget —
// and no row count can, which is one reason a caller should page well below the cap.
//
// It was 1000 with no reason written next to it. The scanner asked for one worker pool's
// worth of rows, so growing the pool from 32 to 1024 turned every scan into an
// invalid_request and deferred work stopped being resumed. The pool and this bound are
// unrelated: callers pick a page size and let EachPending walk the queue, and a caller
// whose page is a constant should assert it against this one at compile time.
// Revisit the number when the typical snapshot grows, not when a pool does.
const PendingPageMax = 4096

// scanLimit refuses a batch size outside a scan's bound and says which scan, what was
// asked and what is accepted. A bare invalid_request here reaches the log of a background
// loop that retries forever, where it reads like an outage instead of a wrong constant.
func scanLimit(scan string, limit, max int) error {
	if limit < 1 || limit > max {
		return E("invalid_request", fmt.Errorf("%s limit %d is outside 1..%d", scan, limit, max))
	}
	return nil
}

// pendingCursor is the last row of a page in the scan's own order: created_at and id, the
// same keys the pending index holds. created_at is a real timestamp now, so the cursor is
// the instant itself and the keyset comparison below is a range on an index rather than a
// textual comparison over a JSON string.
type pendingCursor struct {
	createdAt time.Time
	id        string
}

// Pending returns the oldest bounded page of durable work, never permission to repeat a
// native final effect. Callers must select the exact root and run recovery/authorization.
// A queue longer than one page is walked with EachPending.
func (p *Postgres) Pending(ctx context.Context, limit int) ([]Operation, error) {
	out, _, err := p.pendingPage(ctx, pendingCursor{}, limit)
	return out, err
}

// EachPending visits pending work oldest first, one page per query, until visit returns
// false or the queue is exhausted. No connection is held while visit runs.
//
// It pages by keyset rather than by repeating the first page because visiting a row does
// not remove it from the result: an operation the caller admits stays eligible until its
// worker claims a lease, and one waiting out a retry stays eligible throughout. Repeating
// the first page would return the same rows, and a head of operations that cannot be
// admitted would hide everything queued behind them.
//
// The walk is bounded by the queue it reads: every page moves the cursor strictly forward,
// and a row that becomes eligible behind the cursor waits for the next walk.
func (p *Postgres) EachPending(ctx context.Context, page int, visit func(Operation) bool) error {
	var after pendingCursor
	for {
		out, last, err := p.pendingPage(ctx, after, page)
		if err != nil {
			return err
		}
		for _, op := range out {
			if !visit(op) {
				return nil
			}
		}
		if len(out) < page {
			return nil
		}
		after = last
	}
}

func (p *Postgres) pendingPage(ctx context.Context, after pendingCursor, limit int) ([]Operation, pendingCursor, error) {
	if err := scanLimit("pending scan", limit, PendingPageMax); err != nil {
		return nil, after, err
	}
	// A null id means "from the start"; the first page has no row to continue after.
	var afterID, afterAt any
	if after.id != "" {
		afterID = after.id
		afterAt = after.createdAt
	}
	rows, err := p.pool.Query(ctx, `select snapshot,created_at,id::text from file_operations where state in ('queued','running','outcome_unknown')
 and not exists (select 1 from file_uploads u where u.operation_id=file_operations.id and u.state <> 'committing')
 and created_at < clock_timestamp()-interval '30 seconds'
 and (lease_token='' or (lease_expires_at is not null and lease_expires_at <= clock_timestamp()))
 and ($2::uuid is null or (created_at,id) > ($3::timestamptz,$2::uuid))
 order by created_at,id limit $1`, limit, afterID, afterAt)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	var out []Operation
	last := after
	for rows.Next() {
		var b []byte
		var op Operation
		if err = rows.Scan(&b, &last.createdAt, &last.id); err != nil {
			return nil, after, err
		}
		if err = json.Unmarshal(b, &op); err != nil {
			return nil, after, err
		}
		if err = canonicalOperation(&op); err != nil {
			return nil, after, err
		}
		out = append(out, op)
	}
	return out, last, rows.Err()
}

// ErrOperationQueueUnavailable says the database predates sql/016: there is no
// file_operations table and therefore no queue to report. It is a warning at doctor
// time, not a failure — a pre-migration database is a known state, and the fix is the
// command the message names — so callers can tell it apart from a real read error.
var ErrOperationQueueUnavailable = errors.New("file_operations table is missing; run `eam migrate` first")

// OperationQueue is a read-only snapshot of the nonterminal operation queue.
//
// Doctor reports it because the queue is where a stalled import becomes visible
// without a client asking about a specific operation: during a 23k-file import 129
// operations sat queued for about an hour and nothing surfaced it, so the operator
// found it only by querying the database. The threshold that decides what "stalled"
// means belongs to the operator (doctor's --max-operation-age), because only the
// operator knows what an ordinary import costs; this method reports the facts.
type OperationQueue struct {
	Queued          int
	Running         int
	OutcomeUnknown  int
	CancelRequested int
	// Oldest is nil when nothing is nonterminal.
	Oldest *OperationQueueOldest
}

// Total is every operation still waiting for settlement, in any nonterminal state.
func (q OperationQueue) Total() int { return q.Queued + q.Running + q.OutcomeUnknown }

// OperationQueueOldest identifies the operation that has been nonterminal longest.
// Age is measured on the database clock so it agrees with created_at even when the
// doctor host's clock does not.
type OperationQueueOldest struct {
	State     State
	OwnerID   string
	CreatedAt time.Time
	Age       time.Duration
}

// OperationQueue reads the nonterminal queue without touching terminal history.
//
// One query carries the partial index's own predicate, `state in
// ('queued','running','outcome_unknown')` (sql/047's file_operations_pending), and reads
// in that index's order, so neither a terminal row nor a sort is involved. The rows are
// streamed: the method holds one operation document at a time, and the work is
// proportional to the nonterminal queue, never to the retained journal.
//
// The cancel count is decoded in Go rather than written as a SQL predicate over
// `snapshot`. The journal-predicate rule (TestOperationJournalPredicatesReadColumns)
// forbids naming a document field in SQL because a Go field rename then silently matches
// nothing; `CancelRequested` has no promoted column to use instead, and adding one is a
// schema change outside this read-only report. The document is decoded one row at a time,
// so the cost is a parse per nonterminal operation, not a copy of the queue.
func (p *Postgres) OperationQueue(ctx context.Context) (OperationQueue, error) {
	var q OperationQueue
	if p == nil || p.pool == nil {
		return q, errors.New("operation queue unavailable: the database connection is not configured")
	}
	rows, err := p.pool.Query(ctx, `select id::text, state, owner_id::text, created_at,
		extract(epoch from clock_timestamp() - created_at)::double precision, snapshot
	from file_operations
	where state in ('queued','running','outcome_unknown')
	order by created_at, id`)
	if err != nil {
		return q, operationQueueError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, state, owner string
		var created time.Time
		var ageSeconds float64
		var document []byte
		if err := rows.Scan(&id, &state, &owner, &created, &ageSeconds, &document); err != nil {
			return q, operationQueueError(err)
		}
		switch State(state) {
		case Queued:
			q.Queued++
		case Running:
			q.Running++
		case Unknown:
			q.OutcomeUnknown++
		}
		var operation struct {
			CancelRequested bool
		}
		if err := json.Unmarshal(document, &operation); err != nil {
			return q, fmt.Errorf("read operation queue: operation %s has an undecodable snapshot: %w", id, err)
		}
		if operation.CancelRequested {
			q.CancelRequested++
		}
		if q.Oldest == nil {
			q.Oldest = &OperationQueueOldest{
				State:     State(state),
				OwnerID:   owner,
				CreatedAt: created,
				Age:       time.Duration(ageSeconds * float64(time.Second)),
			}
		}
	}
	if err := rows.Err(); err != nil {
		return q, operationQueueError(err)
	}
	return q, nil
}

// operationQueueError keeps "the table this reads does not exist yet" distinct from a
// read that failed, so doctor warns about the former and fails on the latter.
func operationQueueError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return ErrOperationQueueUnavailable
	}
	return fmt.Errorf("read operation queue: %w", err)
}
