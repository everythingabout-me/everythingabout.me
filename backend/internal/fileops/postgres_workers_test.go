package fileops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// A refused batch size is a programming error that surfaces in a background loop, so the
// refusal itself has to name the scan, the size asked for and the range accepted. The
// limits are checked before the pool is touched, which is why no database is needed.
func TestScanLimitsExplainTheirRefusal(t *testing.T) {
	ctx := context.Background()
	p := &Postgres{}
	visitNone := func(Operation) bool { t.Fatal("a refused scan visited an operation"); return false }
	for _, tc := range []struct {
		name string
		call func() error
		want string
	}{
		{"pending above the cap", func() error { _, err := p.Pending(ctx, PendingPageMax+1); return err }, fmt.Sprintf("pending scan limit %d is outside 1..%d", PendingPageMax+1, PendingPageMax)},
		{"pending zero", func() error { _, err := p.Pending(ctx, 0); return err }, fmt.Sprintf("pending scan limit 0 is outside 1..%d", PendingPageMax)},
		{"paged walk above the cap", func() error { return p.EachPending(ctx, PendingPageMax+1, visitNone) }, fmt.Sprintf("pending scan limit %d is outside 1..%d", PendingPageMax+1, PendingPageMax)},
		{"paged walk zero", func() error { return p.EachPending(ctx, 0, visitNone) }, fmt.Sprintf("pending scan limit 0 is outside 1..%d", PendingPageMax)},
		{"cleanup", func() error { _, err := p.CleanupCandidates(ctx, 1001); return err }, "cleanup scan limit 1001 is outside 1..1000"},
		{"upload expiry", func() error { _, err := p.ExpireUploads(ctx, -1); return err }, "upload expiry scan limit -1 is outside 1..1000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			requireCode(t, err, "invalid_request")
			cause := errors.Unwrap(err)
			if cause == nil || cause.Error() != tc.want {
				t.Fatalf("refusal does not explain itself: cause %v, want %q", cause, tc.want)
			}
		})
	}
}

