package objectmutation

import (
	"bytes"
	"context"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// inspectingBackend answers the provider lifecycle question for one fixture. A session
// the provider no longer knows is the only observation that releases retained liability.
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
// allocation exactly as a cancelled upload would.
func uncertainUpload(t *testing.T, driver *Driver, backend *inspectingBackend, metadata *memoryUploadMetadata, in storage.WriteIntent) {
	t.Helper()
	backend.failPart = true
	if _, err := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart); err == nil {
		t.Fatal("lost native reply reported ACK")
	}
	if err := driver.Discard(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
	requireCode(t, driver.DiscardParts(context.Background(), in, metadata), "recovery_required")
}

func TestDiscardProofReleasesOnlyAProviderAbsentAllocation(t *testing.T) {
	driver, backend, metadata, in := inspectingFixture(t)
	backend.present = true
	backend.parts = 4
	uncertainUpload(t, driver, backend, metadata, in)

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
	requireCode(t, driver.DiscardParts(context.Background(), in, metadata), "recovery_required")

	// The provider now reports the session gone: that is the proof the operator records.
	backend.present = false
	backend.parts = 0
	if evidence, err = driver.ProveDiscard(context.Background(), in, metadata); err != nil {
		t.Fatal(err)
	}
	if !evidence.UploadAbsent {
		t.Fatalf("unexpected proof observation: %+v", evidence)
	}
	if err = driver.DiscardParts(context.Background(), in, metadata); err != nil {
		t.Fatalf("proven allocation stayed retained: %v", err)
	}
	if backend.completeCalls != 0 {
		t.Fatal("reconciliation published or completed native content")
	}
}

func TestDiscardProofIsBoundToTheObservedUploadSession(t *testing.T) {
	driver, backend, metadata, in := inspectingFixture(t)
	backend.present = true
	uncertainUpload(t, driver, backend, metadata, in)
	// A proof for some other session must never release this receipt.
	ctx := context.Background()
	if err := metadata.WithUploadMetadata(ctx, driver.namespace, in.ID, -1, func(record storage.UploadMetadataRecord) error {
		return record.Put(ctx, discardProofKey, encodeNative(&nativeDiscardProof{UploadID: "another-native-upload"}))
	}); err != nil {
		t.Fatal(err)
	}
	backend.present = false
	requireCode(t, driver.DiscardParts(ctx, in, metadata), "recovery_required")
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
