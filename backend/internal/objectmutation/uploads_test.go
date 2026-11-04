package objectmutation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

type memoryUploadMetadata struct {
	mu     sync.Mutex
	values map[string][]byte
}
type memoryUploadRecord struct {
	store  *memoryUploadMetadata
	prefix string
}

func (m *memoryUploadMetadata) WithUploadMetadata(ctx context.Context, binding, id string, _ int, fn func(storage.UploadMetadataRecord) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string][]byte{}
	}
	return fn(&memoryUploadRecord{m, binding + ":" + id + ":"})
}
func (r *memoryUploadRecord) Get(_ context.Context, key string) ([]byte, error) {
	return bytes.Clone(r.store.values[r.prefix+key]), nil
}
func (r *memoryUploadRecord) Put(_ context.Context, key string, value []byte) error {
	r.store.values[r.prefix+key] = bytes.Clone(value)
	return nil
}

type nativeTestBackend struct {
	*fakeBackend
	parts                                            map[int][]byte
	beginCalls, partCalls, completeCalls, abortCalls int
	failPart, failComplete                           bool
	abortErr                                         error
	onPart                                           func()
}

func (b *nativeTestBackend) BeginMultipart(context.Context, Put) (string, error) {
	b.beginCalls++
	return "private-native-upload", nil
}
func (b *nativeTestBackend) PutPart(_ context.Context, _ string, _ string, n int, body io.Reader, size int64) (string, error) {
	b.partCalls++
	if b.onPart != nil {
		b.onPart()
	}
	data, e := io.ReadAll(body)
	if e != nil {
		return "", e
	}
	if int64(len(data)) != size {
		return "", fmt.Errorf("size mismatch")
	}
	if b.parts == nil {
		b.parts = map[int][]byte{}
	}
	b.parts[n] = data
	if b.failPart {
		b.failPart = false
		return "", io.ErrUnexpectedEOF
	}
	return fmt.Sprintf("tag-%d", n), nil
}
func (b *nativeTestBackend) CompleteMultipart(ctx context.Context, in Put, _ string, parts []NativePart) (Object, error) {
	b.completeCalls++
	var data []byte
	for _, p := range parts {
		data = append(data, b.parts[p.Number]...)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	old, exists := b.current[in.Key]
	if in.Absent && exists || in.ExpectedETag != "" && (!exists || old.ETag != in.ExpectedETag) {
		return Object{}, fail("precondition_failed", "")
	}
	out := Object{Key: in.Key, Size: int64(len(data)), VersionID: "complete-version", ETag: "complete-tag", MediaType: in.MediaType, Metadata: in.Metadata}
	b.current[in.Key] = out
	b.versions[in.Key] = map[string][]byte{out.VersionID: data}
	if b.failComplete {
		return Object{}, io.ErrUnexpectedEOF
	}
	return out, nil
}
func (b *nativeTestBackend) AbortMultipart(context.Context, string, string) error {
	b.abortCalls++
	return b.abortErr
}
func nativeFixture(t *testing.T) (*Driver, *nativeTestBackend, storage.Config, Options) {
	_, base, binding, options := fixture(t)
	options.Admission.MultipartUploads = true
	backend := &nativeTestBackend{fakeBackend: base}
	driver, err := New(&fakeTree{backend: base, root: binding.ContentRoot}, binding, backend, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { driver.Close() })
	return driver, backend, binding, options
}
func nativeIntent(t *testing.T, d *Driver, id string, size int64) storage.WriteIntent {
	t.Helper()
	parent, e := d.Stat(context.Background(), "")
	if e != nil {
		t.Fatal(e)
	}
	return storage.WriteIntent{ID: id, Target: storage.Path(id), Size: size, Conditions: storage.Conditions{Absent: true}, ParentVersion: parent.Version, MediaType: "application/octet-stream"}
}
func keepPart(part storage.UploadPart) (storage.UploadPart, error) { return part, nil }

func TestNativeNumberedPartsIndependentReceiverSealAndNoWholeFilePut(t *testing.T) {
	receiver, backend, binding, options := nativeFixture(t)
	options.SpoolRoot = t.TempDir()
	app, err := New(&fakeTree{backend: backend.fakeBackend, root: binding.ContentRoot}, binding, backend, options)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	metadata := &memoryUploadMetadata{}
	in := nativeIntent(t, receiver, "large", nativePartSize+4)
	first := bytes.Repeat([]byte("a"), int(nativePartSize))
	tail, e := receiver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 2, Size: 4}, bytes.NewReader([]byte("tail")), metadata, keepPart)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := backend.Head(context.Background(), binding.ContentRoot+string(in.Target)); !storage.IsCode(e, "not_found") {
		t.Fatal("part prematurely published")
	}
	_, e = receiver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: nativePartSize}, bytes.NewReader(first), metadata, func(p storage.UploadPart) (storage.UploadPart, error) { return p, errors.New("receipt ACK lost") })
	if e == nil {
		t.Fatal("lost receipt not surfaced")
	}
	receipt, e := receiver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: nativePartSize}, bytes.NewReader(first), metadata, keepPart)
	if e != nil {
		t.Fatal(e)
	}
	if backend.partCalls != 2 {
		t.Fatal("durable native receipt resent bytes", backend.partCalls)
	}
	_, e = receiver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: nativePartSize}, bytes.NewReader(bytes.Repeat([]byte("z"), int(nativePartSize))), metadata, keepPart)
	requireCode(t, e, "part_conflict")
	stage, e := app.SealStage(context.Background(), in, []storage.UploadPart{receipt, tail}, metadata, cpOK)
	if e != nil {
		t.Fatal(e)
	}
	if stage.Digest != nil {
		t.Fatal("part hashes fabricated a whole-file checksum")
	}
	result, e := app.Publish(context.Background(), stage, cpOK)
	if e != nil {
		t.Fatal(e)
	}
	if result.Entry.Size != in.Size || backend.puts != 0 || backend.completeCalls != 1 {
		t.Fatalf("whole-file PUT occurred puts%d completes%d", backend.puts, backend.completeCalls)
	}
	if e = app.Discard(context.Background(), in, result.Evidence); e != nil {
		t.Fatal(e)
	}
	if e = app.DiscardParts(context.Background(), in, metadata); e != nil {
		t.Fatal(e)
	}
}

