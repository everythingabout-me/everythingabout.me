package fileops

// Measurement harness for the operation journal's per-entry document cost.
//
// Why this exists: the journal keeps the whole Go Operation — including the per-entry
// `Entries` list — in one jsonb document, and every settlement transition rewrites the
// whole document. The claim under test is that a large move therefore rewrites a
// multi-megabyte document several times per entry while holding the owner lock, so its
// database cost is quadratic in the number of entries. Phase 1 of
// docs/design/journal-entries.md is this measurement; the findings are recorded there.
//
// It is a test, not a separate binary, because it needs the real settlement path:
// Actions.Prepare freezes the manifest, Actions.Run executes and settles every entry, and
// Service.ApplyEntry performs the per-entry Claim/BeginEntry/SaveEntry/Release cycle. Only
// the native tree is a fake (in memory), so the journal is the only thing being timed.
//
// Two passes per size:
//
//	bytes pass — a trigger on file_operations records, for every INSERT and UPDATE, the
//	  exact logical document size (`octet_length(snapshot::text)`) and the size of the
//	  same document with `Entries` removed (`octet_length((snapshot - 'Entries')::text)`).
//	  The difference is the share of every rewrite that exists only because the entries
//	  live in the document.
//	timing pass — no trigger; the pool carries a pgx QueryTracer that attributes every
//	  statement to a class (document write, locked document read, advisory lock, ...) and
//	  sums its execution time. The result is the share of the operation spent inside
//	  journal statements; the owner advisory lock is held from the first statement of
//	  each such transaction to its COMMIT, so the traced time is the lock-held floor.
//
// Run it with (the gate keeps it out of `make test` and `make test-pg`):
//
//	EAM_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
//	EAM_JOURNAL_COST_MEASUREMENT=1 \
//	go test ./internal/fileops -run TestJournalEntriesDocumentCost -v -timeout 1800s
//
// The size defaults to 500 files (a move manifests two entries per file: copy, then
// delete), which is about six minutes. Override with EAM_JOURNAL_COST_FILES=100,500.
// Settling 5,000 files writes roughly 500 GB of JSON, so that size is measured with
// EAM_JOURNAL_COST_FREEZE_ONLY=1 (admission and manifest freeze only, seconds); the
// measured write count per entry and the measured document size then project the total.
// EAM_JOURNAL_COST_BYTES_ONLY=1 skips the timing pass for any size.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestJournalEntriesDocumentCost(t *testing.T) {
	if os.Getenv("EAM_JOURNAL_COST_MEASUREMENT") != "1" {
		t.Skip("journal document-cost measurement: set EAM_JOURNAL_COST_MEASUREMENT=1 (and EAM_TEST_DATABASE_URL) to run it")
	}
	sizes := costFileCounts(t)
	pool, traced, tracer := costPools(t)
	installCostTrigger(t, pool)
	bytesOnly := os.Getenv("EAM_JOURNAL_COST_BYTES_ONLY") == "1"
	freezeOnly := os.Getenv("EAM_JOURNAL_COST_FREEZE_ONLY") == "1"

	for _, files := range sizes {
		files := files
		t.Run(fmt.Sprintf("files=%d", files), func(t *testing.T) {
			if freezeOnly {
				// Admission and the manifest freeze only: enough to measure the document a
				// large operation carries, without settling its entries. Used for sizes whose
				// full settlement would write hundreds of gigabytes.
				measured := measureCostFrozen(t, pool, files)
				reportCost(t, measured, nil, files)
				t.Logf("projection from the frozen document: %s", projectCost(measured))
				return
			}
			measured := measureCostBytes(t, pool, files)
			if bytesOnly {
				reportCost(t, measured, nil, files)
				t.Logf("projection (bytes-only run): %s", projectCost(measured))
				return
			}
			timing := measureCostTime(t, traced, tracer, files)
			reportCost(t, measured, &timing, files)
			t.Logf("projection from the measured bytes/write ratio: %s", projectCost(measured))
		})
	}
}

// costFileCounts parses EAM_JOURNAL_COST_FILES, defaulting to the size the design doc
// reports with a full timing pass.
func costFileCounts(t *testing.T) []int {
	t.Helper()
	raw := os.Getenv("EAM_JOURNAL_COST_FILES")
	if raw == "" {
		raw = "500"
	}
	var sizes []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 {
			t.Fatalf("EAM_JOURNAL_COST_FILES: %q is not a positive file count", part)
		}
		sizes = append(sizes, n)
	}
	if len(sizes) == 0 {
		t.Fatal("EAM_JOURNAL_COST_FILES names no sizes")
	}
	return sizes
}

