package nodes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

// nodesTestDB applies every migration twice to a disposable database, so the
// suites run against the same schema production does, then drops it.
func nodesTestDB(t *testing.T) (*store.DB, context.Context) {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_nodes_")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("migrations unavailable", err)
	}
	for range 2 {
		for _, migration := range migrations {
			data, err := os.ReadFile(migration)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Pool().Exec(ctx, string(data)); err != nil {
				t.Fatalf("migration %s: %v", filepath.Base(migration), err)
			}
		}
	}
	return db, ctx
}
