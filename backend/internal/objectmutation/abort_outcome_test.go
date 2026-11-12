package objectmutation

import (
	"bytes"
	"context"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// finishedUpload leaves one sealed part session and a target a lost reply may have
// published, without making the publication itself successful.
func finishedUpload(t *testing.T, driver *Driver, backend *nativeTestBackend, metadata *memoryUploadMetadata, in storage.WriteIntent, drop bool) []byte {
	t.Helper()
	part, err := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := driver.SealStage(context.Background(), in, []storage.UploadPart{part}, metadata, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	backend.dropComplete = drop
	var checkpoint []byte
	if _, err = driver.Publish(context.Background(), stage, func(_ context.Context, b []byte) error {
		checkpoint = bytes.Clone(b)
		return nil
	}); err == nil {
		t.Fatal("a lost Complete reply implied success")
	}
	return checkpoint
}

// A completed upload removes the provider session just as an abort does, so NoSuchUpload
// cannot be recorded as "nothing applied". The reconcile path must observe the
// destination and attribute an object carrying this operation's own intent marker.
func TestAbsentAbortAfterCompletedUploadIsAttributed(t *testing.T) {
	driver, backend, binding, _ := nativeFixture(t)
	metadata := &memoryUploadMetadata{}
	in := nativeIntent(t, driver, "completed-race", 4)
	checkpoint := finishedUpload(t, driver, backend, metadata, in, true)

	// The completion lands only after the inspection's first HEAD observed an empty
	// target, and it removes the session; the abort that follows answers NoSuchUpload.
	target := binding.ContentRoot + string(in.Target)
	backend.abortErr = storage.NewError("abort_session_absent", in.Target, nil)
	backend.onHead = func(key string) {
		if key != target {
			t.Fatalf("inspected %q, want the target %q", key, target)
		}
		backend.mu.Lock()
		defer backend.mu.Unlock()
		backend.current[target] = Object{Key: target, VersionID: "completed-version", ETag: "completed-tag", Size: in.Size, MediaType: in.MediaType, Metadata: map[string]string{"eam-intent": in.ID}}
	}

	inspection, err := driver.Inspect(context.Background(), in, checkpoint)
	if err != nil || inspection.State != storage.Applied || inspection.Result == nil || inspection.Result.Entry == nil {
		t.Fatalf("a completion behind a vanished session was recorded as %+v (%v)", inspection, err)
	}
	if inspection.Result.Entry.Path != in.Target || inspection.Result.Entry.Size != in.Size {
		t.Fatalf("attributed entry does not describe the target: %+v", inspection.Result.Entry)
	}
	if backend.abortCalls != 1 {
		t.Fatalf("aborted %d times; want the one absent answer", backend.abortCalls)
	}
	recorded, err := driver.load("write", in.ID)
	if err != nil || recorded.Phase != "applied" || recorded.Unpublished {
		t.Fatalf("the published object was not recorded applied: %+v %v", recorded, err)
	}
	// A later reconcile reads the settled record instead of aborting again.
	settled, err := driver.Inspect(context.Background(), in, inspection.Evidence)
	if err != nil || settled.State != storage.Applied || backend.abortCalls != 1 {
		t.Fatalf("reconcile lost the attribution: %+v %v aborts=%d", settled, err, backend.abortCalls)
	}
}

// Cleanup of an upload whose Complete succeeded sees the same NoSuchUpload as an abort
// that won. It must record the publication it can attribute, never "nothing applied".
func TestDiscardAfterCompletedUploadRecordsThePublication(t *testing.T) {
	driver, backend, binding, _ := nativeFixture(t)
	metadata := &memoryUploadMetadata{}
	in := nativeIntent(t, driver, "completed-cleanup", 4)
	// This completion succeeds normally, so the object is published and the session is
	// consumed by Complete. Cleanup then aborts a session the provider no longer knows.
	part, err := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := driver.SealStage(context.Background(), in, []storage.UploadPart{part}, metadata, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	result, err := driver.Publish(context.Background(), stage, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	backend.abortErr = storage.NewError("abort_session_absent", in.Target, nil)
	if err = driver.Discard(context.Background(), in, result.Evidence); err != nil {
		t.Fatal(err)
	}
	if err = driver.DiscardParts(context.Background(), in, metadata); err != nil {
		t.Fatalf("cleanup of a completed upload retained the allocation: %v", err)
	}
	session := readDiscardSession(t, driver, metadata, in)
	proof := readDiscardProof(t, driver, metadata, in)
	if proof == nil || proof.UploadID != session.UploadID || proof.Via != "abort-session-absent" || !proof.Published {
		t.Fatalf("a completed publication was recorded as an unpublished discard: %+v", proof)
	}
	if recorded, loadErr := driver.load("write", in.ID); loadErr != nil || recorded.Phase != "applied" || recorded.Unpublished {
		t.Fatalf("discard erased the applied record: %+v %v", recorded, loadErr)
	}
	if _, err = backend.Head(context.Background(), binding.ContentRoot+string(in.Target)); err != nil {
		t.Fatalf("discard removed the published object: %v", err)
	}
	// The recorded evidence is terminal, so a retried cleanup never aborts again.
	if err = driver.DiscardParts(context.Background(), in, metadata); err != nil || backend.abortCalls != 1 {
		t.Fatalf("repeated cleanup re-aborted: err=%v calls=%d", err, backend.abortCalls)
	}
}
