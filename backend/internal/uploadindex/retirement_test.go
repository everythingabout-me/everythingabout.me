package uploadindex

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestExplicitCompletionStopsFutureChecks(t *testing.T) {
	for _, folder := range []bool{false, true} {
		t.Run(fmt.Sprintf("folder=%t", folder), func(t *testing.T) {
			now := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
			j := newMemoryJournal()
			s := testService(j, ObserverFunc(func(context.Context, string, string, bool) (string, error) {
				t.Error("completed path was observed again")
				return "", nil
			}), &now)
			if err := s.Register(context.Background(), testOwner, "folder", folder, now); err != nil {
				t.Fatal(err)
			}
			if err := s.Settle(context.Background(), testOwner, "folder", folder); err != nil {
				t.Fatal(err)
			}
			now = now.Add(30 * coldInterval)
			runTest(t, s)
			if len(j.rows) != 0 {
				t.Fatal("completed observation was retained")
			}
		})
	}
}

func TestRecoveredFolderExitsOnlyAfterIndexSuccess(t *testing.T) {
	now := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
	j := newMemoryJournal()
	present := false
	var indexErr error
	calls := 0
	s := testService(j, ObserverFunc(func(context.Context, string, string, bool) (string, error) {
		calls++
		if !present {
			return "", nil
		}
		return "verified-folder-marker", indexErr
	}), &now)
	key := testKey()
	key.IsDir = true
	if err := s.Register(context.Background(), key.OwnerID, key.Path, true, now); err != nil {
		t.Fatal(err)
	}
	runTest(t, s)
	if j.record(key).ID == "" {
		t.Fatal("missing marker incorrectly retired")
	}
	present = true
	indexErr = errors.New("index unavailable")
	now = now.Add(hotInterval)
	if err := s.RunOnce(context.Background()); !errors.Is(err, indexErr) {
		t.Fatal("index failure was hidden")
	}
	if j.record(key).ID == "" {
		t.Fatal("marker retired before index repair succeeded")
	}
	indexErr = nil
	now = now.Add(retryInterval)
	runTest(t, s)
	if j.record(key).ID != "" {
		t.Fatal("recovered folder marker retained daily observation")
	}
	now = now.Add(30 * coldInterval)
	runTest(t, s)
	if calls != 3 {
		t.Fatalf("completed marker was observed again: %d", calls)
	}
}

func TestOldClaimCannotRetireRecreatedWatch(t *testing.T) {
	for _, folder := range []bool{false, true} {
		t.Run(fmt.Sprintf("folder=%t", folder), func(t *testing.T) {
			now := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
			j := newMemoryJournal()
			key := testKey()
			key.IsDir = folder
			if err := j.Register(context.Background(), key, now, now); err != nil {
				t.Fatal(err)
			}
			claims, err := j.Claim(context.Background(), key.Binding, now, now.Add(claimLease), 1)
			if err != nil || len(claims) != 1 {
				t.Fatal("initial claim failed", err)
			}
			old := claims[0]
			if err := j.Settle(context.Background(), key, now, now); err != nil {
				t.Fatal(err)
			}
			if err := j.Register(context.Background(), key, now, now); err != nil {
				t.Fatal(err)
			}
			claims, err = j.Claim(context.Background(), key.Binding, now, now.Add(claimLease), 1)
			if err != nil || len(claims) != 1 {
				t.Fatal("replacement claim failed", err)
			}
			fresh := claims[0]
			if old.Generation != fresh.Generation || old.LeaseVersion != fresh.LeaseVersion || old.ID == fresh.ID {
				t.Fatal("test did not exercise reset-counter ABA with a new incarnation")
			}
			if err := j.Finish(context.Background(), old, Result{Fingerprint: "old-marker"}); err != nil {
				t.Fatal(err)
			}
			if row := j.record(key); row.ID != fresh.ID || row.result.Fingerprint != "" {
				t.Fatal("old claim altered replacement watch")
			}
		})
	}
}

func TestHistoricalSettledRowsAreNotClaimed(t *testing.T) {
	now := time.Now()
	j := newMemoryJournal()
	if err := j.Register(context.Background(), testKey(), now, now); err != nil {
		t.Fatal(err)
	}
	row := j.rows[testKey()]
	row.Settled = true
	j.rows[testKey()] = row
	claims, err := j.Claim(context.Background(), testKey().Binding, now, now.Add(claimLease), batchLimit)
	if err != nil || len(claims) != 0 {
		t.Fatal("historical completed record was scheduled", err)
	}
}
