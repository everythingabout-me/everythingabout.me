package objectmutation

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// inspectingBackend answers the provider lifecycle question for one fixture. It is an
// operator probe fallback: ordinary discard no longer depends on it, because a definite
// AbortMultipart answer already consumes the whole allocation.
type inspectingBackend struct {
	*nativeTestBackend
	present    bool
	parts      int
	inspectErr error
	inspects   int
}

func (b *inspectingBackend) InspectMultipart(context.Context, string, string) (MultipartState, error) {
	b.inspects++
	if b.inspectErr != nil {
		return MultipartState{}, b.inspectErr
	}
	return MultipartState{Present: b.present, Parts: b.parts}, nil
}

func inspectingFixture(t *testing.T) (*Driver, *inspectingBackend, *memoryUploadMetadata, storage.WriteIntent) {
	t.Helper()
	_, base, binding, options := fixture(t)
	options.Admission.MultipartUploads = true
	native := &nativeTestBackend{fakeBackend: base}
	backend := &inspectingBackend{nativeTestBackend: native}
	driver, err := New(&fakeTree{backend: base, root: binding.ContentRoot}, binding, backend, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { driver.Close() })
	metadata := &memoryUploadMetadata{}
	return driver, backend, metadata, nativeIntent(t, driver, "discard-proof", 4)
}

// uncertainUpload leaves one part receipt permanently uncertain, then discards the
// allocation exactly as a cancelled upload would: the receiver died between its part
// request and the reply, so no acknowledgement can ever confirm that slot.
func uncertainUpload(t *testing.T, driver *Driver, backend *inspectingBackend, metadata *memoryUploadMetadata, in storage.WriteIntent) {
	t.Helper()
	backend.failPart = true
	if _, err := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart); err == nil {
		t.Fatal("lost native reply reported ACK")
	}
	if err := driver.Discard(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
}

