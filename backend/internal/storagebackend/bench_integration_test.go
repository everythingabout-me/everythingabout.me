package storagebackend_test

// Storage-layer upload benchmark. It calls the storage drivers directly (no HTTP
// server, no Postgres) so transfer, sealing and publication latency can be
// separated from API/database overhead.
//
//	EAM_STORAGE_BENCH=cos,local \
//	EAM_STORAGE_BENCH_CASES=1048576x16,16777216x16,33554432x16 \
//	EAM_STORAGE_BENCH_CONCURRENCY=8 \
//	EAM_STORAGE_BENCH_MODE=pool \
//	go test ./internal/storagebackend -run TestStorageUploadBench -v -timeout 60m
//
// MODE=pool (default) keeps CONCURRENCY part transfers in flight across all
// files; MODE=files runs CONCURRENCY files at a time with four parts each.
// In pool mode a file's time starts when its first part starts.
//
// "cos" uses the COS bucket and object admission from .env under a throwaway
// owner root and removes every published object afterwards. "local" uses a
// temporary directory.

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
)

// benchPartSize must equal objectmutation.nativePartSize; override with EAM_STORAGE_BENCH_PART_SIZE
// only when that constant was changed for an experiment.
var benchPartSize = func() int64 {
	if n, err := strconv.ParseInt(os.Getenv("EAM_STORAGE_BENCH_PART_SIZE"), 10, 64); err == nil && n > 0 {
		return n
	}
	return 8 << 20
}()

// benchMetadata is an in-memory UploadMetadata with the documented fencing:
// number 0 is exclusive per session, positive numbers share the session and
// lock their own slot, -1 is a shared lookup. It never serializes different
// sessions, so it adds no artificial contention.
type benchMetadata struct {
	mu       sync.Mutex
	values   map[string][]byte
	sessions map[string]*sync.RWMutex
	slots    map[string]*sync.Mutex
}

type benchRecord struct {
	m      *benchMetadata
	prefix string
}

func (m *benchMetadata) locks(session, slot string) (*sync.RWMutex, *sync.Mutex) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values, m.sessions, m.slots = map[string][]byte{}, map[string]*sync.RWMutex{}, map[string]*sync.Mutex{}
	}
	if m.sessions[session] == nil {
		m.sessions[session] = &sync.RWMutex{}
	}
	if m.slots[slot] == nil {
		m.slots[slot] = &sync.Mutex{}
	}
	return m.sessions[session], m.slots[slot]
}

func (m *benchMetadata) WithUploadMetadata(_ context.Context, binding, id string, number int, fn func(storage.UploadMetadataRecord) error) error {
	session := binding + ":" + id
	sessionLock, slotLock := m.locks(session, session+":"+strconv.Itoa(number))
	if number == 0 {
		sessionLock.Lock()
		defer sessionLock.Unlock()
	} else {
		sessionLock.RLock()
		defer sessionLock.RUnlock()
		if number > 0 {
			slotLock.Lock()
			defer slotLock.Unlock()
		}
	}
	return fn(&benchRecord{m, session + ":"})
}

func (r *benchRecord) Get(_ context.Context, key string) ([]byte, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	return bytes.Clone(r.m.values[r.prefix+key]), nil
}

func (r *benchRecord) Put(_ context.Context, key string, value []byte) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	r.m.values[r.prefix+key] = bytes.Clone(value)
	return nil
}

type benchCase struct {
	size  int64
	count int
}

type benchResult struct {
	path                        storage.Path
	size                        int64
	parts, seal, publish, total time.Duration
	partTimes                   []time.Duration
	err                         error
}

func parseBenchCases(t *testing.T) []benchCase {
	raw := os.Getenv("EAM_STORAGE_BENCH_CASES")
	if raw == "" {
		raw = "1048576x16,16777216x16,33554432x16"
	}
	var out []benchCase
	for _, item := range strings.Split(raw, ",") {
		size, count, ok := strings.Cut(strings.TrimSpace(item), "x")
		s, e1 := strconv.ParseInt(size, 10, 64)
		c, e2 := strconv.Atoi(count)
		if !ok || e1 != nil || e2 != nil || s < 1 || c < 1 {
			t.Fatalf("invalid bench case %q; want <bytes>x<count>", item)
		}
		out = append(out, benchCase{s, c})
	}
	return out
}