// costPools creates the one scratch database every measured run shares, applies every
// migration twice, and opens two pools to it: a plain one for the bytes pass and a traced
// one for the timing pass, so the recording trigger never inflates a measured statement.
func costPools(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool, *costTracer) {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_journal_cost_")
	ctx := context.Background()

	open := func(cfg *pgxpool.Config) *pgxpool.Pool {
		cfg.MaxConns = 8
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	baseCfg, err := pgxpool.ParseConfig(scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := open(baseCfg)

	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) < 17 {
		t.Fatalf("migration discovery: %d %v", len(migrations), err)
	}
	sort.Strings(migrations)
	for pass := 0; pass < 2; pass++ {
		for _, path := range migrations {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, string(b)); err != nil {
				t.Fatalf("migration %s pass %d: %v", filepath.Base(path), pass+1, err)
			}
		}
	}

	traceCfg, err := pgxpool.ParseConfig(scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	tracer := &costTracer{}
	traceCfg.ConnConfig.Tracer = tracer
	traced := open(traceCfg)
	t.Logf("scratch database %s: %d migrations applied twice", scratch.Name, len(migrations))
	return pool, traced, tracer
}

// installCostTrigger creates the byte recorder and leaves it disabled. The timing pass runs
// with it off; the bytes pass turns it on for exactly the operation under measurement.
func installCostTrigger(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	statements := []string{
		`create unlogged table if not exists journal_cost_writes (
			seq bigserial primary key,
			op_id uuid not null,
			kind text not null,
			state text,
			phase text,
			doc_bytes bigint not null,
			no_entries_bytes bigint not null,
			at timestamptz not null default clock_timestamp())`,
		`create index if not exists journal_cost_writes_op on journal_cost_writes(op_id, seq)`,
		`create or replace function journal_cost_record() returns trigger language plpgsql as $$
		begin
			insert into journal_cost_writes(op_id, kind, state, phase, doc_bytes, no_entries_bytes)
			values (new.id, lower(tg_op), new.state, new.phase,
			        octet_length(new.snapshot::text),
			        octet_length((new.snapshot - 'Entries')::text));
			return null;
		end $$`,
		`drop trigger if exists journal_cost_record on file_operations`,
		`create trigger journal_cost_record after insert or update on file_operations
		 for each row execute function journal_cost_record()`,
		`alter table file_operations disable trigger journal_cost_record`,
	}
	for _, sql := range statements {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("install cost trigger: %v", err)
		}
	}
}

func setCostTrigger(t *testing.T, pool *pgxpool.Pool, enabled bool) {
	t.Helper()
	action := "disable"
	if enabled {
		action = "enable"
	}
	if _, err := pool.Exec(context.Background(), `alter table file_operations `+action+` trigger journal_cost_record`); err != nil {
		t.Fatalf("%s cost trigger: %v", action, err)
	}
}

// costTree builds the native fixture: one folder holding `files` files. A move out of it
// manifests one copy and one delete per file, plus the folder's own mkdir and delete.
func costTree(root Root, files int) *actionNative {
	n := newActionNative(root)
	n.pageSize = 1000
	n.put("src", storage.Folder, "")
	modified := time.Date(2024, 7, 3, 10, 0, 0, 0, time.UTC)
	for i := 0; i < files; i++ {
		path := storage.Path(fmt.Sprintf("src/2024-07-holiday-IMG_%06d.HEIC", i))
		n.put(path, storage.File, "x")
		// The captured entry is what makes the document big: a driver's native ETag, its
		// private native ref, a content digest and the observed metadata. A fixture with
		// only a path would understate a real manifest, so the shape is a real photo entry.
		e := n.entries[path]
		e.Revision = []byte(fmt.Sprintf("%032d", i))
		e.NativeETag = e.Revision
		e.NativeRef = []byte(fmt.Sprintf("native-object-%020d", i))
		e.MediaType = "image/heic"
		e.ModifiedAt = modified.Add(time.Duration(i) * time.Second)
		e.Digest = &storage.Digest{Algorithm: "sha256", Value: bytes.Repeat([]byte{byte(i)}, 32)}
		n.entries[path] = e
	}
	return n
}

func moveCostRequest(root Root, actor Actor) ActionRequest {
	return ActionRequest{Root: root, Actor: actor, Kind: "move", From: "src", To: "dst", IfRevision: "revision"}
}

