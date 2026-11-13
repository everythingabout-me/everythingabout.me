package nodes

import (
	"errors"
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/paths"
	"github.com/jackc/pgx/v5/pgconn"
)

// SQL is the reference implementation. Every corpus spelling must produce the
// documented key exactly, not merely the same equivalence classes.
func TestSQLComparisonMatchesReference(t *testing.T) {
	db, ctx := observationTestDB(t)
	for _, pair := range nameprobe.Pairs {
		for _, spelling := range []string{pair.A, pair.B} {
			var got string
			if err := db.Pool().QueryRow(ctx, `select file_identity_comparison($1,'nfc-fold-v1')`, spelling).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if want := nameprobe.Key(nameprobe.ReferencePipeline, false)(spelling); got != want {
				t.Errorf("SQL key(%q)=%q, want %q", spelling, got, want)
			}
		}
	}
}

// TestSQLIndexCollision proves the uniqueness index merges the four divergent
// pairs: inserting the second spelling as a sibling fails with 23505 even
// though the Go fence and token scope treat it as a different name.
func TestSQLIndexCollision(t *testing.T) {
	db, ctx := observationTestDB(t)
	pool := db.Pool()
	owner := actionOwner(t, pool)
	service := New(db, nil)
	if err := service.FileConfigureNameComparison(ctx, owner.OwnerID, false); err != nil {
		t.Fatal(err)
	}
	insert := func(name string) error {
		_, err := pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size) values($1::uuid,$2,$3,'',$4,false,1)`,
			owner.OwnerID, paths.KeyFor(name, owner.OwnerID), name, name)
		return err
	}
	for _, pair := range nameprobe.Pairs {
		if !nameprobe.Same(nameprobe.ReferencePipeline, false, pair.A, pair.B) {
			continue
		}
		if _, err := pool.Exec(ctx, `delete from nodes where owner_id=$1::uuid`, owner.OwnerID); err != nil {
			t.Fatal(err)
		}
		if err := insert(pair.A); err != nil {
			t.Fatalf("%s first insert: %v", pair.ID, err)
		}
		err := insert(pair.B)
		var problem *pgconn.PgError
		if !errors.As(err, &problem) || problem.Code != "23505" {
			t.Fatalf("%s second insert err=%v, want unique_violation", pair.ID, err)
		}
	}
}
