package objectmutation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// copyFake applies provider-side copies to the same in-memory object map the
// relay path uses, so a test can prove no bytes crossed the gateway.
type copyFake struct {
	*fakeBackend
	mu             sync.Mutex
	copies         int
	parts          []int
	aborts         int
	completed      bool
	copyErr        error
	partErr        error
	failBeforeCopy bool
}

func (c *copyFake) CopyObject(ctx context.Context, in Copy) (Object, error) {
	c.mu.Lock()
	c.copies++
	failBefore, err := c.failBeforeCopy, c.copyErr
	c.mu.Unlock()
	if failBefore {
		return Object{}, err
	}
	c.fakeBackend.mu.Lock()
	source, ok := c.fakeBackend.current[in.Source]
	if !ok {
		c.fakeBackend.mu.Unlock()
		return Object{}, fail("not_found", "")
	}
	if source.ETag != in.SourceETag && source.VersionID != in.SourceETag {
		c.fakeBackend.mu.Unlock()
		return Object{}, fail("precondition_failed", "")
	}
	if _, exists := c.fakeBackend.current[in.Target]; exists && in.Absent {
		c.fakeBackend.mu.Unlock()
		return Object{}, fail("precondition_failed", "")
	}
	object := Object{Key: in.Target, VersionID: "copy-version", ETag: source.ETag, Size: in.Size, ModifiedAt: time.Unix(1_800_000_000, 0), MediaType: in.MediaType, Metadata: in.Metadata}
	c.fakeBackend.current[in.Target] = object
	c.fakeBackend.mu.Unlock()
	return object, err
}

func (c *copyFake) BeginMultipart(context.Context, Put) (string, error) {
	return "copy-upload", nil
}
func (c *copyFake) PutPart(context.Context, string, string, int, io.Reader, int64) (string, error) {
	return "", fail("unsupported", "")
}
func (c *copyFake) CopyPart(_ context.Context, key, uploadID string, number int, source, sourceETag string, offset, length int64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.partErr; err != nil {
		return "", err
	}
	if key == "" || uploadID != "copy-upload" || source == "" || sourceETag == "" || offset < 0 || length <= 0 {
		return "", fail("invalid_request", "")
	}
	c.parts = append(c.parts, number)
	return fmt.Sprintf("part-%d", number), nil
}
func (c *copyFake) CompleteMultipart(_ context.Context, in Put, uploadID string, parts []NativePart) (Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if uploadID != "copy-upload" || len(parts) != len(c.parts) {
		return Object{}, fail("invalid_request", "")
	}
	c.completed = true
	object := Object{Key: in.Key, VersionID: "copy-version", ETag: "etag-multipart", Size: in.Size, ModifiedAt: time.Unix(1_800_000_000, 0), MediaType: in.MediaType, Metadata: in.Metadata}
	c.fakeBackend.mu.Lock()
	c.fakeBackend.current[in.Key] = object
	c.fakeBackend.mu.Unlock()
	return object, nil
}
func (c *copyFake) AbortMultipart(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.aborts++
	return nil
}

