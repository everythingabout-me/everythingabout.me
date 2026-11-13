package fileops

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
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
