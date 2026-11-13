package fileops

import (
	"context"
	"os"
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/storage"
)

// TestComparisonShimFindsLegacyKeysDuringBackfill covers the deploy window: the
// old binary writes the pre-backfill comparison_path while `migrate` has already
// rewritten the existing rows, so a read must still find that row. The read uses
// the other spelling of the same sibling, because the legacy key is not a
// function of the canonical key; only matching the stored value covers the whole
// equivalence class. The test then runs the real sql/052 over the row and checks
// that the backfill rewrote it and the same read still finds it.
func TestComparisonShimFindsLegacyKeysDuringBackfill(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	r := testRoot()
	r.OwnerID = newID()
	r.CaseSensitive = false
	a := testActor(r)
	if _, err := pool.Exec(ctx, `insert into users(id,email,password_hash) values($1,$2,'test-only')`, r.OwnerID, r.OwnerID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	p := NewPostgres(pool)
	if err := p.Initialize(ctx, r, provenBaseline(0, 0, 1000, 10000)); err != nil {
		t.Fatal(err)
	}

	// "ſ̂" is one name with "Ŝ" under the canonical key, but the legacy key keeps
	// them apart and is not a function of the canonical key: the old binary stores
	// the folded spelling of the name the client used, and a request may use the
	// other spelling of the same sibling.
	written := storage.Path("folder/\u017f\u0302")
	readAs := storage.Path("folder/\u015c")
	legacy := nameprobe.Key(nameprobe.LegacyPipeline, false)(string(written))
	current := nameprobe.Key(nameprobe.ReferencePipeline, false)(string(written))
	if legacy == current || nameprobe.Key(nameprobe.ReferencePipeline, false)(string(readAs)) != current {
		t.Fatalf("fixture spellings do not share one canonical key: written %q/%q read %q", legacy, current, readAs)
	}
	u := mustUpload(t, p, r, a, written, 4, "legacy-shim")
	if _, err := pool.Exec(ctx, `update file_uploads set comparison_path=$2 where operation_id=$1`, u.ID, legacy); err != nil {
		t.Fatal(err)
	}

	// The listing filters uploads by the actor's scope, exactly the read that must
	// not lose an upload session during the deploy window.
	scoped := a
	scoped.Scope = readAs
	found := func() bool {
		t.Helper()
		list, err := p.ListUploads(ctx, r, scoped, 10, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, listed := range list {
			if listed.ID == u.ID {
				return true
			}
		}
		return false
	}
	if !found() {
		t.Fatalf("legacy-key upload %s was not found through the shim", u.ID)
	}

	backfill, err := os.ReadFile("../../../sql/052_name_comparison_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(backfill)); err != nil {
		t.Fatalf("run backfill: %v", err)
	}
	var stored string
	if err := pool.QueryRow(ctx, `select comparison_path from file_uploads where operation_id=$1`, u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != current {
		t.Fatalf("backfill left comparison_path=%q, want canonical %q", stored, current)
	}
	if !found() {
		t.Fatalf("canonical-key upload %s was not found after the backfill", u.ID)
	}
}