func TestStorageUploadBench(t *testing.T) {
	backends := os.Getenv("EAM_STORAGE_BENCH")
	if backends == "" {
		t.Skip("set EAM_STORAGE_BENCH=cos,local to run the storage upload benchmark")
	}
	concurrency := 8
	if raw := os.Getenv("EAM_STORAGE_BENCH_CONCURRENCY"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			t.Fatalf("invalid EAM_STORAGE_BENCH_CONCURRENCY %q", raw)
		}
		concurrency = n
	}
	mode := os.Getenv("EAM_STORAGE_BENCH_MODE")
	if mode == "" {
		mode = "pool"
	}
	if mode != "pool" && mode != "files" {
		t.Fatalf("invalid EAM_STORAGE_BENCH_MODE %q; want pool or files", mode)
	}
	cases := parseBenchCases(t)
	for _, name := range strings.Split(backends, ",") {
		name = strings.TrimSpace(name)
		t.Run(name, func(t *testing.T) {
			driver := openBenchDriver(t, name)
			for _, c := range cases {
				runBenchCase(t, name, driver, c, concurrency, mode)
			}
		})
	}
}

func openBenchDriver(t *testing.T, name string) storage.Driver {
	t.Helper()
	var cfg *config.Config
	switch name {
	case "cos":
		loaded, err := config.Load()
		if err != nil {
			t.Fatalf("load .env: %v", err)
		}
		if loaded.BackendName() != "cos" || !loaded.Storage.CoordinatedWrites {
			t.Fatalf("cos bench needs STORAGE_BACKEND=cos and FILE_COORDINATED_WRITES=true in .env")
		}
		cfg = loaded
		// Private spool so the bench never shares ledgers with a running server.
		cfg.Storage.ObjectSpoolRoot = t.TempDir()
	case "local":
		root := t.TempDir()
		cfg = &config.Config{}
		cfg.Storage.Backend = "local"
		cfg.Storage.CoordinatedWrites = true
		cfg.Storage.Local.ContentRoot = filepath.Join(root, "content")
		cfg.Storage.Local.SystemRoot = filepath.Join(root, "system")
		for _, dir := range []string{cfg.Storage.Local.ContentRoot, cfg.Storage.Local.SystemRoot} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("unknown bench backend %q (want cos or local)", name)
	}
	backend, err := storagebackend.New(cfg)
	if err != nil {
		t.Fatalf("open %s backend: %v", name, err)
	}
	t.Cleanup(func() { backend.Close() })
	owner := fmt.Sprintf("storage-bench-%d", time.Now().UnixNano())
	if name == "local" {
		for _, dir := range []string{cfg.Storage.Local.ContentRoot, cfg.Storage.Local.SystemRoot} {
			if err := os.MkdirAll(filepath.Join(dir, owner), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	driver, err := backend.Open(context.Background(), owner)
	if err != nil {
		t.Fatalf("open %s driver: %v", name, err)
	}
	t.Cleanup(func() { driver.Close() })
	t.Logf("%s owner root: %s", name, owner)
	return driver
}

// runFilesMode mirrors the current browser scheduler: N files at a time, each
// with up to four parts; the next file starts only after a whole file finishes.
func runFilesMode(ctx context.Context, driver storage.Driver, intents []storage.WriteIntent, files [][]byte, concurrency int) []benchResult {
	results := make([]benchResult, len(files))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = benchUpload(ctx, driver, intents[i], files[i])
		}(i)
	}
	wg.Wait()
	return results
}

// runPoolMode keeps exactly N part transfers in flight across all files: parts
// are queued in file order and the next queued part starts as soon as any part
// finishes. Seal/publish of a completed file runs outside the pool.
func runPoolMode(ctx context.Context, driver storage.Driver, intents []storage.WriteIntent, files [][]byte, concurrency int) []benchResult {
	results := make([]benchResult, len(files))
	writer, _ := driver.(storage.StagedWriter)
	numbered, isNumbered := driver.(storage.NumberedUploadWriter)
	noop := func(context.Context, []byte) error { return nil }
	type fileState struct {
		mu        sync.Mutex
		metadata  *benchMetadata
		receipts  []storage.UploadPart
		staged    storage.Staged
		remaining int
		started   time.Time
	}
	type job struct{ file, number int }
	states := make([]*fileState, len(files))
	var jobs []job
	for i := range files {
		size := int64(len(files[i]))
		count := 1
		if isNumbered {
			count = int((size + benchPartSize - 1) / benchPartSize)
		}
		states[i] = &fileState{metadata: &benchMetadata{}, receipts: make([]storage.UploadPart, count), remaining: count}
		results[i] = benchResult{path: intents[i].Target, size: size, partTimes: make([]time.Duration, count)}
		for n := 1; n <= count; n++ {
			jobs = append(jobs, job{i, n})
		}
	}
	queue := make(chan job, len(jobs))
	for _, j := range jobs {
		queue <- j
	}
	close(queue)
	var finals sync.WaitGroup
	finish := func(i int) {
		defer finals.Done()
		s, r := states[i], &results[i]
		r.parts = time.Since(s.started)
		if r.err == nil && isNumbered {
			t0 := time.Now()
			staged, err := numbered.SealStage(ctx, intents[i], s.receipts, s.metadata, noop)
			r.seal = time.Since(t0)
			s.staged = staged
			if err != nil {
				r.err = fmt.Errorf("seal: %w", err)
			}
		}
		if r.err != nil {
			if isNumbered {
				_ = numbered.DiscardParts(ctx, intents[i], s.metadata)
			}
			return
		}
		t0 := time.Now()
		if _, err := writer.Publish(ctx, s.staged, noop); err != nil {
			r.err = fmt.Errorf("publish: %w", err)
		}
		r.publish = time.Since(t0)
		r.total = time.Since(s.started)
	}
	var workers sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := range queue {
				s, r, data := states[j.file], &results[j.file], files[j.file]
				s.mu.Lock()
				if s.started.IsZero() {
					s.started = time.Now()
				}
				s.mu.Unlock()
				t0 := time.Now()
				var receipt storage.UploadPart
				var staged storage.Staged
				var err error
				if isNumbered {
					off := int64(j.number-1) * benchPartSize
					end := min(off+benchPartSize, int64(len(data)))
					receipt, err = numbered.ReceivePart(ctx, intents[j.file], storage.UploadPart{Number: j.number, Size: end - off}, bytes.NewReader(data[off:end]), s.metadata, func(p storage.UploadPart) (storage.UploadPart, error) { return p, nil })
				} else {
					staged, err = writer.Stage(ctx, intents[j.file], bytes.NewReader(data), noop)
				}
				s.mu.Lock()
				r.partTimes[j.number-1] = time.Since(t0)
				s.receipts[j.number-1] = receipt
				if !isNumbered {
					s.staged = staged
				}
				if err != nil && r.err == nil {
					r.err = fmt.Errorf("part %d: %w", j.number, err)
				}
				s.remaining--
				done := s.remaining == 0
				s.mu.Unlock()
				if done {
					finals.Add(1)
					go finish(j.file)
				}
			}
		}()
	}
	workers.Wait()
	finals.Wait()
	return results
}

