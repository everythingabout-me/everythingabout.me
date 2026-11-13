package filechanges

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/namecompare"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func changeDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_changes_")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) < 17 {
		t.Fatalf("migration discovery: %d %v", len(migrations), err)
	}
	// Every migration, not a prefix: this fixture used to stop at 032, but the package
	// queries file_operations.state since sql/047 promoted the journal columns, and a
	// fixture missing a column the code reads turns a schema mismatch into a confusing
	// runtime error (that is what made this package hang instead of fail). A partial
	// schema is a second, silent contract; only reuse one when the package provably reads
	// nothing beyond it.
	for range 2 {
		for _, path := range migrations {
			name := filepath.Base(path)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, string(data)); err != nil {
				t.Fatalf("migration %s: %v", name, err)
			}
		}
	}
	return pool
}

func newOwner(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	owner := testUUID(t)
	if _, err := pool.Exec(context.Background(), `insert into users(id,email,password_hash) values($1::uuid,$2,'')`, owner, owner+"@example.test"); err != nil {
		t.Fatal(err)
	}
	return owner
}
func appendCommitted(t *testing.T, pool *pgxpool.Pool, c Change) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	position, err := Append(ctx, tx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return position
}
func createChange(owner, key string, path fc.Path) Change {
	return Change{OwnerID: owner, EffectKey: key, Kind: "created", Coverage: "entry", After: snapshot(testEntry, path, "file"), Origin: "api",
		Actor: Actor{Kind: "token", PrincipalID: "token:private-row-id", Scope: "/", CanRead: true, CanWrite: true}, RequestID: "req-original"}
}

