package storagebackend

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// Every number a speed test reports is one phase with a fixed amount of work. The work,
// never a clock, bounds each phase: a slow link must complete its measurement instead of
// being cut off, and two runs are comparable because they did the same work.
const (
	// measureBytes is the large-file payload: 100 MB, decimal, because that is the number
	// a person reads next to the rate. It is a single conditional PUT — the same kind of
	// write a large file gets on a connection whose probe measured conditional writes —
	// and reading it back is the download half.
	measureBytes = 100_000_000

	// measureLatencySamples is how many metadata round trips the latency row reports
	// the median of. One sample would report a lucky warm connection as the distance to
	// the provider; three is the fewest that has a median at all.
	measureLatencySamples = 3

	// The small-file phase is 100 files of 1 KB: a count a backup of a photo library or a
	// code tree actually produces, and a size at which the cost is one round trip per
	// file rather than bytes per second. The rate it reports is files per second, which
	// is the unit that says whether a nightly backup can finish.
	measureSmallFileCount = 100
	measureSmallFileBytes = 1_000

	// Concurrent writes answer the other question a single stream cannot: whether the
	// provider and the link scale when several objects move at once. Four streams of
	// 250 KB keep the phase short while still overlapping enough requests to differ from
	// one stream.
	measureConcurrentStreams = 4
	measureConcurrentBytes   = 250_000
)

// Phase names one step of a measurement, in the order Measure runs them. The page lists
// the steps in this order and reveals each row when its phase finishes, so the order and
// the names are a contract with that page, not an implementation detail here.
type Phase string

const (
	PhaseLatency     Phase = "latency"
	PhaseUpload      Phase = "upload"
	PhaseDownload    Phase = "download"
	PhaseSmallFiles  Phase = "smallFiles"
	PhaseRenames     Phase = "renames"
	PhaseDeletes     Phase = "deletes"
	PhaseConcurrency Phase = "concurrency"
)

// Progress is one boundary of a measurement: a phase is starting, or has finished with the
// measurement as it stands. A measurement takes minutes on a slow link, so a caller that
// shows nothing until the end makes the person watch an empty page; reporting the
// boundaries is what lets it fill one row at a time. The observer runs on the measuring
// goroutine, and a caller that does not care passes nil.
type Progress struct {
	Phase Phase
	// Done is false at the moment the phase starts and true once its numbers are in
	// Measurement. The start is reported separately because a phase can spend a long time
	// on the network before it has anything to say.
	Done        bool
	Measurement Measurement
}

// Measurement is what one reachability/latency/throughput test observed. Rates are
// bytes per second over the phase that moved the bytes, except the file-operation
// phases, which are files per second because that is what bounds a backup of many small
// files. Latency is the median metadata round trip.
type Measurement struct {
	Latency time.Duration

	Uploaded     int64
	Downloaded   int64
	UploadRate   int64
	DownloadRate int64

	SmallFiles  FileOpsPhase
	Renames     FileOpsPhase
	Deletes     FileOpsPhase
	Concurrency ConcurrentPhase
}

// FileOpsPhase is one phase measured in files rather than bytes: how many objects were
// handled, how long the batch took, and the rate that follows.
type FileOpsPhase struct {
	Files        int
	BytesPerFile int64
	Elapsed      time.Duration
	PerSecond    float64
	// Error is set when the phase could not finish. It does not fail the whole
	// measurement: a provider that answers and moves a large object but refuses repeated
	// creates is a finding to show on its own row, not a failed test.
	Error string
}

// ConcurrentPhase is the overlapping-writes result. Rate is the aggregate, Share is the
// average per-stream part of it.
type ConcurrentPhase struct {
	Streams        int
	BytesPerStream int64
	Elapsed        time.Duration
	Rate           int64
	Share          int64
	Error          string
}

// Measure tests one object connection by writing, reading, renaming and deleting known
// objects under the reserved probe prefix. It answers the questions a person asks about a
// store they just attached — is it reachable, how fast does it move a large file, how
// many small files can a backup write, rename and delete per second, does it scale — using
// the same native path a backup uses, so a passing result is evidence about the path that
// matters rather than about a separate API.
//
// It measures reachability and throughput only. Whether the store may be published to is
// the product's promise (storageproduct), so a fast store whose product cannot publish is
// reported as "reachable, not ready" rather than as a failed transfer.
//
// progress, when not nil, is called at every phase boundary with the measurement as it
// stands, so a page can fill its rows one at a time while the run is still working.
func Measure(ctx context.Context, cfg *config.Config, progress func(Progress)) (Measurement, error) {
	var m Measurement
	if cfg == nil {
		return m, fmt.Errorf("storage configuration is required to measure a connection")
	}
	switch cfg.BackendName() {
	case "cos", "s3":
	default:
		return m, fmt.Errorf("storage connection %q uses backend %q: a speed test measures the object path, which only cos and s3 provide",
			cfg.Storage.ConnectionID, cfg.BackendName())
	}
	backend, closer, err := objectMutationBackend(cfg)
	if err != nil {
		return m, fmt.Errorf("storage connection %q: open the provider for measuring: %w", cfg.Storage.ConnectionID, err)
	}
	if closer != nil {
		defer closer.Close()
	}
	endpoint, bucket, _ := connectionIdentity(cfg)
	return measureBackend(ctx, cfg.Storage.ConnectionID, endpoint, bucket, backend, progress)
}

