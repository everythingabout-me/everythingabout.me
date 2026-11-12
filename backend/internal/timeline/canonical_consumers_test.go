package timeline

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/events"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/jobs"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func canonicalConsumersDB(t *testing.T) (*store.DB, context.Context) {
	t.Helper()
	address := testpg.Require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("eam_canonical_consumers_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+quoted+" with (force)"); err != nil {
			t.Error(err)
		}
		_ = admin.Close(context.Background())
	})
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	db, err := store.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("migrations unavailable", err)
	}
	for _, migration := range migrations {
		data, err := os.ReadFile(migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool().Exec(ctx, string(data)); err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(migration), err)
		}
	}
	return db, ctx
}

// A native atomic rename settles only the ancestor linkage. Consumers must retain
// the leaf identity while deriving its current location, even after cache eviction
// and recreation of the old path. Delete admission must persist those locations
// before detaching the ancestor, without touching another owner or old-path reuse.
func TestCanonicalConsumersAfterAncestorRename(t *testing.T) {
	db, ctx := canonicalConsumersDB(t)
	pool := db.Pool()
	owner := func(email string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `insert into users(email,password_hash) values($1,'test') returning id::text`, email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	user, other := owner("canonical@example.test"), owner("other@example.test")
	insert := func(owner, p string, folder bool) string {
		t.Helper()
		key := paths.KeyFor(p, owner)
		if folder {
			key = paths.DirKeyFor(p, owner)
		}
		var id string
		if err := pool.QueryRow(ctx, `insert into nodes(owner_id,path,key,parent,name,is_dir,size,mime)
 values($1::uuid,$2,$3,$4,$5,$6,7,'image/jpeg') returning id::text`, owner, p, key, paths.ParentOf(p), paths.NameOf(p), folder).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	folder := insert(user, "Before", true)
	leaf := insert(user, "Before/photo.jpg", false)
	index := nodes.New(db, nil)
	stream := events.New(db, index)
	now := time.Now().UTC()
	if err := stream.PublishAuto(ctx, user, []events.AutoInput{{ProducerKey: "stable-media", StartsAt: now, TZ: "UTC", Precision: "day", Title: "Photo", Media: []events.AutoMedia{{NodeID: leaf, Path: "Before/photo.jpg"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into timeline_assets(user_id,node_id,event_time,local_date,media_type)
 values($1::uuid,$2::uuid,$3,$3::timestamptz::date,'photo')`, user, leaf, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update nodes set path='After%_',parent='',name='After%_',key=$3 where owner_id=$1::uuid and id=$2::uuid`, user, folder, paths.DirKeyFor("After%_", user)); err != nil {
		t.Fatal(err)
	}
	var cached string
	if err := pool.QueryRow(ctx, `select path from nodes where id=$1::uuid`, leaf).Scan(&cached); err != nil || cached != "Before/photo.jpg" {
		t.Fatalf("expected stale descendant cache: %q, %v", cached, err)
	}
	timeline := &Service{db: db, engine: &Engine{Location: time.UTC}}
	assertReads := func() {
		t.Helper()
		page, _, err := stream.List(ctx, user, "", 10)
		if err != nil || len(page) != 1 || len(page[0].Payload.Media) != 1 {
			t.Fatalf("event media: %+v, %v", page, err)
		}
		media := page[0].Payload.Media[0]
		if media.Path != "After%_/photo.jpg" || media.Name != "photo.jpg" || media.URL != fc.InlineContentURL("After%_/photo.jpg") {
			t.Fatalf("media did not follow stable identity: %+v", media)
		}
		recent, err := timeline.recentFiles(ctx, user, 10)
		if err != nil {
			t.Fatal(err)
		}
		backfill, _, err := timeline.backfillFiles(ctx, user, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, files := range [][]FileInput{recent, backfill} {
			found := false
			for _, file := range files {
				if file.NodeID == leaf {
					found = true
					if file.Path != "After%_/photo.jpg" || file.Parent != "After%_" {
						t.Fatalf("file facts used cache: %+v", file)
					}
				}
			}
			if !found {
				t.Fatalf("stable file disappeared from facts: %+v", files)
			}
		}
		assets, err := timeline.monthAssets(ctx, user, now.AddDate(0, -1, 0), now.AddDate(0, 1, 0))
		if err != nil || len(assets) != 1 || assets[0].NodeID != leaf || assets[0].Path != "After%_/photo.jpg" || assets[0].Parent != "After%_" {
			t.Fatalf("month asset used cache: %+v, %v", assets, err)
		}
	}
	assertReads()
	recreated := insert(user, "Before/photo.jpg", false)
	var evicted bool
	if err := pool.QueryRow(ctx, `select path is null and key is null and parent is null from nodes where id=$1::uuid`, leaf).Scan(&evicted); err != nil || !evicted {
		t.Fatalf("old-path reuse did not evict stale cache: %t, %v", evicted, err)
	}
	assertReads()
	foreign := insert(other, "After%_/photo.jpg", false)
	neighbor := insert(user, "AfterXX/keep.jpg", false)
	cleanup := jobs.New(db, nil, index)
	cleanup.SetDeleteBinding("canonical-test")
	job, err := cleanup.StartDelete(ctx, user, "After%_")
	if err != nil || job == nil || job.Total != 2 {
		t.Fatalf("canonical delete admission: %+v, %v", job, err)
	}
	rows, err := pool.Query(ctx, `select path,key from fs_delete_entries where job_id=$1 order by path`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{}
	for rows.Next() {
		var p, key string
		if err := rows.Scan(&p, &key); err != nil {
			t.Fatal(err)
		}
		manifest[p] = key
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 2 || manifest["After%_"] != paths.DirKeyFor("After%_", user) || manifest["After%_/photo.jpg"] != paths.KeyFor("After%_/photo.jpg", user) {
		t.Fatalf("delete manifest used stale cache: %+v", manifest)
	}
	var retained int
	if err := pool.QueryRow(ctx, `select count(*) from nodes where id=any($1::uuid[])`, []string{recreated, foreign, neighbor}).Scan(&retained); err != nil || retained != 3 {
		t.Fatalf("delete touched old path, wildcard neighbor or foreign owner: %d, %v", retained, err)
	}
	page, _, err := stream.List(ctx, user, "", 10)
	if err != nil || len(page) != 1 || len(page[0].Payload.Media) != 0 {
		t.Fatalf("deleted media did not degrade cleanly: %+v, %v", page, err)
	}
}
