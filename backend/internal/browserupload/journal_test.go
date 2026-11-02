package browserupload

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresJournalLifecycle(t *testing.T) {
	address := os.Getenv("EAM_TEST_DATABASE_URL")
	if address == "" {
		t.Skip("EAM_TEST_DATABASE_URL unset; disposable PostgreSQL integration not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	name := fmt.Sprintf("browser_upload_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "drop database "+quoted+" with (force)"); err != nil {
			t.Error(err)
		}
	}()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `create table users(id uuid primary key)`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../sql/022_browser_uploads.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal("migration is not repeatable", err)
		}
	}
	owner := "11111111-1111-1111-1111-111111111111"
	other := "22222222-2222-2222-2222-222222222222"
	if _, err = pool.Exec(ctx, `insert into users(id) values($1::uuid),($2::uuid)`, owner, other); err != nil {
		t.Fatal(err)
	}
	j := NewJournal(pool)
	newSession := func(id, path string) Session {
		return Session{ID: id, Binding: "bound-root", OwnerID: owner, Path: path, Size: 3, Mime: "application/json", State: "open", Checkpoint: json.RawMessage(`{}`), ExpiresAt: time.Now().Add(time.Hour)}
	}
	original := newSession("first", "Health/steps.json")
	if err = j.Create(ctx, original); err != nil {
		t.Fatal(err)
	}
	if _, err = j.Get(ctx, "another-root", owner, original.ID); err == nil {
		t.Fatal("storage binding isolation failed")
	}
	if _, err = j.Get(ctx, "bound-root", other, original.ID); err == nil {
		t.Fatal("owner isolation failed")
	}
	if err = j.Create(ctx, newSession("duplicate", original.Path)); err == nil {
		t.Fatal("two active uploads admitted to same path")
	}
	if err = j.CheckConflict(ctx, "bound-root", owner, "Health", true); err == nil {
		t.Fatal("subtree conflict was ignored")
	}
	if err = j.CheckConflict(ctx, "bound-root", owner, "Health-old", true); err != nil {
		t.Fatal("sibling prefix incorrectly conflicts", err)
	}
	due, err := j.Due(ctx, "bound-root", 32)
	if err != nil || len(due) != 0 {
		t.Fatal("unstarted uploads polled before expiry", err)
	}
	// Two processes cannot both receive/finalize the same intent.
	var claims atomic.Int32
	var winner Session
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := j.Claim(ctx, "bound-root", owner, original.ID, leaseTTL)
			if err == nil {
				claims.Add(1)
				mu.Lock()
				winner = s
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("concurrent claim winners=%d", claims.Load())
	}
	winner.State = "receiving"
	winner.Checkpoint = json.RawMessage(`{"providerId":"private-temporary"}`)
	if err = j.Save(ctx, winner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = j.Renew(ctx, winner, time.Minute); err != nil {
		t.Fatal(err)
	}
	stale := winner
	if _, err = pool.Exec(ctx, `update browser_uploads set lease_until=clock_timestamp()-interval '1 second' where id=$1`, winner.ID); err != nil {
		t.Fatal(err)
	}
	winner, err = j.Claim(ctx, "bound-root", owner, winner.ID, leaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	stale.State = "completed"
	stale.Result = json.RawMessage(`{"stale":true}`)
	if err = j.Save(ctx, stale, time.Now()); err == nil {
		t.Fatal("expired lease overwrote successor")
	}
	if err = j.Renew(ctx, stale, 0); err == nil {
		t.Fatal("old receiver released successor lease")
	}
	winner.State = "publishing"
	if err = j.Save(ctx, winner, time.Now()); err != nil {
		t.Fatal(err)
	}
	winner.State = "applied"
	if err = j.Save(ctx, winner, time.Now()); err != nil {
		t.Fatal(err)
	}
	winner.State = "completed"
	winner.Result = json.RawMessage(`{"node":{"path":"Health/steps.json"}}`)
	if err = j.Save(ctx, winner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = j.Renew(ctx, winner, 0); err != nil {
		t.Fatal(err)
	}
	due, err = j.Due(ctx, "bound-root", 32)
	if err != nil || len(due) != 0 {
		t.Fatal("completed receipt remained in recovery query", err)
	}
	if err = j.CheckConflict(ctx, "bound-root", owner, original.Path, false); err != nil {
		t.Fatal("completed upload retained guard", err)
	}
	replay, err := j.Get(ctx, "bound-root", owner, original.ID)
	if err != nil || !validJSON(replay.Result) {
		t.Fatal("completion receipt not retained", err)
	}
	if err = j.Create(ctx, newSession("replacement", original.Path)); err != nil {
		t.Fatal("completed receipt blocked later upload", err)
	}
	// Terminal cleanup is database-only and never drops an active upload.
	if _, err = pool.Exec(ctx, `update browser_uploads set retain_until=clock_timestamp()-interval '1 second' where id=$1`, original.ID); err != nil {
		t.Fatal(err)
	}
	if err = j.Cleanup(ctx, "bound-root", 128); err != nil {
		t.Fatal(err)
	}
	if _, err = j.Get(ctx, "bound-root", owner, original.ID); err == nil {
		t.Fatal("old receipt was not cleaned")
	}
	if _, err = j.Get(ctx, "bound-root", owner, "replacement"); err != nil {
		t.Fatal("active upload was pruned", err)
	}
	// Recreated services replay only applied work; never resubmit a native effect.
	driver := &fakeDriver{applied: true}
	indexCalls := 0
	service := New("bound-root", j, driver, func(ctx context.Context, s Session) (json.RawMessage, error) {
		indexCalls++
		return successfulIndex(ctx, s)
	})
	pending, err := j.Claim(ctx, "bound-root", owner, "replacement", leaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	pending.State = "applied"
	pending.Checkpoint = json.RawMessage(`{"providerId":"known"}`)
	if err = j.Save(ctx, pending, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = j.Renew(ctx, pending, 0); err != nil {
		t.Fatal(err)
	}
	if err = service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if indexCalls != 1 || driver.publishCalls != 0 {
		t.Fatal("restart republished instead of indexing applied object")
	}
	// A large completed history must not create per-file native checks. Deliberately
	// make all completed next_check_at values due; the lifecycle partial index and
	// query predicate must exclude them, independent of timing or machine speed.
	if _, err = pool.Exec(ctx, `insert into browser_uploads
		(id,binding,owner_id,path,size,mime,state,checkpoint,result,expires_at,next_check_at,retain_until)
		select 'scale-'||n::text,'bound-root',$1::uuid,'Archive/'||n::text,0,'application/json',
		'completed','{}'::jsonb,'{}'::jsonb,clock_timestamp(),clock_timestamp()-interval '1 day',clock_timestamp()+interval '1 day'
		from generate_series(1,100000) n`, owner); err != nil {
		t.Fatal(err)
	}
	if due, err = j.Due(ctx, "bound-root", 32); err != nil || len(due) != 0 {
		t.Fatal("completed history entered the recovery batch", err)
	}
	pendingOnly := newSession("pending-only", "Health/pending-only")
	pendingOnly.ExpiresAt = time.Now().Add(-time.Minute)
	if err = j.Create(ctx, pendingOnly); err != nil {
		t.Fatal(err)
	}
	if due, err = j.Due(ctx, "bound-root", 32); err != nil || len(due) != 1 || due[0].ID != "pending-only" {
		t.Fatal("large completed history obscured active recovery", err)
	}
	if err = service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if driver.publishCalls != 0 || driver.inspectCalls != 0 || driver.discardCalls != 1 || indexCalls != 1 {
		t.Fatal("completed history triggered native I/O or repeated index writes")
	}
	if _, err = pool.Exec(ctx, `delete from users where id=$1::uuid`, owner); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(ctx, `select count(*) from browser_uploads where owner_id=$1::uuid`, owner).Scan(&count); err != nil || count != 0 {
		t.Fatal("owner cascade left upload records", err)
	}
}