// report hands one phase boundary to the caller's observer, if it asked for one.
func report(progress func(Progress), phase Phase, done bool, m Measurement) {
	if progress == nil {
		return
	}
	progress(Progress{Phase: phase, Done: done, Measurement: m})
}

// measureBackend is the half of Measure that touches a provider, split out so a test
// can time a fixture instead of a network.
//
// The reachability and large-transfer phases are fatal: without them there is nothing to
// report. The file-operation and concurrency phases are not — a provider that answers and
// moves 100 MB but refuses repeated creates is a finding to display, not a failed test.
func measureBackend(ctx context.Context, name, endpoint, bucket string, backend objectmutation.Backend, progress func(Progress)) (Measurement, error) {
	var m Measurement
	suffix, err := randomProbeSuffix()
	if err != nil {
		return m, err
	}
	prefix := probeRoot + suffix
	objects := newProbeObjects(name, backend)
	// Cleanup runs on the background context: the answer has already been produced by
	// the time it matters, and a client that disconnects must not leave probe objects in
	// the store.
	defer objects.remove(context.Background())

	// A metadata request for a key that does not exist costs one round trip and proves
	// the endpoint answers. It is the latency number, not an error. Three of them, so a
	// single slow request cannot be reported as the distance to the provider.
	report(progress, PhaseLatency, false, m)
	samples := make([]time.Duration, 0, measureLatencySamples)
	for i := 0; i < measureLatencySamples; i++ {
		key := fmt.Sprintf("%s/latency-%d", prefix, i)
		start := time.Now()
		if _, err := backend.Head(ctx, key); err != nil && !storage.IsCode(err, "not_found") {
			return m, fmt.Errorf("storage connection %q: reach %s bucket %q: %w", name, endpoint, bucket, err)
		}
		samples = append(samples, time.Since(start))
	}
	m.Latency = medianDuration(samples)
	report(progress, PhaseLatency, true, m)

	// The row is reported as running before the payload is prepared: filling 100 MB of
	// random bytes is part of what the upload costs, and a row that stayed blank until the
	// provider saw the first byte would make that preparation look like nothing happening.
	report(progress, PhaseUpload, false, m)
	payload := make([]byte, measureBytes)
	if _, err := rand.Read(payload); err != nil {
		return m, fmt.Errorf("storage connection %q: prepare the test payload: %w", name, err)
	}
	key := prefix + "/measure"
	start := time.Now()
	object, err := objects.put(ctx, key, payload)
	if err != nil {
		return m, fmt.Errorf("storage connection %q: upload the test object %s: %w", name, key, err)
	}
	m.Uploaded = int64(len(payload))
	m.UploadRate = rate(m.Uploaded, time.Since(start))
	report(progress, PhaseUpload, true, m)

	report(progress, PhaseDownload, false, m)
	start = time.Now()
	body, err := backend.Open(ctx, key, object.ETag)
	if err != nil {
		return m, fmt.Errorf("storage connection %q: download the test object %s: %w", name, key, err)
	}
	read, err := io.Copy(io.Discard, body)
	closeErr := body.Close()
	if err != nil {
		return m, fmt.Errorf("storage connection %q: read the test object %s back: %w", name, key, err)
	}
	if closeErr != nil {
		return m, fmt.Errorf("storage connection %q: close the test object %s: %w", name, key, closeErr)
	}
	m.Downloaded = read
	m.DownloadRate = rate(read, time.Since(start))
	report(progress, PhaseDownload, true, m)

	smallPrefix, renamedPrefix := prefix+"/small", prefix+"/renamed"
	report(progress, PhaseSmallFiles, false, m)
	written, smallFiles := writeSmallFileBatch(ctx, name, objects, smallPrefix, payload[:measureSmallFileBytes])
	m.SmallFiles = smallFiles
	report(progress, PhaseSmallFiles, true, m)

	report(progress, PhaseRenames, false, m)
	m.Renames = measureRenames(ctx, name, objects, written, renamedPrefix)
	report(progress, PhaseRenames, true, m)

	// The delete phase removes the batch the two phases above left on the store: a renamed
	// object where the rename completed, the original where it did not. Nothing is deleted
	// twice and nothing is measured that the small-file phase did not write.
	report(progress, PhaseDeletes, false, m)
	m.Deletes = measureDeletes(ctx, name, objects, smallPrefix, renamedPrefix)
	report(progress, PhaseDeletes, true, m)

	report(progress, PhaseConcurrency, false, m)
	m.Concurrency = measureConcurrency(ctx, name, objects, prefix+"/concurrent", payload[:measureConcurrentBytes])
	report(progress, PhaseConcurrency, true, m)
	return m, nil
}

