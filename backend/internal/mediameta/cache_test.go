package mediameta

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cacheTestDatabase creates a scratch database on an explicitly supplied disposable
// server and applies every migration twice. The runner applies each file once now
// (schema_migrations), but the second pass keeps the DDL safe for the bootstrap boundary
// and for a database whose version table was lost: files from 044 on are re-executed in
// that case. No application config or environment file is loaded.
func cacheTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testpg.Require(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "eam_mediameta_" + hex.EncodeToString(random[:])
	if _, err = admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, "drop database "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
			t.Error(err)
		}
		admin.Close(ctx)
	})
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, migration := range migrations {
			data, err := os.ReadFile(migration)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, string(data)); err != nil {
				t.Fatalf("migration %s (pass %d): %v", migration, pass+1, err)
			}
		}
	}
	return pool
}

func cacheTestEntry(t *testing.T, pool *pgxpool.Pool) (owner, entry string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `insert into users(email,password_hash) values(gen_random_uuid()::text||'@example.test','test') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size) values($1::uuid,$2,'photo.jpg','','photo.jpg',false,3) returning id::text`, owner, "drive/"+owner+"/photo.jpg").Scan(&entry); err != nil {
		t.Fatal(err)
	}
	return owner, entry
}

func TestPostgresCacheIsKeyedByContentAndGoesAwayWithTheEntry(t *testing.T) {
	pool := cacheTestDatabase(t)
	ctx := context.Background()
	cache := NewPostgresCache(pool)
	owner, entry := cacheTestEntry(t, pool)
	other, _ := cacheTestEntry(t, pool)

	if _, found, err := cache.Get(ctx, owner, entry, "cr_one"); err != nil || found {
		t.Fatalf("empty cache: found=%v err=%v", found, err)
	}
	captured, width, latitude := "2024-05-01T10:20:30+08:00", 3024, 31.2304
	stored := Info{CapturedAt: &captured, Width: &width, Location: &Location{Latitude: latitude, Longitude: 121.4737}}
	if err := cache.Put(ctx, owner, entry, "cr_one", stored); err != nil {
		t.Fatal(err)
	}
	got, found, err := cache.Get(ctx, owner, entry, "cr_one")
	if err != nil || !found || got.CapturedAt == nil || *got.CapturedAt != captured || got.Width == nil || *got.Width != width || got.Location == nil || got.Location.Latitude != latitude || got.Height != nil {
		t.Fatalf("round trip: %+v found=%v err=%v", got, found, err)
	}
	// Replaced content must not be described by the old bytes' facts.
	if _, found, _ := cache.Get(ctx, owner, entry, "cr_two"); found {
		t.Fatal("a result stored for other content was served")
	}
	// The owner is part of the key: an id alone never reaches another account's row.
	if _, found, _ := cache.Get(ctx, other, entry, "cr_one"); found {
		t.Fatal("a result was served across owners")
	}
	// A result written by another parser version is a miss until it is overwritten.
	if _, err := pool.Exec(ctx, `update file_media_metadata set parser_version=parser_version+1 where node_id=$1::uuid`, entry); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := cache.Get(ctx, owner, entry, "cr_one"); found {
		t.Fatal("a result from another parser version was served")
	}
	// One row per entry: a new content state overwrites, including an empty result.
	if err := cache.Put(ctx, owner, entry, "cr_two", Info{}); err != nil {
		t.Fatal(err)
	}
	got, found, err = cache.Get(ctx, owner, entry, "cr_two")
	if err != nil || !found || !got.Empty() {
		t.Fatalf("empty result was not kept: %+v found=%v err=%v", got, found, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from file_media_metadata where node_id=$1::uuid`, entry).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows for one entry: %d %v", rows, err)
	}
	// Deleting the file leaves no coordinates behind.
	if _, err := pool.Exec(ctx, `delete from nodes where id=$1::uuid`, entry); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from file_media_metadata where node_id=$1::uuid`, entry).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after the entry was deleted: %d %v", rows, err)
	}
}
