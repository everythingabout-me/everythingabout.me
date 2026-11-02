package browserupload

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryJournal struct {
	mu            sync.Mutex
	sessions      map[string]Session
	next          map[string]time.Time
	leased        map[string]bool
	failSaveState string
	finishCalls   int
}

func newMemory() *memoryJournal {
	return &memoryJournal{sessions: map[string]Session{}, next: map[string]time.Time{}, leased: map[string]bool{}}
}
func clone(s Session) Session {
	s.Checkpoint = append(json.RawMessage(nil), s.Checkpoint...)
	s.Result = append(json.RawMessage(nil), s.Result...)
	return s
}
func (j *memoryJournal) Create(_ context.Context, s Session) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, other := range j.sessions {
		if other.Binding == s.Binding && other.OwnerID == s.OwnerID && other.Path == s.Path && other.State != "completed" && other.State != "aborted" {
			return fail(409, "conflict")
		}
	}
	j.sessions[s.ID] = clone(s)
	j.next[s.ID] = s.ExpiresAt
	return nil
}
func (j *memoryJournal) Get(_ context.Context, b, o, id string) (Session, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	s, ok := j.sessions[id]
	if !ok || s.Binding != b || s.OwnerID != o {
		return Session{}, fail(404, "missing")
	}
	return clone(s), nil
}
func (j *memoryJournal) Claim(ctx context.Context, b, o, id string, _ time.Duration) (Session, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	s, ok := j.sessions[id]
	if !ok || s.Binding != b || s.OwnerID != o {
		return s, fail(404, "missing")
	}
	if j.leased[id] || s.State == "completed" || s.State == "aborted" {
		return s, fail(409, "busy")
	}
	s.LeaseToken = id + "-lease"
	j.sessions[id] = s
	j.leased[id] = true
	return clone(s), nil
}
func (j *memoryJournal) Renew(_ context.Context, s Session, ttl time.Duration) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	current := j.sessions[s.ID]
	if !j.leased[s.ID] || current.LeaseToken != s.LeaseToken {
		return fail(409, "lost lease")
	}
	if ttl == 0 {
		j.leased[s.ID] = false
	}
	return nil
}
func (j *memoryJournal) Save(ctx context.Context, s Session, next time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.leased[s.ID] || j.sessions[s.ID].LeaseToken != s.LeaseToken {
		return fail(409, "lost lease")
	}
	if j.failSaveState == s.State {
		j.failSaveState = ""
		return errors.New("database failed")
	}
	j.sessions[s.ID] = clone(s)
	j.next[s.ID] = next
	j.finishCalls++
	return nil
}
func (j *memoryJournal) Due(_ context.Context, b string, limit int) ([]Session, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Session
	for _, s := range j.sessions {
		if s.Binding == b && s.State != "completed" && s.State != "aborted" && !j.leased[s.ID] && !j.next[s.ID].After(time.Now()) {
			out = append(out, clone(s))
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}
func (j *memoryJournal) Cleanup(context.Context, string, int) error { return nil }
func (j *memoryJournal) CheckConflict(_ context.Context, b, o, path string, subtree bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.sessions {
		if s.Binding == b && s.OwnerID == o && s.State != "completed" && s.State != "aborted" && (s.Path == path || subtree && strings.HasPrefix(s.Path, path+"/") || strings.HasPrefix(path, s.Path+"/")) {
			return fail(409, "conflict")
		}
	}
	return nil
}
func (j *memoryJournal) due(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.leased[id] = false
	j.next[id] = time.Now().Add(-time.Second)
}

type fakeDriver struct {
	stageCalls, publishCalls, inspectCalls, discardCalls int
	inspection                                           Inspection
	stageError, publishError, discardError               error
	applied                                              bool
	stageHook                                            func()
	publishHook                                          func()
}

func (d *fakeDriver) Stage(ctx context.Context, s Session, body io.Reader, checkpoint func(context.Context, json.RawMessage) error) (json.RawMessage, error) {
	d.stageCalls++
	value := json.RawMessage(`{"providerId":"known-temporary-id"}`)
	if err := checkpoint(ctx, value); err != nil {
		return nil, err
	}
	if d.stageHook != nil {
		d.stageHook()
	}
	if d.stageError != nil {
		return nil, d.stageError
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != s.Size {
		return nil, errors.New("body length mismatch")
	}
	return value, nil
}
func (d *fakeDriver) Publish(_ context.Context, _ Session, _ json.RawMessage) (bool, error) {
	d.publishCalls++
	if d.publishHook != nil {
		d.publishHook()
	}
	return d.applied, d.publishError
}
func (d *fakeDriver) Inspect(context.Context, Session) (Inspection, error) {
	d.inspectCalls++
	return d.inspection, nil
}
func (d *fakeDriver) Discard(context.Context, Session) error { d.discardCalls++; return d.discardError }
func start(t *testing.T, j *memoryJournal, d *fakeDriver, index Index) (*Service, Session) {
	t.Helper()
	s := New("binding", j, d, index)
	session, err := s.Begin(context.Background(), "owner", "Health/steps.json", 3, "application/json", 123)
	if err != nil {
		t.Fatal(err)
	}
	return s, session
}
func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Status != status {
		t.Fatalf("wanted status %d, got %v", status, err)
	}
}
func successfulIndex(context.Context, Session) (json.RawMessage, error) {
	return json.RawMessage(`{"path":"Health/steps.json"}`), nil
}

func TestSuccessfulUploadLeavesRecoveryAndReplaysReceipt(t *testing.T) {
	j := newMemory()
	d := &fakeDriver{applied: true}
	service, session := start(t, j, d, successfulIndex)
	if d.stageCalls != 0 {
		t.Fatal("Begin allocated native data")
	}
	result, err := service.Receive(context.Background(), "owner", session.ID, strings.NewReader("abc"), 3)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "completed" {
		t.Fatalf("state=%s", stored.State)
	}
	if err = service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	replay, err := service.Confirm(context.Background(), "owner", session.ID)
	if err != nil || string(replay) != string(result) {
		t.Fatal("lost response receipt replay failed", err)
	}
	if d.publishCalls != 1 || d.inspectCalls != 0 {
		t.Fatal("successful file was polled or republished")
	}
	if err = service.CheckConflict(context.Background(), "owner", "Health", true); err != nil {
		t.Fatal("completed receipt retained path guard")
	}
}
func TestInterruptedBodyNeverPublishesAndReleasesOnlyAfterCleanup(t *testing.T) {
	j := newMemory()
	d := &fakeDriver{stageError: io.ErrUnexpectedEOF, discardError: errors.New("temporary abort failure")}
	service, session := start(t, j, d, successfulIndex)
	if _, err := service.Receive(context.Background(), "owner", session.ID, strings.NewReader("a"), 3); err == nil {
		t.Fatal("interrupted body succeeded")
	}
	stored, _ := j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "aborting" {
		t.Fatalf("state=%s", stored.State)
	}
	wantStatus(t, service.CheckConflict(context.Background(), "owner", "Health", true), 409)
	d.discardError = nil
	j.due(session.ID)
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, _ = j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "aborted" || d.publishCalls != 0 {
		t.Fatal("interrupted upload published or cleanup lost")
	}
	_, err := service.Confirm(context.Background(), "owner", session.ID)
	wantStatus(t, err, 410)
}
func TestBrowserCancellationAfterStageDoesNotCancelFinalization(t *testing.T) {
	j := newMemory()
	d := &fakeDriver{applied: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.publishHook = cancel
	indexed := false
	service, session := start(t, j, d, func(ctx context.Context, s Session) (json.RawMessage, error) {
		if ctx.Err() != nil {
			t.Fatal("browser cancellation reached detached indexing")
		}
		indexed = true
		return successfulIndex(ctx, s)
	})
	if _, err := service.Receive(ctx, "owner", session.ID, strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("published object not indexed")
	}
}
func TestAppliedIndexFailureRecoversWithoutAnotherPublication(t *testing.T) {
	j := newMemory()
	d := &fakeDriver{applied: true}
	calls := 0
	service, session := start(t, j, d, func(ctx context.Context, s Session) (json.RawMessage, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("index database unavailable")
		}
		return successfulIndex(ctx, s)
	})
	if _, err := service.Receive(context.Background(), "owner", session.ID, strings.NewReader("abc"), 3); err == nil {
		t.Fatal("index failure hidden")
	}
	stored, _ := j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "applied" {
		t.Fatal("known native application was not retained")
	}
	j.due(session.ID)
	restarted := New("binding", j, d, successfulIndex)
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, _ = j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "completed" || d.publishCalls != 1 {
		t.Fatal("restart republished or failed to index")
	}
}
func TestLostAppliedCheckpointInspectsInsteadOfRepublishing(t *testing.T) {
	j := newMemory()
	j.failSaveState = "applied"
	d := &fakeDriver{applied: true, inspection: Inspection{Applied: true, Terminal: true}}
	service, session := start(t, j, d, successfulIndex)
	if _, err := service.Receive(context.Background(), "owner", session.ID, strings.NewReader("abc"), 3); err == nil {
		t.Fatal("failed applied checkpoint hidden")
	}
	stored, _ := j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "publishing" {
		t.Fatal("durable publishing intent missing")
	}
	j.due(session.ID)
	if err := New("binding", j, d, successfulIndex).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, _ = j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "completed" || d.publishCalls != 1 || d.inspectCalls != 1 {
		t.Fatal("ambiguous publication was blindly repeated")
	}
}
func TestVisibleButNonterminalPublicationKeepsGuard(t *testing.T) {
	j := newMemory()
	d := &fakeDriver{publishError: errors.New("lost native response"), inspection: Inspection{Applied: true, Terminal: false}}
	indexes := 0
	service, session := start(t, j, d, func(ctx context.Context, s Session) (json.RawMessage, error) {
		indexes++
		return successfulIndex(ctx, s)
	})
	_, err := service.Receive(context.Background(), "owner", session.ID, strings.NewReader("abc"), 3)
	wantStatus(t, err, 409)
	if indexes != 1 {
		t.Fatal("known visible native object was not indexed")
	}
	stored, _ := j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "unknown" {
		t.Fatal("visible object incorrectly declared terminal")
	}
	wantStatus(t, service.CheckConflict(context.Background(), "owner", session.Path, false), 409)
	j.due(session.ID)
	if err = service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if indexes != 1 {
		t.Fatal("already repaired uncertain object was indexed repeatedly")
	}
	d.inspection.Terminal = true
	j.due(session.ID)
	if err = service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.publishCalls != 1 || indexes != 1 {
		t.Fatal("uncertain publication repeated or reindexed")
	}
}
func TestReceivingCrashDiscardsInsteadOfPublishing(t *testing.T) {
	j := newMemory()
	d := &fakeDriver{}
	service, session := start(t, j, d, successfulIndex)
	session.State = "receiving"
	session.Checkpoint = json.RawMessage(`{"providerId":"known"}`)
	j.sessions[session.ID] = session
	j.due(session.ID)
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, _ := j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "aborted" || d.publishCalls != 0 || d.discardCalls != 1 {
		t.Fatal("receiving crash published unsent file")
	}
}
func TestNoBodyDoesNotCreateFileOrPollBeforeExpiry(t *testing.T) {
	j := newMemory()
	d := &fakeDriver{}
	service, session := start(t, j, d, successfulIndex)
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.inspectCalls+d.stageCalls+d.publishCalls+d.discardCalls != 0 {
		t.Fatal("unstarted upload performed native work")
	}
	session.ExpiresAt = time.Now().Add(-time.Second)
	j.sessions[session.ID] = session
	j.due(session.ID)
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, _ := j.Get(context.Background(), "binding", "owner", session.ID)
	if stored.State != "aborted" || d.publishCalls != 0 {
		t.Fatal("expired unstarted upload not cleared")
	}
}
func TestOwnerIsolationLengthAndLeaseLoss(t *testing.T) {
	j := newMemory()
	d := &fakeDriver{applied: true}
	service, session := start(t, j, d, successfulIndex)
	_, err := service.Receive(context.Background(), "other", session.ID, strings.NewReader("abc"), 3)
	wantStatus(t, err, 404)
	_, err = service.Receive(context.Background(), "owner", session.ID, strings.NewReader("abc"), 2)
	wantStatus(t, err, 400)
	if d.stageCalls != 0 {
		t.Fatal("invalid body admitted")
	}
	d.stageHook = func() {
		j.mu.Lock()
		s := j.sessions[session.ID]
		s.LeaseToken = "successor"
		j.sessions[session.ID] = s
		j.mu.Unlock()
	}
	_, err = service.Receive(context.Background(), "owner", session.ID, strings.NewReader("abc"), 3)
	wantStatus(t, err, 409)
	if d.publishCalls != 0 {
		t.Fatal("stale receiver published after losing lease")
	}
}
