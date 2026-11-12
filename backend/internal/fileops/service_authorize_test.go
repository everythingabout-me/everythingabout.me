package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// authorizeJournal serves the few calls Service.Write makes before the native effect.
// Embedding the interface makes any settlement call panic, which is what a test wants:
// the point of the case below is that the session is *not* settled.
type authorizeJournal struct {
	Journal
	op       Operation
	released bool
}

func (j *authorizeJournal) Get(context.Context, Root, Actor, string) (Operation, error) {
	return j.op, nil
}

func (j *authorizeJournal) Claim(_ context.Context, _ Root, _ Actor, _ string, _ time.Duration) (Operation, error) {
	j.op.Lease = Lease{OperationID: j.op.ID, Token: "lease", Epoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
	return j.op, nil
}

func (j *authorizeJournal) Renew(_ context.Context, l Lease, _ time.Duration) (Lease, error) {
	return l, nil
}

func (j *authorizeJournal) Release(_ context.Context, l Lease) error {
	if l.Epoch != j.op.Lease.Epoch {
		return E("lease_lost", nil)
	}
	j.released = true
	return nil
}

type stagedWriterStub struct{ binding storage.Config }

func (w stagedWriterStub) StorageBinding() storage.Config { return w.binding }
func (w stagedWriterStub) StorageNameRules() storage.NameRules {
	return storage.NameRules{CaseSensitive: true}
}
func (stagedWriterStub) Stage(context.Context, storage.WriteIntent, io.Reader, storage.Checkpoint) (storage.Staged, error) {
	return storage.Staged{}, errors.New("stage is not part of this case")
}
func (stagedWriterStub) Publish(context.Context, storage.Staged, storage.Checkpoint) (storage.MutationResult, error) {
	return storage.MutationResult{}, errors.New("publish is not part of this case")
}
func (stagedWriterStub) Inspect(context.Context, storage.WriteIntent, []byte) (storage.Inspection, error) {
	return storage.Inspection{}, errors.New("inspect is not part of this case")
}
func (stagedWriterStub) Discard(context.Context, storage.WriteIntent, []byte) error {
	return errors.New("discard is not part of this case")
}

// A provider failure while re-checking the destination must not end the session. The
// direct writer's client has already published the object, so failing here discarded a
// file that exists and answered 412 — the one answer that tells the queue to stop.
func TestWriteKeepsTheSessionWhenTheObservationFails(t *testing.T) {
	root := Root{OwnerID: "owner", ConnectionID: "connection", Generation: 1, ContentRoot: "content", SystemRoot: "system", CaseSensitive: true}
	intent := storage.WriteIntent{Target: "trip/a.txt", Size: 5, Conditions: storage.Conditions{Absent: true}}
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	j := &authorizeJournal{op: Operation{
		ID: "op", Kind: "upload_commit", Root: root, PrincipalID: "principal",
		State: Running, Phase: StagedReady, Intent: encoded,
		Stage: &storage.Staged{Size: 5, Handle: []byte(`native-etag:"x"`), Intent: intent},
	}}
	authorizations := 0
	service := &Service{journal: j, options: ServiceOptions{
		LeaseTTL: time.Hour,
		Authorize: func(context.Context, Operation) error {
			authorizations++
			if authorizations == 1 {
				return nil
			}
			return E("storage_failure", storage.NewProviderError("backend_error", "", 503, "SlowDown", "req-42", errors.New("body")))
		},
	}}
	op, err := service.Write(context.Background(), root, Actor{ID: "principal", CanRead: true, CanWrite: true}, "op", stagedWriterStub{binding: storage.Config{OwnerID: root.OwnerID, ConnectionID: root.ConnectionID, Generation: root.Generation, ContentRoot: root.ContentRoot, SystemRoot: root.SystemRoot}}, nil)
	if !IsCode(err, "storage_failure") {
		t.Fatalf("got %v, want a retryable storage failure", err)
	}
	if j.released == false {
		t.Fatal("the lease was kept, so every retry would answer operation_in_progress")
	}
	if op.Terminal() {
		t.Fatalf("the session ended as %s, losing the publication the client already made", op.State)
	}
}