type costWrite struct {
	seq            int64
	kind           string
	state          string
	phase          string
	docBytes       int64
	noEntriesBytes int64
}

// costBytes is everything the byte pass measured for one operation.
type costBytes struct {
	files          int
	manifest       int
	inserts        int
	updates        int
	writes         []costWrite
	totalDoc       int64
	totalNoEntries int64
	walBytes       int64
	peakDoc        int64
	frozenDoc      int64 // the first update: the document immediately after the manifest froze
	groups         []costGroup
	frozen         bool // admission and manifest freeze only; settlement was not run
}

type costGroup struct {
	kind, state, phase string
	count              int
	docBytes           int64
}

// measureCostFrozen admits the operation and freezes the manifest, then stops. It measures
// the document that a large operation carries without paying for its settlement.
func measureCostFrozen(t *testing.T, pool *pgxpool.Pool, files int) costBytes {
	t.Helper()
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	n := costTree(r, files)
	a := actionCoordinator(t, p, nil)

	setCostTrigger(t, pool, true)
	defer setCostTrigger(t, pool, false)

	op, replay, err := a.Prepare(ctx, moveCostRequest(r, actor), n)
	if err != nil || replay {
		t.Fatalf("prepare move of %d files: replay=%v err=%v", files, replay, err)
	}
	res := readCostWrites(t, ctx, pool, op.ID, files, len(op.Entries))
	res.frozen = true
	return res
}

// costWAL returns the current write-ahead-log insert position. The difference of two
// readings around one operation is the physical write volume the database attributes to it,
// including its own page images; it is the number a disk actually pays.
func costWAL(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var lsn string
	if err := pool.QueryRow(context.Background(), `select pg_current_wal_insert_lsn()::text`).Scan(&lsn); err != nil {
		t.Fatalf("read WAL position: %v", err)
	}
	var n int64
	if err := pool.QueryRow(context.Background(), `select pg_wal_lsn_diff($1::pg_lsn, '0/0'::pg_lsn)::bigint`, lsn).Scan(&n); err != nil {
		t.Fatalf("convert WAL position: %v", err)
	}
	return n
}

func measureCostBytes(t *testing.T, pool *pgxpool.Pool, files int) costBytes {
	t.Helper()
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	n := costTree(r, files)
	a := actionCoordinator(t, p, nil)

	setCostTrigger(t, pool, true)
	defer setCostTrigger(t, pool, false)

	walBefore := costWAL(t, pool)
	op, replay, err := a.Prepare(ctx, moveCostRequest(r, actor), n)
	if err != nil || replay {
		t.Fatalf("prepare move of %d files: replay=%v err=%v", files, replay, err)
	}
	out, err := a.Run(ctx, r, actor, op.ID, n)
	if err != nil {
		t.Fatalf("run move of %d files: %v", files, err)
	}
	if out.State != Succeeded {
		t.Fatalf("move of %d files settled %s: %s", files, out.State, out.Problem)
	}
	res := readCostWrites(t, ctx, pool, op.ID, files, len(op.Entries))
	res.walBytes = costWAL(t, pool) - walBefore
	return res
}