func copyFixture(t *testing.T) (*Driver, *copyFake, storage.Config) {
	t.Helper()
	backend := &copyFake{fakeBackend: newFake()}
	cfg := storage.Config{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "drive/owner/", SystemRoot: "system/owner/"}
	options := Options{SpoolRoot: t.TempDir(), MaxPublishBytes: 1024, Admission: Admission{ConnectionID: cfg.ConnectionID, Endpoint: "https://fixture.invalid", Bucket: "bucket", Region: "region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated-positive-negative-test", AtomicPublish: true, ConditionalCreate: true, SingleGateway: true, MultipartUploads: true}}
	d, err := New(&fakeTree{backend: backend.fakeBackend, root: cfg.ContentRoot}, cfg, backend, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, backend, cfg
}

func publishSource(t *testing.T, d *Driver, id, path, data string) storage.Entry {
	t.Helper()
	stage, err := d.Stage(context.Background(), createIntent(id, path, data), strings.NewReader(data), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.Publish(context.Background(), stage, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	return *result.Entry
}

func TestProviderCopyMovesNoBytes(t *testing.T) {
	d, backend, cfg := copyFixture(t)
	ctx := context.Background()
	source := publishSource(t, d, "source", "source.bin", "provider-side payload")
	before := backend.puts
	copied, err := d.Copy(ctx, storage.MoveRequest{ID: "copy", Source: "source.bin", Target: "copy.bin", SourceRevision: source.Revision, TargetConditions: storage.Conditions{Absent: true}}, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	if copied.Entry.Path != "copy.bin" || copied.Entry.Size != source.Size {
		t.Fatalf("copy result=%+v", copied.Entry)
	}
	if backend.copies != 1 || backend.puts != before {
		t.Fatalf("copies=%d puts=%d; the gateway published bytes", backend.copies, backend.puts)
	}
	object, err := backend.Head(ctx, cfg.ContentRoot+"copy.bin")
	if err != nil {
		t.Fatal(err)
	}
	if object.Metadata["eam-intent"] != "copy" {
		t.Fatalf("copy target lost its intent marker: %+v", object.Metadata)
	}
}

func TestProviderCopyRefusesOccupiedTarget(t *testing.T) {
	d, backend, cfg := copyFixture(t)
	ctx := context.Background()
	source := publishSource(t, d, "source", "source.bin", "payload")
	backend.current[cfg.ContentRoot+"taken.bin"] = Object{Key: cfg.ContentRoot + "taken.bin", VersionID: "v-taken", ETag: "etag-taken", Size: 3}
	_, err := d.Copy(ctx, storage.MoveRequest{ID: "copy", Source: "source.bin", Target: "taken.bin", SourceRevision: source.Revision, TargetConditions: storage.Conditions{Absent: true}}, cpOK)
	requireCode(t, err, "precondition_failed")
	object, err := backend.Head(ctx, cfg.ContentRoot+"taken.bin")
	if err != nil || object.ETag != "etag-taken" {
		t.Fatalf("copy replaced an occupied target: %+v %v", object, err)
	}
}

func TestProviderCopyBindsSourceRevision(t *testing.T) {
	d, backend, _ := copyFixture(t)
	ctx := context.Background()
	publishSource(t, d, "source", "source.bin", "payload")
	_, err := d.Copy(ctx, storage.MoveRequest{ID: "copy", Source: "source.bin", Target: "copy.bin", SourceRevision: nativeRevision("stale-etag"), TargetConditions: storage.Conditions{Absent: true}}, cpOK)
	requireCode(t, err, "changed")
	if backend.copies != 0 {
		t.Fatal("stale source reached the provider")
	}
}

func TestProviderCopyLostReplyResolvesFromIntent(t *testing.T) {
	d, backend, cfg := copyFixture(t)
	ctx := context.Background()
	source := publishSource(t, d, "source", "source.bin", "payload")
	backend.copyErr = io.ErrUnexpectedEOF
	request := storage.MoveRequest{ID: "copy", Source: "source.bin", Target: "copy.bin", SourceRevision: source.Revision, TargetConditions: storage.Conditions{Absent: true}}
	if _, err := d.Copy(ctx, request, cpOK); err == nil {
		t.Fatal("lost reply reported success")
	}
	backend.copyErr = nil
	inspection, err := d.InspectEffect(ctx, storage.EffectIntent{ID: "copy", Action: "copy", Source: "source.bin", Target: "copy.bin", Conditions: storage.Conditions{Absent: true}, SourceRevision: source.Revision}, nil)
	if err != nil || inspection.State != storage.Applied {
		t.Fatalf("inspection=%+v %v", inspection, err)
	}
	if backend.copies != 1 {
		t.Fatal("recovery replayed the copy")
	}
	if _, err := backend.Head(ctx, cfg.ContentRoot+"copy.bin"); err != nil {
		t.Fatal("recovery lost the published copy")
	}
}

func TestProviderCopyFailureWithoutEffectIsNotApplied(t *testing.T) {
	d, backend, _ := copyFixture(t)
	ctx := context.Background()
	source := publishSource(t, d, "source", "source.bin", "payload")
	backend.failBeforeCopy = true
	backend.copyErr = io.ErrUnexpectedEOF
	request := storage.MoveRequest{ID: "copy", Source: "source.bin", Target: "copy.bin", SourceRevision: source.Revision, TargetConditions: storage.Conditions{Absent: true}}
	if _, err := d.Copy(ctx, request, cpOK); err == nil {
		t.Fatal("failed copy reported success")
	}
	inspection, err := d.InspectEffect(ctx, storage.EffectIntent{ID: "copy", Action: "copy", Source: "source.bin", Target: "copy.bin", Conditions: storage.Conditions{Absent: true}, SourceRevision: source.Revision}, nil)
	if err != nil || inspection.State != storage.NotApplied {
		t.Fatalf("inspection=%+v %v", inspection, err)
	}
}

func TestProviderCopyAboveSingleRequestLimitUsesParts(t *testing.T) {
	d, backend, cfg := copyFixture(t)
	ctx := context.Background()
	const size int64 = 6 << 30
	source := Object{Key: cfg.ContentRoot + "big.bin", VersionID: "v-big", ETag: "etag-big", Size: size, MediaType: "video/mp4"}
	backend.current[source.Key] = source
	copied, err := d.Copy(ctx, storage.MoveRequest{ID: "copy", Source: "big.bin", Target: "big-copy.bin", SourceRevision: nativeRevision(source.ETag), TargetConditions: storage.Conditions{Absent: true}}, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	if backend.copies != 0 || !backend.completed {
		t.Fatalf("copies=%d completed=%v", backend.copies, backend.completed)
	}
	want := int((size + copyPartBytes - 1) / copyPartBytes)
	if len(backend.parts) != want || copied.Entry.Size != size {
		t.Fatalf("parts=%d want=%d size=%d", len(backend.parts), want, copied.Entry.Size)
	}
	if backend.puts != 0 {
		t.Fatal("large copy published through the gateway")
	}
}

func TestProviderCopyPartFailureAbortsAndReportsNoEffect(t *testing.T) {
	d, backend, cfg := copyFixture(t)
	ctx := context.Background()
	const size int64 = 6 << 30
	source := Object{Key: cfg.ContentRoot + "big.bin", VersionID: "v-big", ETag: "etag-big", Size: size, MediaType: "video/mp4"}
	backend.current[source.Key] = source
	backend.partErr = io.ErrUnexpectedEOF
	_, err := d.Copy(ctx, storage.MoveRequest{ID: "copy", Source: "big.bin", Target: "big-copy.bin", SourceRevision: nativeRevision(source.ETag), TargetConditions: storage.Conditions{Absent: true}}, cpOK)
	var noEffect *storage.NoFinalEffect
	if !errors.As(err, &noEffect) {
		t.Fatalf("aborted part copy reported %v", err)
	}
	if backend.aborts != 1 || backend.completed {
		t.Fatalf("aborts=%d completed=%v", backend.aborts, backend.completed)
	}
}

func TestProviderCopyWithoutMultipartAdmissionRefusesLargeObject(t *testing.T) {
	backend := &copyFake{fakeBackend: newFake()}
	cfg := storage.Config{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "drive/owner/", SystemRoot: "system/owner/"}
	options := Options{SpoolRoot: t.TempDir(), MaxPublishBytes: 1024, Admission: Admission{ConnectionID: cfg.ConnectionID, Endpoint: "https://fixture.invalid", Bucket: "bucket", Region: "region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated-positive-negative-test", AtomicPublish: true, ConditionalCreate: true, SingleGateway: true}}
	d, err := New(&fakeTree{backend: backend.fakeBackend, root: cfg.ContentRoot}, cfg, backend, options)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	source := Object{Key: cfg.ContentRoot + "big.bin", VersionID: "v-big", ETag: "etag-big", Size: 6 << 30, MediaType: "video/mp4"}
	backend.current[source.Key] = source
	_, err = d.Copy(context.Background(), storage.MoveRequest{ID: "copy", Source: "big.bin", Target: "big-copy.bin", SourceRevision: nativeRevision(source.ETag), TargetConditions: storage.Conditions{Absent: true}}, cpOK)
	var noEffect *storage.NoFinalEffect
	if !errors.As(err, &noEffect) || !storage.IsCode(err, "too_large") {
		t.Fatalf("oversized copy reported %v", err)
	}
	if backend.completed || backend.aborts != 0 {
		t.Fatal("refusal still started a part session")
	}
}

func TestProviderCopyInspectionReportsApplied(t *testing.T) {
	d, _, _ := copyFixture(t)
	ctx := context.Background()
	source := publishSource(t, d, "source", "source.bin", "payload")
	copied, err := d.Copy(ctx, storage.MoveRequest{ID: "copy", Source: "source.bin", Target: "copy.bin", SourceRevision: source.Revision, TargetConditions: storage.Conditions{Absent: true}}, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := d.InspectEffect(ctx, storage.EffectIntent{ID: "copy", Action: "copy", Source: "source.bin", Target: "copy.bin", Conditions: storage.Conditions{Absent: true}, SourceRevision: source.Revision}, copied.Evidence)
	if err != nil || inspection.State != storage.Applied || inspection.Result == nil || inspection.Result.Entry.Path != "copy.bin" {
		t.Fatalf("inspection=%+v %v", inspection, err)
	}
}