// A part body is forwarded to the provider while it is still arriving: the provider is
// entered before the request ends, which is what lets the receiver hold no payload.
func TestNativePartStreamsIntoProviderWhileBodyStillArrives(t *testing.T) {
	driver, backend, _, _ := nativeFixture(t)
	metadata := &memoryUploadMetadata{}
	in := nativeIntent(t, driver, "streamed", 4)
	entered := make(chan struct{})
	backend.onPart = func() { close(entered) }
	body, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("da"))
		<-entered
		_, _ = writer.Write([]byte("ta"))
		_ = writer.Close()
	}()
	accepted := make(chan error, 1)
	go func() {
		_, e := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, body, metadata, keepPart)
		accepted <- e
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		_ = writer.Close()
		t.Fatal("the provider was not reached until the whole part had arrived")
	}
	if e := <-accepted; e != nil {
		t.Fatal(e)
	}
	if string(backend.parts[1]) != "data" {
		t.Fatalf("provider received %q", backend.parts[1])
	}
}

func TestNativePartTruncatedStreamIsASizeMismatchThatStaysUnconfirmed(t *testing.T) {
	driver, backend, _, _ := nativeFixture(t)
	metadata := &memoryUploadMetadata{}
	in := nativeIntent(t, driver, "short-part", 4)
	_, e := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("da")), metadata, keepPart)
	requireCode(t, e, "size_mismatch")
	if backend.parts[1] != nil {
		t.Fatal("a truncated part was recorded")
	}
	receipt, e := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart)
	if e != nil {
		t.Fatal(e)
	}
	if receipt.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte("data"))) || receipt.Handle == "" {
		t.Fatalf("retry did not confirm the complete part: %+v", receipt)
	}
}