func runBenchCase(t *testing.T, backend string, driver storage.Driver, c benchCase, concurrency int, mode string) {
	ctx := context.Background()
	label := fmt.Sprintf("%dx%.1fMiB", c.count, float64(c.size)/(1<<20))
	files := make([][]byte, c.count)
	for i := range files {
		files[i] = make([]byte, c.size)
		rand.Read(files[i])
	}
	parent, err := driver.Stat(ctx, "")
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	stamp := time.Now().UnixNano()
	intents := make([]storage.WriteIntent, c.count)
	for i := range intents {
		id := fmt.Sprintf("bench-%s-%d-%d", label, stamp, i)
		intents[i] = storage.WriteIntent{ID: id, Target: storage.Path(id + ".bin"), Size: c.size, MediaType: "application/octet-stream", Conditions: storage.Conditions{Absent: true}, ParentVersion: parent.Version}
	}
	var results []benchResult
	start := time.Now()
	if mode == "files" {
		results = runFilesMode(ctx, driver, intents, files, concurrency)
		t.Logf("== %s %s, files mode: %d files x 4 parts in parallel", backend, label, concurrency)
	} else {
		results = runPoolMode(ctx, driver, intents, files, concurrency)
		t.Logf("== %s %s, pool mode: %d parts in flight globally", backend, label, concurrency)
	}
	wall := time.Since(start)

	var ok int
	var sumParts, sumSeal, sumPublish []time.Duration
	for i, r := range results {
		if r.err != nil {
			t.Errorf("  #%-2d ERROR %v", i, r.err)
			continue
		}
		ok++
		sumParts, sumSeal, sumPublish = append(sumParts, r.parts), append(sumSeal, r.seal), append(sumPublish, r.publish)
		slowest := time.Duration(0)
		for _, p := range r.partTimes {
			slowest = max(slowest, p)
		}
		t.Logf("  #%-2d total %6.2fs  parts %6.2fs (%d, slowest %5.2fs)  seal %5.2fs  publish %5.2fs  %6.2f MiB/s",
			i, r.total.Seconds(), r.parts.Seconds(), len(r.partTimes), slowest.Seconds(), r.seal.Seconds(), r.publish.Seconds(), mib(r.size, r.total))
	}
	t.Logf("  median parts %.2fs  seal %.2fs  publish %.2fs", median(sumParts), median(sumSeal), median(sumPublish))
	t.Logf("  WALL %.2fs  %d/%d ok  aggregate %.2f MiB/s", wall.Seconds(), ok, c.count, mib(c.size*int64(ok), wall))

	// Cleanup: published objects are removed through the driver's own native path.
	if remover, isRemover := driver.(storage.Remover); isRemover {
		for i, r := range results {
			if r.err != nil {
				continue
			}
			entry, err := driver.Stat(ctx, r.path)
			if err == nil {
				_, err = remover.Remove(ctx, storage.Mutation{ID: fmt.Sprintf("bench-rm-%d-%d", stamp, i), Path: r.path, Conditions: storage.Conditions{ExpectedVersion: entry.Version}}, func(context.Context, []byte) error { return nil })
			}
			if err != nil {
				t.Logf("  cleanup %s: %v", r.path, err)
			}
		}
	}
}