// writeSmallFileBatch writes the small-object batch one object at a time and returns the
// objects it wrote, so the rename and delete phases measure that batch rather than a
// synthetic one. It is sequential because the number it reports is per-file latency: a
// concurrent batch would answer the concurrency phase's question instead and hide the
// per-round-trip cost this row exists to report.
func writeSmallFileBatch(ctx context.Context, name string, objects *probeObjects, prefix string, payload []byte) ([]objectmutation.Object, FileOpsPhase) {
	start := time.Now()
	written := make([]objectmutation.Object, 0, measureSmallFileCount)
	var writeErr error
	for i := 0; i < measureSmallFileCount; i++ {
		key := fmt.Sprintf("%s-%03d", prefix, i)
		object, err := objects.put(ctx, key, payload)
		if err != nil {
			writeErr = fmt.Errorf("storage connection %q: write small object %s: %w", name, key, err)
			break
		}
		written = append(written, object)
	}
	phase := newFileOpsPhase(len(written), int64(len(payload)), time.Since(start))
	if writeErr != nil {
		phase.Error = writeErr.Error()
	}
	return written, phase
}

// measureRenames moves each written object to a fresh key: a provider-side copy bound to
// the source revision, then the conditional delete of the source. That pair is exactly
// what this service's move costs inside an object store, so the rate is the rate a rename
// of these files would actually run at.
func measureRenames(ctx context.Context, name string, objects *probeObjects, sources []objectmutation.Object, prefix string) FileOpsPhase {
	if len(sources) == 0 {
		return FileOpsPhase{Error: fmt.Sprintf("storage connection %q: the small-file batch is empty, so there is nothing to rename", name)}
	}
	copier, ok := objects.backend.(objectmutation.CopyBackend)
	if !ok {
		return FileOpsPhase{Error: fmt.Sprintf("storage connection %q: this provider has no provider-side copy, so a rename cannot be measured", name)}
	}
	start := time.Now()
	renamed := 0
	var firstErr error
	for i, source := range sources {
		target := fmt.Sprintf("%s-%03d", prefix, i)
		mediaType := source.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		copied, err := copier.CopyObject(ctx, objectmutation.Copy{
			Source: source.Key, Target: target, SourceETag: source.ETag,
			Size: source.Size, MediaType: mediaType, Absent: true,
		})
		if err != nil {
			firstErr = fmt.Errorf("storage connection %q: rename %s to %s: %w", name, source.Key, target, err)
			break
		}
		// The target exists from here on: track it before anything else can fail, so the
		// cleanup removes it even when the source delete does not.
		objects.add(copied)
		if _, err := objects.backend.Delete(ctx, source.Key, source.ETag); err != nil {
			firstErr = fmt.Errorf("storage connection %q: remove %s after copying it to %s: %w", name, source.Key, target, err)
			break
		}
		objects.forget(source.Key)
		renamed++
	}
	phase := newFileOpsPhase(renamed, 0, time.Since(start))
	if firstErr != nil {
		phase.Error = firstErr.Error()
	}
	return phase
}

// measureDeletes removes the small-file batch for real and times it. The objects come
// from the batch the two phases above left behind, so this is the delete a backup's
// prune would pay, not a synthetic single-object measurement.
func measureDeletes(ctx context.Context, name string, objects *probeObjects, prefixes ...string) FileOpsPhase {
	targets := objects.withPrefixes(prefixes...)
	if len(targets) == 0 {
		return FileOpsPhase{Error: fmt.Sprintf("storage connection %q: the small-file batch is empty, so there is nothing to delete", name)}
	}
	start := time.Now()
	deleted := 0
	var firstErr error
	for _, target := range targets {
		if _, err := objects.backend.Delete(ctx, target.Key, target.ETag); err != nil {
			firstErr = fmt.Errorf("storage connection %q: delete probe object %s: %w", name, target.Key, err)
			break
		}
		objects.forget(target.Key)
		deleted++
	}
	phase := newFileOpsPhase(deleted, 0, time.Since(start))
	if firstErr != nil {
		phase.Error = firstErr.Error()
	}
	return phase
}

