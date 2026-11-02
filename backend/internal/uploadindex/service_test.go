package uploadindex

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testOwner = "11111111-1111-4111-8111-111111111111"

// memoryJournal models durable scheduling independently from the service. The
// Postgres conformance test exercises the actual generation/lease predicates.
type memoryRecord struct {
	Watch
	next   time.Time
	lease  time.Time
	result Result
}

type memoryJournal struct {
	mu          sync.Mutex
	rows        map[Key]memoryRecord
	registerErr error
	finishErr   error
	incarnation int
}

func newMemoryJournal() *memoryJournal { return &memoryJournal{rows: make(map[Key]memoryRecord)} }

func (j *memoryJournal) Register(_ context.Context, key Key, until, now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.registerErr != nil {
		return j.registerErr
	}
	row, exists := j.rows[key]
	if !exists {
		j.incarnation++
		row.ID = fmt.Sprint(j.incarnation)
	}
	row.Key = key
	row.Generation++
	row.Settled = false
	if until.After(row.IssuedUntil) {
		row.IssuedUntil = until
	}
	row.next, row.lease = now, time.Time{}
	j.rows[key] = row
	return nil
}

func (j *memoryJournal) Settle(_ context.Context, key Key, now, next time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.rows, key)
	return nil
}

func (j *memoryJournal) Claim(_ context.Context, binding string, now, lease time.Time, limit int) ([]Watch, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var result []Watch
	for key, row := range j.rows {
		if key.Binding != binding || row.Settled || row.next.After(now) || row.lease.After(now) {
			continue
		}
		row.LeaseVersion++
		row.lease = lease
		j.rows[key] = row
		result = append(result, row.Watch)
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (j *memoryJournal) Finish(_ context.Context, watch Watch, result Result) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finishErr != nil {
		return j.finishErr
	}
	row, exists := j.rows[watch.Key]
	if !exists || row.ID != watch.ID || row.Generation != watch.Generation || row.LeaseVersion != watch.LeaseVersion {
		return nil
	}
	if watch.IsDir && result.Error == "" && result.Fingerprint != "" {
		delete(j.rows, watch.Key)
		return nil
	}
	row.next, row.lease, row.result = result.NextCheck, time.Time{}, result
	j.rows[watch.Key] = row
	return nil
}

func (j *memoryJournal) record(key Key) memoryRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.rows[key]
}

func testService(j Journal, observer Observer, now *time.Time) *Service {
	s := New("binding-a", j, observer)
	s.now = func() time.Time { return *now }
	return s
}

func testKey() Key { return Key{Binding: "binding-a", OwnerID: testOwner, Path: "Health/day.json"} }