func TestPostgresPendingWalksTheWholeQueueInPages(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 1000, 1000))
	// Four operations share one creation instant so the walk has to continue inside a tie,
	// where only the id orders them; the rest are spread over distinct instants.
	const total = 7
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < total; i++ {
		op := mustBegin(t, p, genericRequest(r, a, storage.Path(fmt.Sprintf("queue/%d", i)), 1))
		created := base
		if i >= 4 {
			created = base.Add(time.Duration(i) * time.Second)
		}
		// One typed parameter feeds both places: with a second `$2::text` use PostgreSQL
		// inferred $2 as timestamptz anyway and wrote its own text rendering into the
		// document, which is not the RFC 3339 form Operation.CreatedAt unmarshals.
		if _, err := pool.Exec(ctx, `update file_operations set created_at=$2::timestamptz, snapshot=jsonb_set(snapshot,'{CreatedAt}',to_jsonb($2::timestamptz)) where id=$1`, op.ID, created); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(ops []Operation) []string {
		out := make([]string, len(ops))
		for i, op := range ops {
			out[i] = op.ID
		}
		return out
	}
	walk := func(page, stopAfter int) []string {
		t.Helper()
		var got []string
		err := p.EachPending(ctx, page, func(op Operation) bool {
			got = append(got, op.ID)
			return len(got) != stopAfter
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	whole, err := p.Pending(ctx, PendingPageMax)
	if err != nil {
		t.Fatal(err)
	}
	want := ids(whole)
	if len(want) != total {
		t.Fatalf("fixture queue has %d pending operations, want %d", len(want), total)
	}
	first, err := p.Pending(ctx, 3)
	if err != nil || !slices.Equal(ids(first), want[:3]) {
		t.Fatalf("first page %v, want %v: %v", ids(first), want[:3], err)
	}
	// 3 crosses the tie and ends on a short page; 7 ends on an empty page after a full one;
	// 1 makes every row a page boundary.
	for _, page := range []int{1, 3, total, total + 1} {
		if got := walk(page, 0); !slices.Equal(got, want) {
			t.Fatalf("walk with page %d visited %v, want every operation once in order %v", page, got, want)
		}
	}
	if got := walk(3, 4); !slices.Equal(got, want[:4]) {
		t.Fatalf("walk stopped by its visitor visited %v, want %v", got, want[:4])
	}
	// A leased operation leaves the queue without moving the rows behind it out of reach.
	leased := mustClaim(t, p, whole[2], a)
	remaining := slices.Delete(slices.Clone(want), 2, 3)
	if got := walk(2, 0); !slices.Equal(got, remaining) {
		t.Fatalf("walk past leased %s visited %v, want %v", leased.ID, got, remaining)
	}
}

// The doctor report is only useful if it describes the nonterminal queue and nothing
// else: a terminal row must not inflate a count, a cancel request must be visible, and
// the oldest row must be the one the operator would go read. The age is measured on the
// database clock, so the assertion allows for the seconds the fixture takes to run.
func TestPostgresOperationQueueCountsNonterminalWorkAndNamesTheOldest(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 1000, 1000))

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	oldest := mustBegin(t, p, genericRequest(r, a, storage.Path("queue/oldest"), 1))
	running := mustBegin(t, p, genericRequest(r, a, storage.Path("queue/running"), 1))
	unknown := mustBegin(t, p, genericRequest(r, a, storage.Path("queue/unknown"), 1))
	terminal := mustBegin(t, p, genericRequest(r, a, storage.Path("queue/terminal"), 1))
	for _, row := range []struct {
		op Operation
		at time.Time
	}{
		{oldest, base},
		{running, base.Add(time.Hour)},
		{unknown, base.Add(90 * time.Minute)},
		{terminal, base.Add(100 * time.Minute)},
	} {
		// One timestamp feeds both the promoted column and the document, as a transition
		// would leave them; a row the two disagree on is repaired on its next write.
		if _, err := pool.Exec(ctx, `update file_operations set created_at=$2::timestamptz, snapshot=jsonb_set(snapshot,'{CreatedAt}',to_jsonb($2::timestamptz)) where id=$1`, row.op.ID, row.at); err != nil {
			t.Fatal(err)
		}
	}
	mustClaim(t, p, running, a) // queued -> running
	if _, err := pool.Exec(ctx, `update file_operations set state='outcome_unknown' where id=$1`, unknown.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update file_operations set state='succeeded' where id=$1`, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RequestCancel(ctx, r, a, oldest.ID); err != nil {
		t.Fatal(err)
	}

	queue, err := p.OperationQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if queue.Queued != 1 || queue.Running != 1 || queue.OutcomeUnknown != 1 || queue.Total() != 3 {
		t.Fatalf("queue split queued/running/unknown/total = %d/%d/%d/%d, want 1/1/1/3",
			queue.Queued, queue.Running, queue.OutcomeUnknown, queue.Total())
	}
	if queue.CancelRequested != 1 {
		t.Fatalf("cancel-requested count %d, want 1", queue.CancelRequested)
	}
	if queue.Oldest == nil {
		t.Fatal("queue reports no oldest operation, want the queued operation created two hours ago")
	}
	if queue.Oldest.State != Queued {
		t.Fatalf("oldest state %q, want %q", queue.Oldest.State, Queued)
	}
	if queue.Oldest.OwnerID != r.OwnerID {
		t.Fatalf("oldest owner %q, want %q", queue.Oldest.OwnerID, r.OwnerID)
	}
	if !queue.Oldest.CreatedAt.Equal(base) {
		t.Fatalf("oldest created at %s, want %s", queue.Oldest.CreatedAt, base)
	}
	if age := queue.Oldest.Age; age < 2*time.Hour || age > 2*time.Hour+5*time.Minute {
		t.Fatalf("oldest age %s, want about 2h", age)
	}
}

// A database migrated to before sql/016 has no queue to report. That is a warning at
// doctor time, so the method must return the distinguishable error rather than a
// read failure that would fail the whole check.
func TestPostgresOperationQueueReportsAMissingTableAsUnavailable(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, _, _ := seedOwner(t, pool, provenBaseline(0, 0, 1000, 1000))
	if _, err := pool.Exec(ctx, `drop table file_operations cascade`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OperationQueue(ctx); !errors.Is(err, ErrOperationQueueUnavailable) {
		t.Fatalf("missing table read returned %v, want %v", err, ErrOperationQueueUnavailable)
	}
}
