package objectmutation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

var _ fileops.NativeActions = (*Driver)(nil)

type fakeBackend struct {
	mu                      sync.Mutex
	current                 map[string]Object
	versions                map[string]map[string][]byte
	puts, deletes, heads    int
	putError, errorAfterPut error
	deleteError             error
	zeroPutMetadata         bool
	// emptyETag, when set, is the ETag every zero-byte object gets. It models COS/S3,
	// whose ETag is the content MD5: every empty folder marker spells the same ETag, so
	// rewriting identical empty bytes cannot move a revision.
	emptyETag       string
	failRefreshOnce bool
	block, entered  chan struct{}
}

func newFake() *fakeBackend {
	return &fakeBackend{current: map[string]Object{}, versions: map[string]map[string][]byte{}}
}
func (b *fakeBackend) Head(ctx context.Context, key string) (Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.heads++
	if b.failRefreshOnce && b.puts > 0 {
		b.failRefreshOnce = false
		return Object{}, io.ErrUnexpectedEOF
	}
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	o, ok := b.current[key]
	if !ok {
		return Object{}, fail("not_found", "")
	}
	return o, nil
}

// Open reads the current object only when its ETag still matches (If-Match).
func (b *fakeBackend) Open(ctx context.Context, key, etag string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.current[key]
	if !ok || etag == "" || o.ETag != etag {
		return nil, fail("changed", "")
	}
	data, ok := b.versions[key][o.VersionID]
	if !ok {
		return nil, fail("changed", "")
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), nil
}
func (b *fakeBackend) Put(ctx context.Context, p Put) (Object, error) {
	b.mu.Lock()
	b.puts++
	failure := b.putError
	block, entered := b.block, b.entered
	b.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		<-block
	}
	if failure != nil {
		return Object{}, failure
	}
	data, err := io.ReadAll(p.Body)
	if err != nil {
		return Object{}, err
	}
	if int64(len(data)) != p.Size {
		return Object{}, fmt.Errorf("native body length mismatch")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	old, exists := b.current[p.Key]
	if p.Absent && exists || p.ExpectedETag != "" && (!exists || old.ETag != p.ExpectedETag) {
		return Object{}, fail("precondition_failed", "")
	}
	version := fmt.Sprintf("v%d", b.puts)
	etag := "etag-" + version
	if p.Size == 0 && b.emptyETag != "" {
		etag = b.emptyETag
	}
	obj := Object{Key: p.Key, VersionID: version, ETag: etag, Size: p.Size, ModifiedAt: time.Date(2026, 11, 2, 0, 0, b.puts, 0, time.UTC), MediaType: p.MediaType, Metadata: p.Metadata}
	if b.versions[p.Key] == nil {
		b.versions[p.Key] = map[string][]byte{}
	}
	b.versions[p.Key][version] = bytes.Clone(data)
	b.current[p.Key] = obj
	if b.zeroPutMetadata {
		obj.ModifiedAt = time.Time{}
		obj.MediaType = ""
	}
	return obj, b.errorAfterPut
}
func (b *fakeBackend) Delete(ctx context.Context, key, etag string) (Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deletes++
	if b.deleteError != nil {
		return Object{}, b.deleteError
	}
	o, ok := b.current[key]
	if !ok || o.ETag != etag {
		return Object{}, fail("precondition_failed", "")
	}
	delete(b.current, key)
	return Object{Key: key, VersionID: "delete-marker"}, nil
}

type fakeTree struct {
	backend *fakeBackend
	root    string
}