func registerTest(t *testing.T, s *Service, now time.Time) {
	t.Helper()
	if err := s.Register(context.Background(), testOwner, testKey().Path, false, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func runTest(t *testing.T, s *Service) {
	t.Helper()
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLostConfirmRepairsAfterServiceRestart(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	j := newMemoryJournal()
	var object string
	index := make(map[string]string)
	observer := ObserverFunc(func(_ context.Context, owner, path string, folder bool) (string, error) {
		if owner != testOwner || path != testKey().Path || folder {
			t.Error("observation crossed its owner/path/kind")
		}
		if object != "" {
			index[path] = object
		}
		return object, nil
	})
	registerTest(t, testService(j, observer, &now), now)
	// The browser disappears after native bytes finish; no confirm or Settle call.
	object = "native-etag:size:last-modified"
	restarted := testService(j, observer, &now)
	runTest(t, restarted)
	if index[testKey().Path] != object {
		t.Fatal("completed native object was not indexed after restart")
	}
	row := j.record(testKey())
	if row.Settled || !row.next.Equal(now.Add(hotInterval)) {
		t.Fatal("observing an existing object incorrectly settled its watch")
	}
}

func TestMissingAndProviderErrorsRemainRetryable(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	j := newMemoryJournal()
	var providerErr error
	s := testService(j, ObserverFunc(func(context.Context, string, string, bool) (string, error) { return "", providerErr }), &now)
	registerTest(t, s, now)
	runTest(t, s)
	if row := j.record(testKey()); row.result.Fingerprint != "" || !row.next.Equal(now.Add(hotInterval)) {
		t.Fatal("missing object was not retained for hot observation")
	}
	now = now.Add(hotInterval)
	providerErr = errors.New("provider unavailable")
	if err := s.RunOnce(context.Background()); !errors.Is(err, providerErr) {
		t.Fatalf("expected provider error, got %v", err)
	}
	if row := j.record(testKey()); row.result.Error != providerErr.Error() || !row.next.Equal(now.Add(retryInterval)) {
		t.Fatal("provider failure was not durably rescheduled")
	}
	providerErr = nil
	now = now.Add(retryInterval)
	runTest(t, s)
	if row := j.record(testKey()); row.result.Error != "" {
		t.Fatal("successful retry did not clear prior error")
	}
}

func TestUnresolvedLegacyExpiryDoesNotDiscardLateObjects(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	j := newMemoryJournal()
	object := ""
	s := testService(j, ObserverFunc(func(context.Context, string, string, bool) (string, error) { return object, nil }), &now)
	registerTest(t, s, now)
	now = now.Add(2*time.Hour + time.Second)
	runTest(t, s)
	if row := j.record(testKey()); !row.next.Equal(now.Add(coldInterval)) {
		t.Fatal("expired unresolved legacy watch should remain on cold schedule")
	}
	object = "late-native-object"
	now = now.Add(coldInterval)
	runTest(t, s)
	if j.record(testKey()).result.Fingerprint != object {
		t.Fatal("late unresolved legacy object disappeared from observation after expiry")
	}
}

func TestRenewalAndConfirmFenceStaleAcknowledgment(t *testing.T) {
	for _, settle := range []bool{false, true} {
		t.Run(fmt.Sprintf("settle=%t", settle), func(t *testing.T) {
			now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
			j := newMemoryJournal()
			var s *Service
			s = testService(j, ObserverFunc(func(ctx context.Context, owner, path string, folder bool) (string, error) {
				if settle {
					return "old", s.Settle(ctx, owner, path, folder)
				}
				return "old", s.Register(ctx, owner, path, folder, now.Add(3*time.Hour))
			}), &now)
			registerTest(t, s, now)
			runTest(t, s)
			row := j.record(testKey())
			if settle {
				if row.ID != "" {
					t.Fatal("old acknowledgment recreated explicitly completed watch")
				}
				return
			}
			if row.result.Fingerprint != "" || row.Generation != 2 {
				t.Fatal("old observation overwrote renewed generation")
			}
			if !row.next.Equal(now) || !row.IssuedUntil.Equal(now.Add(3*time.Hour)) {
				t.Fatal("old observation postponed renewed upload")
			}
		})
	}
}

func TestIndexCheckpointFailureReplaysAfterLease(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	j := newMemoryJournal()
	j.finishErr = errors.New("database unavailable")
	index := ""
	writes := 0
	observer := ObserverFunc(func(context.Context, string, string, bool) (string, error) {
		if index != "object" {
			index = "object"
			writes++
		}
		return index, nil
	})
	s := testService(j, observer, &now)
	registerTest(t, s, now)
	if err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("checkpoint failure was hidden")
	}
	j.finishErr = nil
	now = now.Add(claimLease)
	runTest(t, testService(j, observer, &now))
	if writes != 1 || j.record(testKey()).result.Fingerprint != "object" {
		t.Fatal("replayed observation did not converge idempotently")
	}
}

func TestClaimBatchAndParallelismAreBounded(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	j := newMemoryJournal()
	var active, maximum, calls atomic.Int32
	s := testService(j, ObserverFunc(func(context.Context, string, string, bool) (string, error) {
		current := active.Add(1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		time.Sleep(time.Millisecond)
		calls.Add(1)
		active.Add(-1)
		return "object", nil
	}), &now)
	for index := 0; index < 100; index++ {
		if err := s.Register(context.Background(), testOwner, fmt.Sprintf("folder/%d", index), false, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	runTest(t, s)
	if calls.Load() != batchLimit || maximum.Load() > parallelLimit {
		t.Fatalf("unbounded work: calls=%d, parallel=%d", calls.Load(), maximum.Load())
	}
}

func TestShutdownLeavesInterruptedClaimForRestart(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	j := newMemoryJournal()
	started := make(chan struct{})
	s := testService(j, ObserverFunc(func(ctx context.Context, _, _ string, _ bool) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}), &now)
	registerTest(t, s, now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not immediately resume due work")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop with server context")
	}
	if row := j.record(testKey()); !row.result.CheckedAt.IsZero() {
		t.Fatal("canceled observation was acknowledged")
	}
	now = now.Add(claimLease)
	restarted := testService(j, ObserverFunc(func(context.Context, string, string, bool) (string, error) { return "after-restart", nil }), &now)
	runTest(t, restarted)
	if j.record(testKey()).result.Fingerprint != "after-restart" {
		t.Fatal("interrupted claim was not replayed")
	}
}

func TestBindingOwnerAndKindStayIsolated(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	j := newMemoryJournal()
	other := New("binding-b", j, ObserverFunc(func(context.Context, string, string, bool) (string, error) {
		t.Error("wrong binding observed")
		return "", nil
	}))
	other.now = func() time.Time { return now }
	registerTest(t, other, now)
	s := testService(j, ObserverFunc(func(_ context.Context, owner, _ string, folder bool) (string, error) {
		if owner != testOwner || !folder {
			t.Error("owner or object kind was not preserved")
		}
		return "folder-marker", nil
	}), &now)
	if err := s.Register(context.Background(), testOwner, testKey().Path, true, now); err != nil {
		t.Fatal(err)
	}
	runTest(t, s)
	if row := j.record(Key{Binding: "binding-b", OwnerID: testOwner, Path: testKey().Path}); !row.result.CheckedAt.IsZero() {
		t.Fatal("watch from another binding was claimed")
	}
}

func TestRegistrationFailurePropagatesAndRejectsRoot(t *testing.T) {
	now := time.Now()
	j := newMemoryJournal()
	j.registerErr = errors.New("database unavailable")
	s := testService(j, nil, &now)
	if err := s.Register(context.Background(), testOwner, "file", false, now); !errors.Is(err, j.registerErr) {
		t.Fatal("durability failure must prevent credentials from being returned")
	}
	if err := s.Register(context.Background(), testOwner, "", true, now); err == nil {
		t.Fatal("root observation accepted")
	}
}