func readCostWrites(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, files, entries int) costBytes {
	t.Helper()
	rows, err := pool.Query(ctx, `select seq, kind, coalesce(state,''), coalesce(phase,''), doc_bytes, no_entries_bytes
		from journal_cost_writes where op_id=$1 order by seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	res := costBytes{files: files, manifest: entries}
	for rows.Next() {
		var w costWrite
		if err = rows.Scan(&w.seq, &w.kind, &w.state, &w.phase, &w.docBytes, &w.noEntriesBytes); err != nil {
			t.Fatal(err)
		}
		res.writes = append(res.writes, w)
		res.totalDoc += w.docBytes
		res.totalNoEntries += w.noEntriesBytes
		if w.docBytes > res.peakDoc {
			res.peakDoc = w.docBytes
		}
		switch w.kind {
		case "insert":
			res.inserts++
		case "update":
			res.updates++
			if res.frozenDoc == 0 {
				res.frozenDoc = w.docBytes
			}
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(res.writes) == 0 {
		t.Fatal("no journal writes were recorded; the trigger did not fire")
	}
	res.groups = groupCostWrites(res.writes)
	return res
}

func groupCostWrites(writes []costWrite) []costGroup {
	byKey := map[[3]string]*costGroup{}
	var order [][3]string
	for _, w := range writes {
		key := [3]string{w.kind, w.state, w.phase}
		g, ok := byKey[key]
		if !ok {
			g = &costGroup{kind: w.kind, state: w.state, phase: w.phase}
			byKey[key] = g
			order = append(order, key)
		}
		g.count++
		g.docBytes += w.docBytes
	}
	out := make([]costGroup, 0, len(byKey))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}

// costTiming is the traced timing pass.
type costTiming struct {
	prepare time.Duration
	total   time.Duration
	byClass map[string]costClass
}

type costClass struct {
	count int
	total time.Duration
}

type costTracer struct {
	mu    sync.Mutex
	spans map[string]*costClass
}

type costSpanKey struct{}

type costSpan struct {
	sql   string
	start time.Time
}

func (c *costTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, costSpanKey{}, costSpan{sql: data.SQL, start: time.Now()})
}

func (c *costTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	span, ok := ctx.Value(costSpanKey{}).(costSpan)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spans == nil {
		c.spans = map[string]*costClass{}
	}
	key := classifyCostSQL(span.sql)
	entry := c.spans[key]
	if entry == nil {
		entry = &costClass{}
		c.spans[key] = entry
	}
	entry.count++
	entry.total += time.Since(span.start)
}

func (c *costTracer) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans = map[string]*costClass{}
}

func (c *costTracer) snapshot() map[string]costClass {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]costClass, len(c.spans))
	for k, v := range c.spans {
		out[k] = *v
	}
	return out
}

// classifyCostSQL names the journal statements that matter and leaves everything else in
// `other`. The classes are what the report attributes time to.
func classifyCostSQL(sql string) string {
	flat := strings.Join(strings.Fields(strings.ToLower(sql)), " ")
	switch {
	case strings.Contains(flat, "pg_advisory_xact_lock"):
		return "owner_advisory_lock"
	case strings.Contains(flat, "update file_operations"):
		return "document_write"
	case strings.Contains(flat, "from file_operations where id=$1 for update"):
		return "document_read_locked"
	case strings.Contains(flat, "from file_operations where id=$1"):
		return "document_read_only"
	case strings.Contains(flat, "from file_operations"):
		return "journal_scan"
	case strings.Contains(flat, "file_operation_guards"):
		return "guards"
	case strings.Contains(flat, "file_operation_checkpoints"):
		return "checkpoints"
	case strings.Contains(flat, "file_credit_claims") || strings.Contains(flat, "file_reservations") || strings.Contains(flat, "file_budgets"):
		return "quota"
	case strings.Contains(flat, "file_action_bindings"):
		return "action_binding"
	case strings.Contains(flat, "file_uploads"):
		return "uploads"
	default:
		return "other"
	}
}

func measureCostTime(t *testing.T, pool *pgxpool.Pool, tracer *costTracer, files int) costTiming {
	t.Helper()
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	n := costTree(r, files)
	a := actionCoordinator(t, p, nil)

	tracer.reset()
	start := time.Now()
	op, replay, err := a.Prepare(ctx, moveCostRequest(r, actor), n)
	prepare := time.Since(start)
	if err != nil || replay {
		t.Fatalf("prepare move of %d files: replay=%v err=%v", files, replay, err)
	}
	runStart := time.Now()
	out, err := a.Run(ctx, r, actor, op.ID, n)
	total := time.Since(runStart)
	if err != nil {
		t.Fatalf("run move of %d files: %v", files, err)
	}
	if out.State != Succeeded {
		t.Fatalf("move of %d files settled %s: %s", files, out.State, out.Problem)
	}
	return costTiming{prepare: prepare, total: total, byClass: tracer.snapshot()}
}

func costBaseline() Baseline {
	return provenBaseline(0, 0, 1<<50, 1<<50)
}

// reportCost prints the measured numbers in a shape that can be pasted into the design
// doc. Nothing here is asserted: the harness measures, it does not gate.
func reportCost(t *testing.T, bytes costBytes, timing *costTiming, files int) {
	t.Helper()
	entries := bytes.manifest
	if entries == 0 {
		t.Fatal("no manifest entries")
	}
	writes := bytes.inserts + bytes.updates
	t.Logf("files=%d manifest_entries=%d", files, entries)
	if bytes.frozen {
		t.Logf("mode: admission and manifest freeze only; entry settlement was not run")
	} else {
		t.Logf("journal writes: %d total (%d inserts, %d updates) = %.2f per manifest entry, %.2f per file",
			writes, bytes.inserts, bytes.updates, float64(writes)/float64(entries), float64(writes)/float64(files))
	}
	t.Logf("document bytes per write: after the manifest froze %d, peak %d", bytes.frozenDoc, bytes.peakDoc)
	t.Logf("document bytes written by the operation: %d total (%.3f MB); Entries account for %d (%.1f%%)",
		bytes.totalDoc, float64(bytes.totalDoc)/1e6, bytes.totalDoc-bytes.totalNoEntries,
		100*float64(bytes.totalDoc-bytes.totalNoEntries)/float64(bytes.totalDoc))
	t.Logf("without Entries the same transitions would write: %d total (%.3f MB) = %.1f%% of the measured volume",
		bytes.totalNoEntries, float64(bytes.totalNoEntries)/1e6, 100*float64(bytes.totalNoEntries)/float64(bytes.totalDoc))
	if bytes.walBytes > 0 {
		t.Logf("WAL written by the whole operation: %d bytes (%.3f MB) = %.1f%% of the logical document volume",
			bytes.walBytes, float64(bytes.walBytes)/1e6, 100*float64(bytes.walBytes)/float64(bytes.totalDoc))
	}
	if bytes.frozen {
		t.Logf("frozen document: %d entries, %d bytes total, %d bytes per entry",
			entries, bytes.peakDoc, bytes.peakDoc/int64(entries))
	} else {
		t.Logf("per manifest entry: %d rewrites, %.1f KB total, %d bytes of document per rewrite",
			writes/entries, float64(bytes.totalDoc)/float64(entries)/1000, bytes.peakDoc/int64(entries))
	}
	for _, g := range bytes.groups {
		t.Logf("  transition %-8s state=%-15s phase=%-15s count=%d doc_bytes=%d",
			g.kind, g.state, g.phase, g.count, g.docBytes)
	}
	if timing == nil {
		return
	}
	var traced, docWrite, lockedRead time.Duration
	for class, stat := range timing.byClass {
		traced += stat.total
		switch class {
		case "document_write":
			docWrite = stat.total
		case "document_read_locked":
			lockedRead = stat.total
		}
	}
	t.Logf("timing: prepare %s, run %s (%.2f ms/file)", timing.prepare, timing.total, float64(timing.total.Milliseconds())/float64(files))
	t.Logf("timing: journal statements %s = %.1f%% of the run; document writes %s (%.1f%%, %.1f ms/write); locked document reads %s",
		traced, 100*float64(traced)/float64(timing.total), docWrite, 100*float64(docWrite)/float64(timing.total),
		float64(docWrite.Milliseconds())/float64(bytes.updates), lockedRead)
	classes := make([]string, 0, len(timing.byClass))
	for class := range timing.byClass {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	for _, class := range classes {
		stat := timing.byClass[class]
		t.Logf("  %-22s count=%6d total=%10s avg=%8s", class, stat.count, stat.total, stat.total/time.Duration(max(stat.count, 1)))
	}
}

// projectCost extrapolates the total document bytes to sizes the timing budget does not
// allow. It uses only measured facts: the writes-per-entry ratio and the document size at
// this size. It is explicitly a projection, and it is labelled as one.
func projectCost(b costBytes) string {
	if len(b.writes) == 0 || b.manifest == 0 {
		return "unavailable (no writes)"
	}
	writesPerEntry := float64(b.inserts+b.updates) / float64(b.manifest)
	perEntryDoc := float64(b.peakDoc) / float64(b.manifest)
	// Both factors grow with the manifest — the document grows, and the number of rewrites
	// grows because every entry has its own transitions — so the total scales with the
	// square of the entry count. Scaling the measured total is therefore exact under the
	// measured per-entry constants and needs no model of where the bytes go.
	total := func(entries float64) float64 {
		ratio := entries / float64(b.manifest)
		return float64(b.totalDoc) * ratio * ratio
	}
	var out strings.Builder
	fmt.Fprintf(&out, "writes/entry=%.2f doc/entry=%.0fB; manifest entries -> total document bytes: ", writesPerEntry, perEntryDoc)
	if b.frozen {
		fmt.Fprintf(&out, "frozen document is %.2f MB at %d entries", float64(b.peakDoc)/1e6, b.manifest)
		return out.String()
	}
	for _, entries := range []int{1000, 5000, 10000} {
		fmt.Fprintf(&out, "%d->%.2f GB  ", entries, total(float64(entries))/1e9)
	}
	return strings.TrimSpace(out.String())
}