func (t *fakeTree) Describe() storage.Descriptor {
	return storage.Descriptor{RangeRead: true, Names: storage.NameRules{CaseSensitive: true, MaxPathBytes: 1024}}
}
func (*fakeTree) Close() error { return nil }
func (t *fakeTree) Stat(ctx context.Context, path storage.Path) (storage.Entry, error) {
	if path == "" {
		return storage.Entry{Kind: storage.Folder}, nil
	}
	t.backend.mu.Lock()
	defer t.backend.mu.Unlock()
	key := t.root + string(path)
	if o, ok := t.backend.current[key]; ok {
		return storage.Entry{Path: path, Kind: storage.File, Size: o.Size, Revision: nativeRevision(o.VersionID)}, nil
	}
	if o, ok := t.backend.current[key+"/"]; ok {
		return storage.Entry{Path: path, Kind: storage.Folder, Revision: nativeRevision(o.VersionID)}, nil
	}
	for k := range t.backend.current {
		if strings.HasPrefix(k, key+"/") {
			return storage.Entry{Path: path, Kind: storage.Folder}, nil
		}
	}
	return storage.Entry{}, fail("not_found", path)
}
func (t *fakeTree) List(ctx context.Context, path storage.Path, req storage.PageRequest) (storage.Page, error) {
	if req.Cursor != "" {
		return storage.Page{}, fail("invalid_cursor", path)
	}
	t.backend.mu.Lock()
	defer t.backend.mu.Unlock()
	prefix := t.root
	if path != "" {
		prefix += string(path) + "/"
	}
	children := map[storage.Path]storage.Entry{}
	for k, o := range t.backend.current {
		if !strings.HasPrefix(k, prefix) || k == prefix {
			continue
		}
		relative := strings.TrimPrefix(k, prefix)
		segment, _, descendant := strings.Cut(relative, "/")
		p := storage.Path(segment)
		if path != "" {
			p = path + "/" + p
		}
		kind := storage.File
		size := o.Size
		if descendant {
			kind = storage.Folder
			size = 0
		}
		children[p] = storage.Entry{Path: p, Kind: kind, Size: size}
	}
	page := storage.Page{Entries: []storage.Entry{}}
	for _, e := range children {
		page.Entries = append(page.Entries, e)
	}
	sort.Slice(page.Entries, func(i, j int) bool { return page.Entries[i].Path < page.Entries[j].Path })
	return page, nil
}
func (t *fakeTree) Open(ctx context.Context, path storage.Path, opts storage.ReadOptions) (storage.Read, error) {
	e, err := t.Stat(ctx, path)
	if err != nil {
		return storage.Read{}, err
	}
	if e.Kind != storage.File {
		return storage.Read{}, fail("not_file", path)
	}
	version := ""
	if len(opts.PinnedVersion) > 0 {
		version, err = parseRevision(opts.PinnedVersion)
	} else {
		version, err = parseRevision(e.Revision)
	}
	if err != nil {
		return storage.Read{}, err
	}
	t.backend.mu.Lock()
	data, ok := t.backend.versions[t.root+string(path)][version]
	t.backend.mu.Unlock()
	if !ok {
		return storage.Read{}, fail("changed", path)
	}
	e.Revision = nativeRevision(version)
	e.NativeRef = []byte(t.root + string(path))
	e.Size = int64(len(data))
	length := e.Size - opts.Offset
	if opts.Length != nil {
		length = *opts.Length
	}
	if opts.Offset < 0 || length < 0 || opts.Offset+length > e.Size {
		return storage.Read{}, fail("invalid_range", path)
	}
	return storage.Read{Entry: e, Body: io.NopCloser(bytes.NewReader(data[opts.Offset : opts.Offset+length])), Offset: opts.Offset, Length: length}, nil
}
func (t *fakeTree) StatReference(ctx context.Context, path storage.Path, ref []byte) (storage.Entry, error) {
	if string(ref) != t.root+string(path) {
		return storage.Entry{}, fail("stale_reference", path)
	}
	return t.Stat(ctx, path)
}
func (t *fakeTree) OpenReference(ctx context.Context, path storage.Path, ref []byte, opts storage.ReadOptions) (storage.Read, error) {
	if string(ref) != t.root+string(path) {
		return storage.Read{}, fail("stale_reference", path)
	}
	return t.Open(ctx, path, opts)
}