func TestPostgresDurableChanges(t *testing.T) {
	pool := changeDatabase(t)
	ctx := context.Background()
	t.Run("atomic rollback and repeatable confirmed effect", func(t *testing.T) {
		owner := newOwner(t, pool)
		initial, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		head, err := EnsureHead(ctx, initial, owner)
		if err != nil {
			t.Fatal(err)
		}
		if err = initial.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		c := createChange(owner, "atomic", "/Notes/a.txt")
		observed := time.Now().UTC().Truncate(time.Microsecond)
		c.ObservedAt = observed
		var auditID string
		for _, commit := range []bool{false, true} {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			position, err := Append(ctx, tx, c)
			if err != nil || position != 1 {
				t.Fatalf("append %d %v", position, err)
			}
			if err = MarkChanged(ctx, tx, owner, []string{testEntry, testRoot, testRoot}, position, observed); err != nil {
				t.Fatal(err)
			}
			zero := int64(0)
			a := Audit{OwnerID: owner, EffectKey: "audit-atomic", Action: "content", Outcome: "completed", Status: 200, BytesWritten: &zero, Path: &c.After.Path, Actor: c.Actor, Origin: "api"}
			if err = RecordAudit(ctx, tx, &a); err != nil {
				t.Fatal(err)
			}
			visible, err := ReadHead(ctx, pool, owner)
			if err != nil || visible.Position != 0 || visible.RootEpoch != head.RootEpoch {
				t.Fatalf("uncommitted head %+v %v", visible, err)
			}
			rows, err := Read(ctx, pool, owner, 0, 1, 10)
			if err != nil || len(rows) != 0 {
				t.Fatalf("uncommitted rows %+v %v", rows, err)
			}
			if commit {
				err = tx.Commit(ctx)
				auditID = a.ID
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		rows, err := Read(ctx, pool, owner, 0, 1, 10)
		if err != nil || len(rows) != 1 || !validUUID(rows[0].ID) {
			t.Fatalf("committed rows %+v %v", rows, err)
		}
		if !rows[0].ObservedAt.Equal(observed) || rows[0].After.ID != testEntry || rows[0].Actor.PrincipalID != c.Actor.PrincipalID {
			t.Fatalf("lost saved snapshot %+v", rows[0])
		}
		if delta := rows[0].ExpiresAt.Sub(rows[0].RecordedAt); delta < DefaultRetention-time.Second || delta > DefaultRetention+time.Second {
			t.Fatalf("retention %v", delta)
		}
		c.ObservedAt = observed.Add(time.Hour)
		if pos := appendCommitted(t, pool, c); pos != 1 {
			t.Fatalf("replay position %d", pos)
		}
		marker, err := ReadMarker(ctx, pool, owner, testRoot)
		if err != nil || marker.Position != 1 || !marker.LastChangedAt.Equal(observed) || !validUUID(marker.Token) {
			t.Fatalf("marker %+v %v", marker, err)
		}
		originalToken := marker.Token
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = MarkChanged(ctx, tx, owner, []string{testRoot}, 1, observed.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		marker, err = ReadMarker(ctx, pool, owner, testRoot)
		if err != nil || !marker.LastChangedAt.Equal(observed) || marker.Token != originalToken {
			t.Fatalf("replay changed marker time %+v %v", marker, err)
		}
		audits, err := ReadAudit(ctx, pool, owner, 10)
		if err != nil || len(audits) != 1 || audits[0].ID != auditID || audits[0].BytesWritten == nil || *audits[0].BytesWritten != 0 {
			t.Fatalf("audit %+v %v", audits, err)
		}
		next, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		position, err := Append(ctx, next, createChange(owner, "later-change", "/Notes/b.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if err = MarkChanged(ctx, next, owner, []string{testRoot}, position, observed.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err = next.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		marker, err = ReadMarker(ctx, pool, owner, testRoot)
		if err != nil || marker.Position != 2 || !marker.LastChangedAt.Equal(observed) || marker.Token == originalToken || !validUUID(marker.Token) {
			t.Fatalf("clock regression marker %+v %v", marker, err)
		}
		secondToken := marker.Token
		sameTime, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		position, err = Append(ctx, sameTime, createChange(owner, "same-time-change", "/Notes/c.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if err = MarkChanged(ctx, sameTime, owner, []string{testRoot}, position, observed); err != nil {
			t.Fatal(err)
		}
		if err = sameTime.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		marker, err = ReadMarker(ctx, pool, owner, testRoot)
		if err != nil || marker.Position != 3 || !marker.LastChangedAt.Equal(observed) || marker.Token == secondToken {
			t.Fatalf("timestamp collision marker %+v %v", marker, err)
		}
	})

	for _, commitFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("committed head ordering first commit=%v", commitFirst), func(t *testing.T) {
			owner, other := newOwner(t, pool), newOwner(t, pool)
			appendCommitted(t, pool, createChange(owner, "baseline", "/base"))
			a, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Rollback(ctx)
			if pos, err := Append(ctx, a, createChange(owner, "first", "/first")); err != nil || pos != 2 {
				t.Fatalf("first %d %v", pos, err)
			}
			b, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Rollback(ctx)
			var pid int32
			if err = b.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			type result struct {
				position int64
				err      error
			}
			done := make(chan result, 1)
			go func() {
				pos, err := Append(ctx, b, createChange(owner, "second", "/second"))
				if err == nil {
					err = b.Commit(ctx)
				}
				done <- result{pos, err}
			}()
			waitForLock(t, pool, pid)
			committed, err := ReadHead(ctx, pool, owner)
			if err != nil || committed.Position != 1 {
				t.Fatalf("head jumped uncommitted effect %+v %v", committed, err)
			}
			if got := appendCommitted(t, pool, createChange(other, "unrelated", "/other")); got != 1 {
				t.Fatal("unrelated owner blocked")
			}
			if commitFirst {
				err = a.Commit(ctx)
			} else {
				err = a.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-done:
				want := int64(2)
				if commitFirst {
					want = 3
				}
				if result.err != nil || result.position != want {
					t.Fatalf("ordered result %+v want %d", result, want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("waiting writer did not resume")
			}
		})
	}

	t.Run("concurrent dedup and conflicting payload", func(t *testing.T) {
		owner := newOwner(t, pool)
		c := createChange(owner, "same-effect", "/Notes/a")
		var wg sync.WaitGroup
		results := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				tx, err := pool.Begin(ctx)
				if err != nil {
					results <- err
					return
				}
				defer tx.Rollback(ctx)
				pos, err := Append(ctx, tx, c)
				if err == nil && pos != 1 {
					err = fmt.Errorf("duplicate position %d", pos)
				}
				if err == nil {
					err = tx.Commit(ctx)
				}
				results <- err
			})
		}
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}
		head, err := ReadHead(ctx, pool, owner)
		if err != nil || head.Position != 1 {
			t.Fatalf("duplicate head %+v %v", head, err)
		}
		conflict := c
		conflict.After = snapshot(testEntry, "/different", "file")
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = Append(ctx, tx, conflict); !errors.Is(err, ErrEffectConflict) {
			t.Fatalf("conflict error %v", err)
		}
		other := newOwner(t, pool)
		c.OwnerID = other
		if pos := appendCommitted(t, pool, c); pos != 1 {
			t.Fatalf("owner-scoped effect key %d", pos)
		}
	})

	t.Run("scope matching before limit and retained tombstones", func(t *testing.T) {
		owner, other := newOwner(t, pool), newOwner(t, pool)
		for i := 0; i < 8; i++ {
			c := createChange(owner, fmt.Sprintf("hidden-%d", i), fc.Path(fmt.Sprintf("/Private/%d", i)))
			c.ImpactPaths = []fc.Path{"/"}
			appendCommitted(t, pool, c)
		}
		first := appendCommitted(t, pool, createChange(owner, "visible-1", "/Apps/Straße/cafe\u0301.txt"))
		appendCommitted(t, pool, createChange(other, "other-owner", "/Apps/Straße/foreign"))
		before := snapshot(testEntry, "/Apps/Straße/deleted.txt", "file")
		last := appendCommitted(t, pool, Change{OwnerID: owner, EffectKey: "delete", Kind: "deleted", Before: before, Origin: "api"})
		rows, err := ReadScoped(ctx, pool, owner, "/apps/STRASSE", false, 0, last, 1)
		if err != nil || len(rows) != 1 || rows[0].Position != first {
			t.Fatalf("first visible page %+v %v", rows, err)
		}
		rows, err = ReadScoped(ctx, pool, owner, "/apps/STRASSE", false, first, last, 10)
		if err != nil || len(rows) != 1 || rows[0].Kind != "deleted" || rows[0].Before.Path != before.Path {
			t.Fatalf("retained tombstone %+v %v", rows, err)
		}
		projection, ok := Project(rows[0], "/apps/STRASSE", namecompare.Key(false))
		if !ok || projection.Type != "remove" {
			t.Fatalf("delete projection %+v %v", projection, ok)
		}
		var live int
		if err = pool.QueryRow(ctx, `select count(*) from nodes where owner_id=$1::uuid and id=$2::uuid`, owner, testEntry).Scan(&live); err != nil || live != 0 {
			t.Fatalf("tombstone depended on live row %d %v", live, err)
		}
		if rows, err = ReadScoped(ctx, pool, owner, "/Apps/Straße-old", false, 0, last, 10); err != nil || len(rows) != 0 {
			t.Fatalf("sibling prefix visible %+v %v", rows, err)
		}
		ancestor := Change{OwnerID: owner, EffectKey: "ancestor-move", Kind: "moved", Coverage: "subtree", Before: snapshot(testEntry, "/Apps", "folder"), After: snapshot(testEntry, "/Private/Moved", "folder")}
		last = appendCommitted(t, pool, ancestor)
		rows, err = ReadScoped(ctx, pool, owner, "/Apps/Notes", true, last-1, last, 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("ancestor SQL candidate %+v %v", rows, err)
		}
		p, ok := Project(rows[0], "/Apps/Notes", namecompare.Key(true))
		if !ok || p.Type != "invalidated" || p.Path != "/Apps/Notes" {
			t.Fatalf("ancestor projection %+v %v", p, ok)
		}
	})

	t.Run("preallocated event token agrees across affected markers", func(t *testing.T) {
		owner := newOwner(t, pool)
		c := createChange(owner, "explicit-token", "/Notes/token.txt")
		c.ID = testUUID(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		position, err := Append(ctx, tx, c)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		if err = MarkChangedWithToken(ctx, tx, owner, []string{testEntry, testRoot}, position, at, c.ID); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{testEntry, testRoot} {
			marker, err := ReadMarker(ctx, pool, owner, id)
			if err != nil || marker.Token != c.ID || !marker.LastChangedAt.Equal(at) {
				t.Fatalf("explicit token marker %+v %v", marker, err)
			}
		}
		rows, err := Read(ctx, pool, owner, 0, position, 1)
		if err != nil || len(rows) != 1 || rows[0].ID != c.ID {
			t.Fatalf("saved event identity %+v %v", rows, err)
		}
	})

	t.Run("retention removes contiguous prefix but preserves effect identity", func(t *testing.T) {
		owner := newOwner(t, pool)
		changes := []Change{}
		for i := 0; i < 3; i++ {
			c := createChange(owner, fmt.Sprintf("retain-%d", i), fc.Path(fmt.Sprintf("/Notes/%d", i)))
			changes = append(changes, c)
			appendCommitted(t, pool, c)
		}
		now := time.Now().UTC()
		if _, err := pool.Exec(ctx, `update file_changes set expires_at=$2 where owner_id=$1::uuid and position in(1,3)`, owner, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		removed, err := PruneExpired(ctx, tx, owner, now)
		if err != nil || removed != 1 {
			t.Fatalf("prefix prune %d %v", removed, err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		head, err := ReadHead(ctx, pool, owner)
		if err != nil || head.RetainedAfter != 1 || head.Position != 3 || head.RetainedSince.IsZero() {
			t.Fatalf("retention head %+v %v", head, err)
		}
		if pos := appendCommitted(t, pool, changes[0]); pos != 1 {
			t.Fatalf("expired effect replay allocated %d", pos)
		}
		rows, err := Read(ctx, pool, owner, 0, 3, 10)
		if err != nil || len(rows) != 2 || rows[0].Position != 2 {
			t.Fatalf("retained suffix %+v %v", rows, err)
		}
	})

	t.Run("audit isolation keysets immutable actor and completed zero bytes", func(t *testing.T) {
		owner, other := newOwner(t, pool), newOwner(t, pool)
		at := time.Now().UTC().Truncate(time.Microsecond)
		zero := int64(0)
		var recorded []Audit
		for i, ownerID := range []string{owner, owner, other} {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			a := Audit{OwnerID: ownerID, EffectKey: fmt.Sprintf("audit-%d", i), Action: "content", Outcome: "completed", Actor: Actor{Kind: "token", PrincipalID: "token:retired-id", Label: "original-label"}, ObservedAt: at, Origin: "api"}
			if i == 0 {
				a.Outcome = "admitted"
			}
			if i == 1 {
				a.BytesWritten = &zero
			}
			if err = RecordAudit(ctx, tx, &a); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			recorded = append(recorded, a)
		}
		page, err := ReadAuditPage(ctx, pool, owner, 1, time.Time{}, "")
		if err != nil || len(page) != 1 {
			t.Fatalf("audit first %+v %v", page, err)
		}
		next, err := ReadAuditPage(ctx, pool, owner, 2, page[0].ObservedAt, page[0].ID)
		if err != nil || len(next) != 1 || next[0].ID == page[0].ID {
			t.Fatalf("audit next %+v %v", next, err)
		}
		all := append(page, next...)
		nilBytes, zeroBytes := false, false
		for _, a := range all {
			if a.OwnerID != owner || a.Actor.Label != "original-label" {
				t.Fatalf("audit owner/actor %+v", a)
			}
			if a.BytesWritten == nil {
				nilBytes = true
			} else if *a.BytesWritten == 0 {
				zeroBytes = true
			}
		}
		if !nilBytes || !zeroBytes {
			t.Fatal("admission nil and completed zero collapsed")
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		a := recorded[1]
		a.ObservedAt = at.Add(time.Hour)
		if err = RecordAudit(ctx, tx, &a); err != nil || a.ID != recorded[1].ID || !a.ObservedAt.Equal(at) {
			t.Fatalf("audit replay %+v %v", a, err)
		}
		different := int64(1)
		a.BytesWritten = &different
		if err = RecordAudit(ctx, tx, &a); !errors.Is(err, ErrEffectConflict) {
			t.Fatalf("changed byte claim accepted %v", err)
		}
	})
}

func waitForLock(t *testing.T, pool *pgxpool.Pool, pid int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `select coalesce(wait_event_type='Lock',false) from pg_stat_activity where pid=$1`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("writer never waited on the owner head")
		case <-ticker.C:
		}
	}
}
