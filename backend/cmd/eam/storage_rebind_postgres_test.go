package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/filechanges"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The value comparison is covered without a database. This test proves the durable
// half: the real store updates file_budgets.root_binding and rotates
// file_change_heads in one transaction, and it records coordinates a pre-upgrade
// binding never had.
func TestStorageRebindPostgres(t *testing.T) {
	scratch := testpg.CreateDatabase(t, "eam_rebind_")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	applyMigrations(t, ctx, scratch.URL)
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	owner := "11111111-1111-1111-1111-111111111111"
	if _, err := db.Pool().Exec(ctx, `insert into users(id,email,password_hash) values($1::uuid,$2,'')`, owner, "rebind@example.test"); err != nil {
		t.Fatal(err)
	}
	old := binding(owner, "default", "bucket-old", "ap-hongkong", "drive/"+owner+"/", 1)
	seedBudget(t, ctx, db, owner, old)
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldHead, err := filechanges.BindRoot(ctx, tx, owner, old.namespace())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	moved := binding(owner, "default", "bucket-new", "ap-beijing", "drive/"+owner+"/", 2)
	source := newFakeBindingSource()
	source.roots[owner] = moved

	// The startup gate refuses before any bucket is touched.
	err = refuseStorageRoots(ctx, newRootStore(db), source)
	if err == nil {
		t.Fatal("a changed bucket was accepted")
	}
	for _, fragment := range []string{"bucket", "bucket-old", "bucket-new", "storage-rebind"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("refusal omits %q: %v", fragment, err)
		}
	}

	out, err := rebindRoot(ctx, newRootStore(db), source, rebindOptions{Owner: owner, From: "default", To: "default", WritersQuiesced: true})
	if err != nil {
		t.Fatalf("rebind: %v\n%s", err, out)
	}
	stored := readBudget(t, ctx, db, owner)
	if stored.Bucket != "bucket-new" || stored.Region != "ap-beijing" || stored.Generation != 2 {
		t.Fatalf("budget binding = %+v, want the configured root", stored)
	}
	var namespace, epoch string
	if err := db.Pool().QueryRow(ctx, `select root_namespace, root_epoch::text from file_change_heads where owner_id=$1::uuid`, owner).Scan(&namespace, &epoch); err != nil {
		t.Fatal(err)
	}
	if namespace != moved.namespace() {
		t.Fatalf("change namespace = %q, want %q", namespace, moved.namespace())
	}
	if epoch == oldHead.RootEpoch {
		t.Fatal("rebind did not rotate the change-stream epoch")
	}
	if !strings.Contains(out, "bucket") || !strings.Contains(out, "no bucket was listed") {
		t.Fatalf("output does not say what changed and what was untouched:\n%s", out)
	}

	// A matching legacy binding gains coordinates without a namespace rotation.
	legacy := moved
	legacy.Bucket, legacy.Region = "", ""
	seedBudget(t, ctx, db, owner, legacy)
	if err := refuseStorageRoots(ctx, newRootStore(db), source); err != nil {
		t.Fatalf("matching legacy binding refused: %v", err)
	}
	if stored := readBudget(t, ctx, db, owner); stored.Bucket != "bucket-new" || stored.Region != "ap-beijing" {
		t.Fatalf("coordinates were not backfilled: %+v", stored)
	}
}

func applyMigrations(t *testing.T, ctx context.Context, databaseURL string) {
	t.Helper()
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatalf("discover migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, path := range migrations {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(path), err)
		}
	}
}

func readBudget(t *testing.T, ctx context.Context, db *store.DB, owner string) rootBinding {
	t.Helper()
	var raw []byte
	if err := db.Pool().QueryRow(ctx, `select root_binding from file_budgets where owner_id=$1::uuid`, owner).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored rootBinding
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func seedBudget(t *testing.T, ctx context.Context, db *store.DB, owner string, binding rootBinding) {
	t.Helper()
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `insert into file_budgets(owner_id,root_binding,baseline,logical_bytes,physical_bytes,logical_limit,physical_limit)
 values($1::uuid,$2,'{}'::jsonb,0,0,0,0)
 on conflict (owner_id) do update set root_binding=excluded.root_binding`, owner, data); err != nil {
		t.Fatal(err)
	}
}