func fixture(t *testing.T) (*Driver, *fakeBackend, storage.Config, Options) {
	t.Helper()
	b := newFake()
	cfg := storage.Config{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "drive/owner/", SystemRoot: "system/owner/"}
	options := Options{SpoolRoot: t.TempDir(), MaxPublishBytes: 1024, Admission: Admission{Version: AdmissionVersion, ConnectionID: cfg.ConnectionID, Endpoint: "https://fixture.invalid", Bucket: "bucket", Region: "region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated-positive-negative-test", AtomicPublish: true, ConditionalCreate: true, SingleGateway: true}}
	d, err := New(&fakeTree{backend: b, root: cfg.ContentRoot}, cfg, b, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, b, cfg, options
}
func cpOK(context.Context, []byte) error { return nil }
func createIntent(id, path, data string) storage.WriteIntent {
	return storage.WriteIntent{ID: id, Target: storage.Path(path), Conditions: storage.Conditions{Absent: true}, Size: int64(len(data)), MediaType: "application/octet-stream"}
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if !storage.IsCode(err, code) {
		t.Fatalf("error=%v want%s", err, code)
	}
}
func TestObjectPublicationDurabilityReplayAndDirectExit(t *testing.T) {
	d, b, cfg, opts := fixture(t)
	ctx := context.Background()
	in := createIntent("intent", " Spelling +%2F?# 中文.txt ", "raw\x00\xff")
	var savedReceipt []byte
	stage, err := d.Stage(ctx, in, strings.NewReader("raw\x00\xff"), func(_ context.Context, v []byte) error { savedReceipt = bytes.Clone(v); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if b.puts != 0 {
		t.Fatal("Stage touched final native path")
	}
	result, err := d.Publish(ctx, stage, func(_ context.Context, evidence []byte) error {
		var r receipt
		_ = json.Unmarshal(evidence, &r)
		if r.Phase == "applied" {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if err == nil || result.Entry == nil {
		t.Fatal("lost database ack not surfaced")
	}
	if b.puts != 1 {
		t.Fatal("publication replayed")
	}
	restarted, err := New(&fakeTree{backend: b, root: cfg.ContentRoot}, cfg, b, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	inspected, err := restarted.Inspect(ctx, in, savedReceipt)
	if err != nil || inspected.State != storage.Applied {
		t.Fatalf("lost ack recovery=%+v %v", inspected, err)
	}
	again, err := restarted.Publish(ctx, stage, cpOK)
	if err != nil || again.Entry.Path != in.Target || b.puts != 1 {
		t.Fatalf("duplicate publication=%+v %v count%d", again, err, b.puts)
	}
	body, err := b.Open(ctx, cfg.ContentRoot+string(in.Target), strings.TrimPrefix(string(result.Entry.Revision), "native-etag:"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if string(got) != "raw\x00\xff" {
		t.Fatal("native direct exit changed raw content")
	}
	// Unknown ordinary spool material is never treated as a cleanup candidate.
	if err = d.root.WriteFile("ordinary-untracked.data", []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Discard(ctx, in, savedReceipt); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Discard(ctx, in, savedReceipt); err != nil {
		t.Fatal(err)
	}
	if _, err = d.root.Stat("ordinary-untracked.data"); err != nil {
		t.Fatal("cleanup removed unknown material")
	}
	if _, err = b.Head(ctx, cfg.ContentRoot+string(in.Target)); err != nil {
		t.Fatal("cleanup deleted authoritative target")
	}
}
func TestObjectAmbiguousPublicationNeverUsesHeadToResolve(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			d, b, cfg, opts := fixture(t)
			ctx := context.Background()
			in := createIntent("uncertain", "file", "bytes")
			stage, err := d.Stage(ctx, in, strings.NewReader("bytes"), cpOK)
			if err != nil {
				t.Fatal(err)
			}
			if applied {
				b.errorAfterPut = io.ErrUnexpectedEOF
			} else {
				b.putError = io.ErrUnexpectedEOF
			}
			if _, err = d.Publish(ctx, stage, cpOK); err == nil {
				t.Fatal("ambiguous call succeeded")
			}
			heads := b.heads
			restarted, err := New(&fakeTree{backend: b, root: cfg.ContentRoot}, cfg, b, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			inspected, err := restarted.Inspect(ctx, in, stage.Handle)
			if err != nil || inspected.State != storage.OutcomeUnknown {
				t.Fatalf("uncertainty misresolved %+v %v", inspected, err)
			}
			if b.heads != heads {
				t.Fatal("HEAD used as effect attribution")
			}
			requireCode(t, restarted.Discard(ctx, in, stage.Handle), "outcome_unknown")
			_, err = restarted.Publish(ctx, stage, cpOK)
			requireCode(t, err, "outcome_unknown")
			if b.puts != 1 {
				t.Fatal("unknown native request replayed")
			}
		})
	}
}
func TestObjectBusyPublisherAndLateAbortKeepFence(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	in := createIntent("busy", "file", "late")
	stage, err := d.Stage(ctx, in, strings.NewReader("late"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	b.entered = make(chan struct{})
	b.block = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := d.Publish(ctx, stage, cpOK); done <- err }()
	<-b.entered
	inspected, err := d.Inspect(ctx, in, stage.Handle)
	if err != nil || inspected.State != storage.OutcomeUnknown {
		t.Fatal("busy native request did not block result")
	}
	requireCode(t, d.Discard(ctx, in, stage.Handle), "outcome_unknown")
	close(b.block)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	inspected, err = d.Inspect(ctx, in, stage.Handle)
	if err != nil || inspected.State != storage.Applied {
		t.Fatal("terminal native receipt missing")
	}
	never := createIntent("queued", "other", "no")
	inspected, err = d.Inspect(ctx, never, nil)
	if err != nil || inspected.State != storage.NotApplied {
		t.Fatal("never-started intent not fenced")
	}
	_, err = d.Stage(ctx, never, strings.NewReader("no"), cpOK)
	requireCode(t, err, "outcome_unknown")
	if b.puts != 1 {
		t.Fatal("sealed late worker changed native storage")
	}
}
func TestObjectMissingLedgerWithReceiptStaysUnknown(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	in := createIntent("restored", "file", "abc")
	stage, err := d.Stage(ctx, in, strings.NewReader("abc"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate restoring the gateway's ledger behind the durable service receipt.
	name := keyFor("write", in.ID) + ".json"
	if err = d.root.Remove(name); err != nil {
		t.Fatal(err)
	}
	inspected, err := d.Inspect(ctx, in, stage.Handle)
	if err != nil || inspected.State != storage.OutcomeUnknown {
		t.Fatalf("restored ledger guessed rollback %+v %v", inspected, err)
	}
	_, err = d.Publish(ctx, stage, cpOK)
	requireCode(t, err, "outcome_unknown")
	if b.puts != 0 {
		t.Fatal("restored receipt authorized fresh publication")
	}
}
func TestObjectStageRejectsBadBodiesCorruptionAndReuse(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		id, body string
		digest   *storage.Digest
		code     string
	}{{"short", "ab", nil, "size_mismatch"}, {"long", "abcd", nil, "size_mismatch"}, {"hash", "abc", &storage.Digest{Algorithm: "sha-256", Value: make([]byte, 32)}, "digest_mismatch"}} {
		in := createIntent(tc.id, tc.id, "abc")
		in.Digest = tc.digest
		_, err := d.Stage(ctx, in, strings.NewReader(tc.body), cpOK)
		requireCode(t, err, tc.code)
	}
	zero := createIntent("zero", "zero", "")
	stage, err := d.Stage(ctx, zero, strings.NewReader(""), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Publish(ctx, stage, cpOK); err != nil {
		t.Fatal(err)
	}
	in := createIntent("corrupt", "good", "abc")
	stage, err = d.Stage(ctx, in, strings.NewReader("abc"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Stage(ctx, in, strings.NewReader("bad"), cpOK)
	requireCode(t, err, "intent_conflict")
	r, err := d.load("write", in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.root.WriteFile(d.dataName(r), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = d.Publish(ctx, stage, cpOK)
	requireCode(t, err, "digest_mismatch")
	if b.puts != 1 {
		t.Fatal("corrupt body reached provider")
	}
}
func TestObjectCopyFoldersAndCapturedCurrentDelete(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	folder, err := d.Mkdir(ctx, storage.Mutation{ID: "mkdir", Path: "Empty", Conditions: storage.Conditions{Absent: true}}, cpOK)
	if err != nil || folder.Entry.Kind != storage.Folder {
		t.Fatalf("mkdir %+v %v", folder, err)
	}
	in := createIntent("source", "source", "independent")
	stage, err := d.Stage(ctx, in, strings.NewReader("independent"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	source, err := d.Publish(ctx, stage, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := d.Copy(ctx, storage.MoveRequest{ID: "copy", Source: "source", Target: "copy", SourceRevision: source.Entry.Revision, TargetConditions: storage.Conditions{Absent: true}}, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Remove(ctx, storage.Mutation{ID: "delete", Path: "source", Conditions: storage.Conditions{ExpectedRevision: source.Entry.Revision}}, cpOK); err != nil {
		t.Fatal(err)
	}
	body, err := b.Open(ctx, string(copied.Entry.NativeRef), strings.TrimPrefix(string(copied.Entry.Revision), "native-etag:"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "independent" {
		t.Fatal("copy became source alias")
	}
	current, err := d.Stat(ctx, "Empty")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current.Revision, folder.Entry.Revision) {
		t.Fatal("folder marker native version mismatch")
	}
	if _, err = d.Remove(ctx, storage.Mutation{ID: "delete-empty", Path: "Empty", Conditions: storage.Conditions{ExpectedRevision: current.Revision}}, cpOK); err != nil {
		t.Fatal(err)
	}
	implicit := createIntent("implicit", "Implicit/child", "x")
	b.current["drive/owner/Implicit/child"] = Object{Key: "drive/owner/Implicit/child", ETag: "external", Size: 1}
	prefix, err := d.Stat(ctx, "Implicit")
	if err != nil || prefix.Kind != storage.Folder || prefix.Revision != nil {
		t.Fatalf("implicit prefix folder=%+v %v", prefix, err)
	}
	_, err = d.Stage(ctx, implicit, strings.NewReader("x"), cpOK)
	requireCode(t, err, "precondition_failed")
	inspection, err := d.InspectEffect(ctx, storage.EffectIntent{ID: "copy", Action: "copy", Source: "source", Target: "copy", SourceRevision: source.Entry.Revision, Conditions: storage.Conditions{Absent: true}}, copied.Evidence)
	if err != nil || inspection.State != storage.Applied {
		t.Fatalf("copy recovery=%+v %v", inspection, err)
	}
}
func TestObjectAdmissionAndSpoolSymlinkFailClosed(t *testing.T) {
	d, b, cfg, opts := fixture(t)
	_ = d
	opts.Admission.SingleGateway = false
	if _, err := New(&fakeTree{backend: b, root: cfg.ContentRoot}, cfg, b, opts); err == nil {
		t.Fatal("unadmitted native writer enabled")
	}
	opts.Admission.SingleGateway = true
	opts.Admission.ConditionalCreate = false
	if _, err := New(&fakeTree{backend: b, root: cfg.ContentRoot}, cfg, b, opts); err == nil {
		t.Fatal("unverified conditional create admitted")
	}
	opts.Admission.ConditionalCreate = true
	in := createIntent("symlink", "target", "secret")
	attempt := "00000000000000000000000000000001"
	r := &ledger{Kind: "write", ID: in.ID, Intent: in, Attempt: attempt, Phase: "ready", Digest: &storage.Digest{Algorithm: "sha-256", Value: make([]byte, 32)}}
	outside := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(opts.SpoolRoot, d.namespace, d.dataName(r))); err != nil {
		t.Fatal(err)
	}
	if err := d.save(r); err != nil {
		t.Fatal(err)
	}
	_, err := d.Publish(context.Background(), d.staged(r), cpOK)
	if err == nil {
		t.Fatal("spool symlink was followed")
	}
	if b.puts != 0 {
		t.Fatal("unsafe spool published")
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "secret" {
		t.Fatal("outside bytes changed")
	}
	modified := time.Now()
	in = createIntent("mtime", "mtime", "x")
	in.SourceModifiedAt = &modified
	_, err = d.Stage(context.Background(), in, strings.NewReader("x"), cpOK)
	requireCode(t, err, "unsupported")
}
func TestObjectBlockedBodyCancellationReleasesNativeFence(t *testing.T) {
	d, _, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	received := make(chan struct{})
	done := make(chan error, 1)
	in := createIntent("blocked", "file", "abc")
	go func() {
		_, err := d.Stage(ctx, in, reader, func(context.Context, []byte) error {
			select {
			case <-received:
			default:
				close(received)
			}
			return nil
		})
		done <- err
	}()
	<-received
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled stage acknowledged")
		}
	case <-time.After(time.Second):
		t.Fatal("blocking input ignored cancellation")
	}
	inspected, err := d.Inspect(context.Background(), in, nil)
	if err != nil || inspected.State != storage.NotApplied {
		t.Fatalf("abandoned receiver retains lock %+v %v", inspected, err)
	}
}
func TestObjectConditionalFailureKnownAndOtherErrorsUnknown(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	in := createIntent("condition", "target", "x")
	stage, err := d.Stage(ctx, in, strings.NewReader("x"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	b.putError = fail("precondition_failed", "")
	_, err = d.Publish(ctx, stage, cpOK)
	requireCode(t, err, "precondition_failed")
	inspected, err := d.Inspect(ctx, in, stage.Handle)
	if err != nil || inspected.State != storage.NotApplied {
		t.Fatal("explicit terminal condition rejection not recorded")
	}
}
func TestObjectLateCheckpointTargetInsertionNeverOverwrites(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	in := createIntent("late", "target", "new")
	stage, err := d.Stage(ctx, in, strings.NewReader("new"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Publish(ctx, stage, func(context.Context, []byte) error {
		_, e := b.Put(ctx, Put{Key: "drive/owner/target", Body: strings.NewReader("old"), Size: 3, MediaType: "text/plain"})
		return e
	})
	if err == nil {
		t.Fatal("late occupied target overwritten")
	}
	obj, _ := b.Head(ctx, "drive/owner/target")
	body, _ := b.Open(ctx, obj.Key, obj.ETag)
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "old" {
		t.Fatal("target bytes pre-deleted")
	}
	if b.puts != 1 {
		t.Fatal("conditional preflight allowed second final request")
	}
}
func TestObjectReceiptDifferentRootAndIDRejected(t *testing.T) {
	d, _, _, _ := fixture(t)
	ctx := context.Background()
	in := createIntent("one", "file", "x")
	stage, err := d.Stage(ctx, in, strings.NewReader("x"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	in.ID = "two"
	_, err = d.Inspect(ctx, in, stage.Handle)
	requireCode(t, err, "invalid_evidence")
	var evidence receipt
	if err = json.Unmarshal(stage.Handle, &evidence); err != nil {
		t.Fatal(err)
	}
	evidence.Binding = "other"
	raw, _ := json.Marshal(evidence)
	in.ID = "one"
	_, err = d.Inspect(ctx, in, raw)
	requireCode(t, err, "invalid_evidence")
}

func TestReferenceReadsUseTheProvenKey(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	key := "drive/owner/file.txt"
	b.mu.Lock()
	b.current[key] = Object{Key: key, VersionID: "v1", ETag: "etag-v1", Size: 3, MediaType: "text/plain"}
	b.versions[key] = map[string][]byte{"v1": []byte("abc")}
	heads := b.heads
	b.mu.Unlock()

	reader := storage.ReferenceReader(d)
	entry, err := reader.StatReference(ctx, "file.txt", []byte(key))
	if err != nil || entry.Size != 3 || string(entry.Revision) != "native-etag:etag-v1" || string(entry.NativeRef) != key {
		t.Fatalf("reference stat: %+v %v", entry, err)
	}
	b.mu.Lock()
	observed := b.heads
	b.mu.Unlock()
	if observed != heads+1 {
		t.Fatalf("reference stat made %d native HEADs", observed-heads)
	}
	// A key from another path or root generation is stale evidence, never a redirect.
	for _, ref := range []string{"drive/owner/other.txt", "drive/other/file.txt", ""} {
		if _, err := reader.StatReference(ctx, "file.txt", []byte(ref)); !storage.IsCode(err, "stale_reference") {
			t.Fatalf("stat accepted foreign key %q: %v", ref, err)
		}
		if _, err := reader.OpenReference(ctx, "file.txt", []byte(ref), storage.ReadOptions{}); !storage.IsCode(err, "stale_reference") {
			t.Fatalf("open accepted foreign key %q: %v", ref, err)
		}
	}
	b.mu.Lock()
	afterForeign := b.heads
	b.mu.Unlock()
	if afterForeign != observed {
		t.Fatal("foreign keys reached native storage")
	}

	length := int64(1)
	read, err := reader.OpenReference(ctx, "file.txt", []byte(key), storage.ReadOptions{Offset: 1, Length: &length, PinnedVersion: entry.Revision})
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(read.Body)
	closeErr := read.Body.Close()
	if readErr != nil || closeErr != nil || string(data) != "b" || read.Length != 1 || !bytes.Equal(read.Entry.Revision, entry.Revision) {
		t.Fatalf("reference read: %q %+v %v %v", data, read, readErr, closeErr)
	}
}
