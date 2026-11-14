package storagebackend

import (
	"context"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/objectmutation"
)

// recordingBackend remembers the keys a measurement touched, so the test can prove
// the probe stayed inside the reserved prefix instead of writing next to a user tree.
type recordingBackend struct {
	objectmutation.Backend
	keys []string
}

func (b *recordingBackend) Put(ctx context.Context, in objectmutation.Put) (objectmutation.Object, error) {
	b.keys = append(b.keys, in.Key)
	return b.Backend.Put(ctx, in)
}

func TestMeasureTransfersTheWholePayloadAndCleansUp(t *testing.T) {
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
	if len(recorder.keys) != 1 || !strings.HasPrefix(recorder.keys[0], probeRoot) {
		t.Fatalf("touched keys = %v, want one under %q", recorder.keys, probeRoot)
	}
	if len(fixture.objects) != 0 {
		t.Fatalf("fixture still holds %d object(s) after the measurement, want the payload deleted", len(fixture.objects))
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