// measureConcurrency overlaps a fixed number of writes and times the whole batch. The
// keys are distinct so the provider cannot answer from a cache or collapse the writes,
// and every stream re-reads the same payload from its own reader.
func measureConcurrency(ctx context.Context, name string, objects *probeObjects, prefix string, payload []byte) ConcurrentPhase {
	phase := ConcurrentPhase{Streams: measureConcurrentStreams, BytesPerStream: int64(len(payload))}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	start := time.Now()
	for i := 0; i < measureConcurrentStreams; i++ {
		wg.Add(1)
		go func(stream int) {
			defer wg.Done()
			key := fmt.Sprintf("%s-%02d", prefix, stream)
			if _, err := objects.put(ctx, key, payload); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("storage connection %q: write concurrent object %s: %w", name, key, err)
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	phase.Elapsed = time.Since(start)
	if firstErr != nil {
		phase.Error = firstErr.Error()
	}
	// The batch is still timed when one stream failed: a provider that answers 3 of 4
	// concurrent creates is faster than one that refuses all of them, and the row says so
	// next to the error.
	if measureConcurrentStreams > 0 {
		phase.Rate = rate(int64(measureConcurrentStreams)*int64(len(payload)), phase.Elapsed)
		phase.Share = phase.Rate / measureConcurrentStreams
	}
	return phase
}

// newFileOpsPhase turns a batch and its elapsed time into the rate the page prints.
func newFileOpsPhase(files int, bytesPerFile int64, elapsed time.Duration) FileOpsPhase {
	phase := FileOpsPhase{Files: files, BytesPerFile: bytesPerFile, Elapsed: elapsed}
	if files > 0 && elapsed > 0 {
		phase.PerSecond = roundRate(float64(files) / elapsed.Seconds())
	}
	return phase
}

// probeObjects records what a measurement wrote, keyed by key, so a later phase can reuse
// an object (rename it, delete it) and the final cleanup removes only what is left. It is
// safe for the concurrent phase's goroutines.
type probeObjects struct {
	name    string
	backend objectmutation.Backend
	mu      sync.Mutex
	objects map[string]objectmutation.Object
	order   []string
}

func newProbeObjects(name string, backend objectmutation.Backend) *probeObjects {
	return &probeObjects{name: name, backend: backend, objects: map[string]objectmutation.Object{}}
}

func (p *probeObjects) put(ctx context.Context, key string, payload []byte) (objectmutation.Object, error) {
	object, err := p.backend.Put(ctx, objectmutation.Put{
		Key: key, Body: bytes.NewReader(payload), Size: int64(len(payload)),
		MediaType: "application/octet-stream", Absent: true,
	})
	if err != nil {
		return object, err
	}
	p.add(object)
	return object, nil
}

func (p *probeObjects) add(object objectmutation.Object) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.objects[object.Key]; !exists {
		p.order = append(p.order, object.Key)
	}
	p.objects[object.Key] = object
}

func (p *probeObjects) forget(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.objects, key)
}

// withPrefixes returns the tracked objects whose key starts with any of the prefixes, in
// creation order.
func (p *probeObjects) withPrefixes(prefixes ...string) []objectmutation.Object {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]objectmutation.Object, 0)
	for _, key := range p.order {
		object, live := p.objects[key]
		if !live {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				out = append(out, object)
				break
			}
		}
	}
	return out
}

// remove deletes every object a phase left behind. The deletes overlap so the answer to
// the person who asked for the test is not delayed by one round trip per probe object.
func (p *probeObjects) remove(ctx context.Context) {
	p.mu.Lock()
	objects := make([]objectmutation.Object, 0, len(p.order))
	for _, key := range p.order {
		if object, live := p.objects[key]; live {
			objects = append(objects, object)
		}
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, object := range objects {
		wg.Add(1)
		go func(object objectmutation.Object) {
			defer wg.Done()
			if _, err := p.backend.Delete(ctx, object.Key, object.ETag); err != nil {
				// The test itself passed; a leftover probe object is operator housekeeping,
				// and naming it is the only way anyone finds it again.
				slog.Warn("storage probe object was not cleaned up", "connection", p.name, "key", object.Key, "error", err)
			}
		}(object)
	}
	wg.Wait()
}

// medianDuration is the middle sample of an odd count and the lower middle of an even
// one: the report is one observed round trip, never an interpolation of two.
func medianDuration(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ordered[len(ordered)/2]
}

// roundRate keeps a per-second figure to one decimal: "2.5 files/s" is a different
// provider than "2 files/s" on a slow link, while more digits say nothing.
func roundRate(perSecond float64) float64 {
	return math.Round(perSecond*10) / 10
}

func rate(bytes int64, elapsed time.Duration) int64 {
	if elapsed <= 0 {
		return 0
	}
	return bytes * int64(time.Second) / int64(elapsed)
}
