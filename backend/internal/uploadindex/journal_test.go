package uploadindex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This test never loads .env or application configuration. An explicitly supplied
// disposable Postgres gets a private database, removed when the test completes.
func TestPostgresJournalConformance(t *testing.T) {
	url := os.Getenv("EAM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("EAM_TEST_DATABASE_URL unset; disposable journal integration not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	database := "eam_upload_watch_test_" + hex.EncodeToString(suffix[:])
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "drop database "+quoted+" with (force)"); err != nil {
			t.Error(err)
		}
	}()
	poolConfig, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.Database = database
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "create table users (id uuid primary key)"); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../sql/018_upload_index_watches.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("migration is not re-runnable: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, "insert into users (id) values ($1::uuid)", testOwner); err != nil {
		t.Fatal(err)
	}
	j := NewJournal(pool)
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	key := testKey()
	if err := j.Register(ctx, key, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	claim := func(at time.Time) []Watch {
		t.Helper()
		watches, err := j.Claim(ctx, key.Binding, at, at.Add(claimLease), batchLimit)
		if err != nil {
			t.Fatal(err)
		}
		return watches
	}
	one := func(at time.Time) Watch {
		t.Helper()
		watches := claim(at)
		if len(watches) != 1 {
			t.Fatalf("expected one claim, got %d", len(watches))
		}
		return watches[0]
	}
	read := func() (next time.Time, generation, lease int64, settled bool, fingerprint string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select next_check_at,generation,lease_version,settled,last_fingerprint
from upload_index_watches where binding=$1 and owner_id=$2::uuid and path=$3 and is_dir=$4`,
			key.Binding, key.OwnerID, key.Path, key.IsDir).Scan(&next, &generation, &lease, &settled, &fingerprint); err != nil {
			t.Fatal(err)
		}
		return
	}
	old := one(now)
	if watches := claim(now); len(watches) != 0 {
		t.Fatal("an active lease was concurrently claimed")
	}
	// An older process can finish after lease expiry and a second worker claim.
	reclaimed := one(now.Add(claimLease))
	if reclaimed.LeaseVersion <= old.LeaseVersion {
		t.Fatal("reclaim did not fence the old lease")
	}
	staleResult := Result{CheckedAt: now, NextCheck: now.Add(48 * time.Hour), Fingerprint: "stale"}
	if err := j.Finish(ctx, old, staleResult); err != nil {
		t.Fatal(err)
	}
	if next, _, _, _, fp := read(); !next.Equal(now) || fp != "" {
		t.Fatal("old lease acknowledgment overwrote reclaimed work")
	}

	// A sign renewal increments intent generation while preserving longest expiry.
	if err := j.Register(ctx, key, now.Add(3*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := j.Register(ctx, key, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, reclaimed, staleResult); err != nil {
		t.Fatal(err)
	}
	if next, gen, _, settled, fp := read(); !next.Equal(now) || gen != 3 || settled || fp != "" {
		t.Fatal("renewed watch was overwritten by stale generation")
	}
	renewed := one(now)
	if !renewed.IssuedUntil.Equal(now.Add(3 * time.Hour)) {
		t.Fatal("renewal shortened outstanding credential observation")
	}

	if err := j.Settle(ctx, key, now, now.Add(coldInterval)); err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, renewed, staleResult); err != nil {
		t.Fatal(err)
	}
	if next, _, _, settled, _ := read(); !settled || !next.Equal(now.Add(coldInterval)) {
		t.Fatal("stale acknowledgment undid explicit settlement")
	}
	if watches := claim(now); len(watches) != 0 {
		t.Fatal("settled watch stayed hot")
	}
	cold := one(now.Add(coldInterval))
	good := Result{CheckedAt: now.Add(coldInterval), NextCheck: now.Add(2 * coldInterval), Fingerprint: "actual-native-facts"}
	if err := j.Finish(ctx, cold, good); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, fp := read(); fp != good.Fingerprint {
		t.Fatal("cold observation did not retain actual native facts")
	}

	// Binding and marker kind are part of the key, not merely query filters.
	otherBinding := key
	otherBinding.Binding = "binding-b"
	if err := j.Register(ctx, otherBinding, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	folder := key
	folder.IsDir = true
	if err := j.Register(ctx, folder, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if watches := claim(now); len(watches) != 1 || !watches[0].IsDir || watches[0].Binding != key.Binding {
		t.Fatal("binding or folder/file isolation failed")
	}
	var count int
	if err := pool.QueryRow(ctx, "select count(*) from upload_index_watches").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected three independent retained watches, got %d", count)
	}
	if _, err := pool.Exec(ctx, "delete from users where id=$1::uuid", testOwner); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "select count(*) from upload_index_watches").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("owner deletion did not remove retained watches")
	}
}
