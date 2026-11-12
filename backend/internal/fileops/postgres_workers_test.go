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
		if _, err := pool.Exec(ctx, `update file_operations set snapshot=jsonb_set(snapshot,'{CreatedAt}',to_jsonb($2::text)) where id=$1`, op.ID, created.Format(time.RFC3339Nano)); err != nil {
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