func benchUpload(ctx context.Context, driver storage.Driver, in storage.WriteIntent, data []byte) (res benchResult) {
	res.path, res.size = in.Target, int64(len(data))
	writer, ok := driver.(storage.StagedWriter)
	if !ok {
		res.err = fmt.Errorf("driver is not a staged writer")
		return
	}
	noop := func(context.Context, []byte) error { return nil }
	start := time.Now()
	var staged storage.Staged
	if numbered, isNumbered := driver.(storage.NumberedUploadWriter); isNumbered {
		// Native multipart: same path the API's embedded part receiver uses.
		metadata := &benchMetadata{}
		count := int((res.size + benchPartSize - 1) / benchPartSize)
		receipts := make([]storage.UploadPart, count)
		res.partTimes = make([]time.Duration, count)
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4) // maxParallelism per file
		for n := 1; n <= count; n++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(n int) {
				defer wg.Done()
				defer func() { <-sem }()
				off := int64(n-1) * benchPartSize
				end := min(off+benchPartSize, res.size)
				t0 := time.Now()
				receipt, err := numbered.ReceivePart(ctx, in, storage.UploadPart{Number: n, Size: end - off}, bytes.NewReader(data[off:end]), metadata, func(p storage.UploadPart) (storage.UploadPart, error) { return p, nil })
				mu.Lock()
				defer mu.Unlock()
				res.partTimes[n-1] = time.Since(t0)
				receipts[n-1] = receipt
				if err != nil && res.err == nil {
					res.err = fmt.Errorf("part %d: %w", n, err)
				}
			}(n)
		}
		wg.Wait()
		res.parts = time.Since(start)
		if res.err != nil {
			_ = numbered.DiscardParts(ctx, in, metadata)
			return
		}
		sort.Slice(receipts, func(a, b int) bool { return receipts[a].Number < receipts[b].Number })
		t1 := time.Now()
		var err error
		staged, err = numbered.SealStage(ctx, in, receipts, metadata, noop)
		res.seal = time.Since(t1)
		if err != nil {
			res.err = fmt.Errorf("seal: %w", err)
			return
		}
	} else {
		// Drivers without native parts (local) stage the whole stream.
		var err error
		staged, err = writer.Stage(ctx, in, bytes.NewReader(data), noop)
		res.parts = time.Since(start)
		res.partTimes = []time.Duration{res.parts}
		if err != nil {
			res.err = fmt.Errorf("stage: %w", err)
			return
		}
	}
	t2 := time.Now()
	if _, err := writer.Publish(ctx, staged, noop); err != nil {
		res.err = fmt.Errorf("publish: %w", err)
	}
	res.publish = time.Since(t2)
	res.total = time.Since(start)
	return
}

func mib(n int64, d time.Duration) float64 { return float64(n) / (1 << 20) / d.Seconds() }

func median(values []time.Duration) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(a, b int) bool { return values[a] < values[b] })
	return values[len(values)/2].Seconds()
}
