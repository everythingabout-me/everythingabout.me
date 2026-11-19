package storagebackend

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// recordingBackend remembers the keys a measurement touched, so the test can prove
// the probe stayed inside the reserved prefix instead of writing next to a user tree.
type recordingBackend struct {
	objectmutation.Backend
	mu   sync.Mutex
	keys []string
}

func (b *recordingBackend) Put(ctx context.Context, in objectmutation.Put) (objectmutation.Object, error) {
	b.mu.Lock()
	b.keys = append(b.keys, in.Key)
	b.mu.Unlock()
	return b.Backend.Put(ctx, in)
}

// CopyObject delegates so the wrapped fixture still looks like a real provider to the
// rename phase; embedding an interface would otherwise hide it.
func (b *recordingBackend) CopyObject(ctx context.Context, in objectmutation.Copy) (objectmutation.Object, error) {
	copier, ok := b.Backend.(objectmutation.CopyBackend)
	if !ok {
		return objectmutation.Object{}, storage.NewError("unsupported", storage.Path(in.Target), nil)
	}
	return copier.CopyObject(ctx, in)
}

func (b *recordingBackend) touched() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.keys...)
}

// CopyObject gives the fixture the provider-side copy COS and S3 both have, so the rename
// phase is exercised instead of only its "unsupported" branch.
func (b *probeFixtureBackend) CopyObject(_ context.Context, in objectmutation.Copy) (objectmutation.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	source, ok := b.objects[in.Source]
	if !ok {
		return objectmutation.Object{}, storage.NewError("not_found", storage.Path(in.Source), nil)
	}
	if in.SourceETag == "" || source.etag != in.SourceETag {
		return objectmutation.Object{}, storage.NewError("changed", storage.Path(in.Source), nil)
	}
	if _, exists := b.objects[in.Target]; exists && in.Absent {
		return objectmutation.Object{}, probeFixtureRefusal(in.Target, "ForbidOverwrite", http.StatusPreconditionFailed)
	}
	b.sequence++
	stored := probeFixtureObject{body: source.body, etag: `"fixture-` + itoaFixture(b.sequence) + `"`}
	b.objects[in.Target] = stored
	return probeFixtureObjectResult(in.Target, stored), nil
}

func TestMeasureReportsEveryPhaseAndCleansUp(t *testing.T) {
	fixture := newProbeFixtureBackend(false)
	recorder := &recordingBackend{Backend: fixture}
	measurement, err := measureBackend(context.Background(), "nas", "https://nas.invalid", "backups", recorder)
	if err != nil {
		t.Fatal(err)
	}
	if measurement.Uploaded != measureBytes || measurement.Downloaded != measureBytes {
		t.Fatalf("measured upload/download = %d/%d, want %d each", measurement.Uploaded, measurement.Downloaded, measureBytes)
	}
	if measurement.UploadRate <= 0 || measurement.DownloadRate <= 0 {
		t.Fatalf("rates = %d/%d bytes per second, want both positive", measurement.UploadRate, measurement.DownloadRate)
	}
	if measurement.Latency < 0 {
		t.Fatalf("latency = %s, want a non-negative duration", measurement.Latency)
	}
	if measurement.SmallFiles.Error != "" {
		t.Fatalf("small-file phase failed on a working fixture: %s", measurement.SmallFiles.Error)
	}
	if measurement.SmallFiles.Files != measureSmallFileCount || measurement.SmallFiles.BytesPerFile != measureSmallFileBytes {
		t.Fatalf("small-file phase = %d files of %d bytes, want %d of %d",
			measurement.SmallFiles.Files, measurement.SmallFiles.BytesPerFile, measureSmallFileCount, measureSmallFileBytes)
	}
	if measurement.SmallFiles.PerSecond <= 0 {
		t.Fatalf("small-file rate = %v files per second, want positive", measurement.SmallFiles.PerSecond)
	}
	if measurement.Renames.Error != "" {
		t.Fatalf("rename phase failed on a working fixture: %s", measurement.Renames.Error)
	}
	if measurement.Renames.Files != measureSmallFileCount || measurement.Renames.PerSecond <= 0 {
		t.Fatalf("rename phase = %d files at %v per second, want %d and a positive rate",
			measurement.Renames.Files, measurement.Renames.PerSecond, measureSmallFileCount)
	}
	if measurement.Deletes.Error != "" {
		t.Fatalf("delete phase failed on a working fixture: %s", measurement.Deletes.Error)
	}
	if measurement.Deletes.Files != measureSmallFileCount || measurement.Deletes.PerSecond <= 0 {
		t.Fatalf("delete phase = %d files at %v per second, want %d and a positive rate",
			measurement.Deletes.Files, measurement.Deletes.PerSecond, measureSmallFileCount)
	}
	if measurement.Concurrency.Error != "" {
		t.Fatalf("concurrency phase failed on a working fixture: %s", measurement.Concurrency.Error)
	}
	if measurement.Concurrency.Streams != measureConcurrentStreams || measurement.Concurrency.Rate <= 0 {
		t.Fatalf("concurrency phase = %d streams at %d bytes per second, want %d streams and a positive rate",
			measurement.Concurrency.Streams, measurement.Concurrency.Rate, measureConcurrentStreams)
	}
	// Every object a phase wrote is deleted, including the small batch the rename and
	// delete phases had to reuse and the concurrent writes that are not part of the
	// headline upload.
	if len(fixture.objects) != 0 {
		t.Fatalf("fixture still holds %d object(s) after the measurement, want every payload deleted", len(fixture.objects))
	}
	keys := recorder.touched()
	// One large upload and the small batch are Puts; the rename phase copies provider-side
	// (not a Put) and the concurrent phase writes four more objects.
	if want := 1 + measureSmallFileCount + measureConcurrentStreams; len(keys) != want {
		t.Fatalf("wrote %d object(s), want %d (upload + small files + concurrent streams)", len(keys), want)
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, probeRoot) {
			t.Fatalf("probe wrote key %q outside the reserved prefix %q", key, probeRoot)
		}
	}
}

func TestMeasureReportsAnUnreachableProvider(t *testing.T) {
	fixture := newProbeFixtureBackend(false)
	_, err := measureBackend(context.Background(), "nas", "https://nas.invalid", "backups", &unreachableBackend{Backend: fixture})
	if err == nil {
		t.Fatal("a provider that cannot be reached must fail the measurement")
	}
	for _, want := range []string{"nas", "https://nas.invalid", "backups"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

// unreachableBackend fails the metadata round trip, which is the first thing a
// measurement asks and the point at which "the store is not there" is decided.
type unreachableBackend struct{ objectmutation.Backend }

func (b *unreachableBackend) Head(context.Context, string) (objectmutation.Object, error) {
	return objectmutation.Object{}, context.DeadlineExceeded
}