func readDiscardProof(t *testing.T, driver *Driver, metadata *memoryUploadMetadata, in storage.WriteIntent) *nativeDiscardProof {
	t.Helper()
	ctx := context.Background()
	var proof *nativeDiscardProof
	err := metadata.WithUploadMetadata(ctx, driver.namespace, in.ID, -1, func(record storage.UploadMetadataRecord) error {
		var e error
		proof, e = readNative[nativeDiscardProof](ctx, record, discardProofKey)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func readDiscardSession(t *testing.T, driver *Driver, metadata *memoryUploadMetadata, in storage.WriteIntent) *nativeSession {
	t.Helper()
	ctx := context.Background()
	var session *nativeSession
	err := metadata.WithUploadMetadata(ctx, driver.namespace, in.ID, -1, func(record storage.UploadMetadataRecord) error {
		var e error
		session, e = readNative[nativeSession](ctx, record, "session")
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("discard left no native session record")
	}
	return session
}

func TestDefiniteAbortIsTerminalForAnUnconfirmablePartReceipt(t *testing.T) {
	driver, backend, metadata, in := inspectingFixture(t)
	// The provider would still report the session if anyone asked. Cleanup must not need
	// to ask: the abort answer itself already consumed this allocation.
	backend.present = true
	backend.parts = 4
	uncertainUpload(t, driver, backend, metadata, in)

	if err := driver.DiscardParts(context.Background(), in, metadata); err != nil {
		t.Fatalf("definite abort left the allocation retained: %v", err)
	}
	if backend.abortCalls != 1 || backend.inspects != 0 {
		t.Fatalf("discard aborted=%d inspected=%d; want 1 abort and no provider probe", backend.abortCalls, backend.inspects)
	}
	session := readDiscardSession(t, driver, metadata, in)
	proof := readDiscardProof(t, driver, metadata, in)
	if proof == nil || proof.UploadID != session.UploadID || proof.Via != "abort" {
		t.Fatalf("unexpected discard evidence: %+v", proof)
	}
	// The recorded evidence is terminal, so a retried cleanup never aborts again.
	if err := driver.DiscardParts(context.Background(), in, metadata); err != nil || backend.abortCalls != 1 {
		t.Fatalf("repeated discard re-aborted: err=%v calls=%d", err, backend.abortCalls)
	}
}

func TestUnconfirmedAbortRetainsLiabilityUntilARetryGetsAnAnswer(t *testing.T) {
	driver, backend, metadata, in := inspectingFixture(t)
	uncertainUpload(t, driver, backend, metadata, in)

	backend.abortErr = errors.New("abort reply lost")
	if err := driver.DiscardParts(context.Background(), in, metadata); err == nil {
		t.Fatal("unknown abort outcome released the allocation")
	}
	if proof := readDiscardProof(t, driver, metadata, in); proof != nil {
		t.Fatalf("unknown abort outcome recorded evidence: %+v", proof)
	}

	backend.abortErr = nil
	if err := driver.DiscardParts(context.Background(), in, metadata); err != nil {
		t.Fatalf("retry with a definite answer did not release: %v", err)
	}
	if backend.abortCalls != 2 {
		t.Fatalf("abort calls=%d; want 2", backend.abortCalls)
	}
	if proof := readDiscardProof(t, driver, metadata, in); proof == nil || proof.Via != "abort" {
		t.Fatalf("retry recorded no abort evidence: %+v", proof)
	}
}

func TestDiscardEvidenceIsBoundToTheObservedUploadSession(t *testing.T) {
	driver, backend, metadata, in := inspectingFixture(t)
	uncertainUpload(t, driver, backend, metadata, in)
	// Evidence for some other session must never release this receipt.
	ctx := context.Background()
	if err := metadata.WithUploadMetadata(ctx, driver.namespace, in.ID, -1, func(record storage.UploadMetadataRecord) error {
		return record.Put(ctx, discardProofKey, encodeNative(&nativeDiscardProof{UploadID: "another-native-upload"}))
	}); err != nil {
		t.Fatal(err)
	}
	if err := driver.DiscardParts(ctx, in, metadata); err != nil {
		t.Fatalf("discard failed to replace stale evidence: %v", err)
	}
	if backend.abortCalls != 1 {
		t.Fatal("stale evidence suppressed the real provider abort")
	}
	session := readDiscardSession(t, driver, metadata, in)
	if proof := readDiscardProof(t, driver, metadata, in); proof == nil || proof.UploadID != session.UploadID {
		t.Fatalf("evidence stayed bound to another session: %+v", proof)
	}
}

// markSessionDiscarded models the ordinary cleanup step that already ran for an
// allocation whose abort answer was lost before the operator was asked to look.
func markSessionDiscarded(t *testing.T, driver *Driver, metadata *memoryUploadMetadata, in storage.WriteIntent) {
	t.Helper()
	ctx := context.Background()
	if err := metadata.WithUploadMetadata(ctx, driver.namespace, in.ID, 0, func(record storage.UploadMetadataRecord) error {
		session, err := readNative[nativeSession](ctx, record, "session")
		if err != nil {
			return err
		}
		if session == nil {
			return fail("not_found", in.Target)
		}
		session.Phase = "discarded"
		return record.Put(ctx, "session", encodeNative(session))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorProofReleasesOnlyAProviderAbsentAllocation(t *testing.T) {
	driver, backend, metadata, in := inspectingFixture(t)
	backend.present = true
	backend.parts = 4
	uncertainUpload(t, driver, backend, metadata, in)
	markSessionDiscarded(t, driver, metadata, in)

	evidence, err := driver.ObserveDiscard(context.Background(), in, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.UploadAbsent || evidence.Parts != 4 {
		t.Fatalf("unexpected provider observation: %+v", evidence)
	}
	if _, err = driver.ObserveDiscard(context.Background(), in, metadata); err != nil {
		t.Fatalf("observe must stay read-only and repeatable: %v", err)
	}
	_, err = driver.ProveDiscard(context.Background(), in, metadata)
	requireCode(t, err, "recovery_required")

	// The provider now reports the session gone: that is the proof the operator records.
	backend.present = false
	backend.parts = 0
	if evidence, err = driver.ProveDiscard(context.Background(), in, metadata); err != nil {
		t.Fatal(err)
	}
	if !evidence.UploadAbsent {
		t.Fatalf("unexpected proof observation: %+v", evidence)
	}
	if proof := readDiscardProof(t, driver, metadata, in); proof == nil || proof.Via != "operator" {
		t.Fatalf("operator probe recorded no evidence: %+v", proof)
	}
	// Proven evidence releases the allocation without another provider abort.
	if err = driver.DiscardParts(context.Background(), in, metadata); err != nil {
		t.Fatalf("proven allocation stayed retained: %v", err)
	}
	if backend.abortCalls != 0 || backend.completeCalls != 0 {
		t.Fatalf("proven discard re-aborted or published: abort=%d complete=%d", backend.abortCalls, backend.completeCalls)
	}
}

func TestDiscardProofRefusesLiveAllocationAndUnprovableProviders(t *testing.T) {
	driver, backend, metadata, in := inspectingFixture(t)
	if _, err := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart); err != nil {
		t.Fatal(err)
	}
	// A live allocation may be observed, but never proven terminal.
	backend.present = false
	if _, err := driver.ObserveDiscard(context.Background(), in, metadata); err != nil {
		t.Fatal(err)
	}
	_, proveErr := driver.ProveDiscard(context.Background(), in, metadata)
	requireCode(t, proveErr, "invalid_transition")

	// Without the provider capability the command must fail closed instead of guessing.
	_, base, binding, options := fixture(t)
	options.Admission.MultipartUploads = true
	plain := &nativeTestBackend{fakeBackend: base}
	unprovable, err := New(&fakeTree{backend: base, root: binding.ContentRoot}, binding, plain, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unprovable.Close() })
	plainIntent := nativeIntent(t, unprovable, "discard-unprovable", 4)
	_, err = unprovable.ProveDiscard(context.Background(), plainIntent, &memoryUploadMetadata{})
	requireCode(t, err, "unsupported")
}