func TestNativePartReservationBindsSlotAndConfirmationBindsContent(t *testing.T) {
	driver, backend, _, _ := nativeFixture(t)
	metadata := &memoryUploadMetadata{}
	in := nativeIntent(t, driver, "part-lost", 4)
	backend.failPart = true
	if _, e := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart); e == nil {
		t.Fatal("lost native reply reported ACK")
	}
	if backend.partCalls != 1 {
		t.Fatalf("uncertain receive never reached the provider: %d", backend.partCalls)
	}
	// A streamed part has no digest before its bytes arrive, so the failed attempt reserved
	// the slot by number, size, and session only. The next replay owns the bytes and is
	// recorded as the confirmed content.
	receipt, e := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("evil")), metadata, keepPart)
	if e != nil {
		t.Fatal(e)
	}
	if receipt.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte("evil"))) || string(backend.parts[1]) != "evil" {
		t.Fatalf("receipt %+v does not describe the stored bytes %q", receipt, backend.parts[1])
	}
	// Once confirmed, the recorded digest fences the slot: different content is rejected
	// before it can reach the provider again.
	_, e = driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart)
	requireCode(t, e, "part_conflict")
	if backend.partCalls != 2 {
		t.Fatal("different content reached a confirmed native slot")
	}
	if e = driver.Discard(context.Background(), in, nil); e != nil {
		t.Fatal(e)
	}
	if e = driver.DiscardParts(context.Background(), in, metadata); e != nil {
		t.Fatalf("definite abort must release an unconfirmable receipt: %v", e)
	}
	_, e = driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart)
	requireCode(t, e, "upload_aborted")
	if backend.completeCalls != 0 || backend.abortCalls != 1 {
		t.Fatal("aborted upload published or cleanup omitted")
	}
	// The recorded abort evidence is terminal: a retried cleanup must not abort again.
	if e = driver.DiscardParts(context.Background(), in, metadata); e != nil || backend.abortCalls != 1 {
		t.Fatalf("repeated discard re-aborted: %v calls=%d", e, backend.abortCalls)
	}
}

func TestNativeCompletionLostReplyNeverReplaysOrReleasesGuard(t *testing.T) {
	driver, backend, _, _ := nativeFixture(t)
	metadata := &memoryUploadMetadata{}
	in := nativeIntent(t, driver, "complete-lost", 4)
	part, e := driver.ReceivePart(context.Background(), in, storage.UploadPart{Number: 1, Size: 4}, bytes.NewReader([]byte("data")), metadata, keepPart)
	if e != nil {
		t.Fatal(e)
	}
	stage, e := driver.SealStage(context.Background(), in, []storage.UploadPart{part}, metadata, cpOK)
	if e != nil {
		t.Fatal(e)
	}
	backend.failComplete = true
	var checkpoint []byte
	_, e = driver.Publish(context.Background(), stage, func(_ context.Context, b []byte) error { checkpoint = bytes.Clone(b); return nil })
	if e == nil {
		t.Fatal("lost Complete reply implied success")
	}
	inspection, e := driver.Inspect(context.Background(), in, checkpoint)
	if e != nil || inspection.State != storage.OutcomeUnknown {
		t.Fatalf("unknown result not guarded %+v %v", inspection, e)
	}
	_, e = driver.Publish(context.Background(), stage, cpOK)
	requireCode(t, e, "outcome_unknown")
	requireCode(t, driver.Discard(context.Background(), in, checkpoint), "outcome_unknown")
	if backend.completeCalls != 1 || backend.puts != 0 {
		t.Fatal("native effect automatically replayed")
	}
}
