package objectmutation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"everythingabout.me/backend/internal/storage"
)

// Driver is a single-gateway admitted view over an existing read driver. Spool
// intent records and locks are recovery evidence, never user data. A settled record
// is kept for storage.IntentRetention and then pruned; a non-terminal one is never
// aged out because its native effect may still land. The caller must preserve this
// spool across restarts and database restoration.
type Driver struct {
	tree      storage.Driver
	binding   storage.Config
	backend   Backend
	options   Options
	root      *os.Root
	namespace string
	lifetime  context.Context
	cancel    context.CancelFunc
	once      sync.Once
	closeErr  error
	// stageBound is the largest single entry this writer may stage. It starts at the
	// connection's interactive single-PUT policy; a caller that derives a larger bound
	// from its own work raises it through AdmitStagedBytes.
	stageBound atomic.Int64
}

var _ storage.Driver = (*Driver)(nil)
var _ storage.BoundStagedWriter = (*Driver)(nil)
var _ storage.StagingBound = (*Driver)(nil)
var _ storage.BoundEffectRecovery = (*Driver)(nil)
var _ storage.FolderWriter = (*Driver)(nil)
var _ storage.Remover = (*Driver)(nil)
var _ storage.NativeMover = (*Driver)(nil)
var _ storage.NativeCopier = (*Driver)(nil)

func New(tree storage.Driver, binding storage.Config, backend Backend, options Options) (*Driver, error) {
	a := options.Admission
	if err := ValidateAdmission(a); err != nil {
		return nil, err
	}
	if tree == nil || backend == nil || storage.ValidateBinding(binding) != nil {
		return nil, fail("unsupported", "")
	}
	if a.ConnectionID != binding.ConnectionID {
		return nil, fail("invalid_config", "")
	}
	for _, p := range []string{binding.ContentRoot, binding.SystemRoot} {
		if p == "" && binding.RootAtBucket {
			// The tree is the bucket itself. Every other prefix rule still applies to the
			// system root below; only the content prefix has nothing to validate.
			continue
		}
		if !strings.HasSuffix(p, "/") || strings.HasPrefix(p, "/") || storage.ValidatePath(storage.Path(strings.TrimSuffix(p, "/"))) != nil {
			return nil, fail("invalid_config", "")
		}
	}
	// A bucket-rooted tree necessarily contains the private system prefix. The driver
	// refuses that name instead of hiding it (objecttree.Bind advertises it as reserved),
	// so the containment is bounded rather than ignored.
	if !binding.RootAtBucket && (strings.HasPrefix(binding.ContentRoot, binding.SystemRoot) || strings.HasPrefix(binding.SystemRoot, binding.ContentRoot)) {
		return nil, fail("invalid_config", "")
	}
	if options.MaxPublishBytes == 0 {
		options.MaxPublishBytes = 32 << 20
	}
	if options.MaxPublishBytes < 1 || options.MaxPublishBytes > 5<<30 || !filepath.IsAbs(options.SpoolRoot) {
		return nil, fail("invalid_config", "")
	}
	canonical, err := filepath.EvalSymlinks(options.SpoolRoot)
	if err != nil {
		return nil, err
	}
	base, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	if err = admitSpool(base); err != nil {
		return nil, err
	}
	binding.Credentials = nil
	identity, _ := json.Marshal(binding)
	hash := sha256.Sum256(identity)
	namespace := "object-mutations-v1-" + hex.EncodeToString(hash[:])
	if err = base.Mkdir(namespace, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	root, err := privateRoot(base, namespace)
	if err != nil {
		return nil, err
	}
	if err = syncDirectory(base); err != nil {
		root.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Driver{tree: tree, binding: binding, backend: backend, options: options, root: root, namespace: namespace, lifetime: ctx, cancel: cancel}
	d.stageBound.Store(options.MaxPublishBytes)
	return d, nil
}
func fail(code string, path storage.Path) error  { return storage.NewError(code, path, nil) }
func (d *Driver) StorageBinding() storage.Config { return d.binding }
func (d *Driver) StorageNameRules() storage.NameRules {
	n := d.tree.Describe().Names
	n.ReservedNames = append([]string(nil), n.ReservedNames...)
	return n
}
func (d *Driver) Describe() storage.Descriptor {
	desc := d.tree.Describe()
	desc.MaxFileBytes = d.options.MaxPublishBytes
	desc.AtomicCreate = true
	desc.AtomicReplace = true
	// Revisions are current-object ETags, so a read is validated, never pinned,
	// and the bucket's versioning setting is irrelevant to correctness.
	desc.PinnedRead = false
	desc.NativeConditionalMutation = false
	desc.NativeTreeMove = false
	desc.RetainsVersions = false
	// Class A needs both part signing and read grants. A driver that cannot issue
	// one half of the contract must not advertise the class.
	desc.TransferClasses = []string{storage.TransferRelay}
	if d.SignedUploadSupported() && d.ReadGrantSupported() {
		desc.TransferClasses = []string{storage.TransferPresignedParts, storage.TransferRelay}
	}
	desc.Names = d.StorageNameRules()
	return desc
}

// AdmitStagedBytes raises the single-entry staging ceiling to a bound the caller
// computed from its own work (see storage.StagingBound). A whole-tree copy stages one
// entry at a time, so the bound it declares is each entry it is about to hand over;
// the connection's interactive single-PUT policy describes a request a person made,
// not that copy, and refusing the copy at its first larger file would stop a backup
// midway. The provider's own one-request publication limit is not negotiable, so a
// larger entry is refused by name instead of failing later inside a native PUT.
func (d *Driver) AdmitStagedBytes(size int64) error {
	if size < 0 {
		return fail("invalid_intent", "")
	}
	if size > singleRequestLimit {
		return storage.NewError("too_large", "", fmt.Errorf("entry of %d bytes exceeds the %d-byte single-request publication limit", size, singleRequestLimit))
	}
	for {
		current := d.stageBound.Load()
		if size <= current || d.stageBound.CompareAndSwap(current, size) {
			return nil
		}
	}
}

// Close closes this view and its underlying tree.
func (d *Driver) Close() error {
	d.once.Do(func() { d.cancel(); d.closeErr = errors.Join(d.root.Close(), d.tree.Close()) })
	return d.closeErr
}
func (d *Driver) operation(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if d.lifetime.Err() != nil {
		return nil, nil, fail("closed", "")
	}
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.lifetime, cancel)
	return child, func() { stop(); cancel() }, nil
}
func (d *Driver) key(p storage.Path, kind storage.Kind) string {
	key := d.binding.ContentRoot + string(p)
	if kind == storage.Folder {
		key += "/"
	}
	return key
}

// nativeRevision derives the revision token from the current object's ETag.
// Every conditional native request (If-Match / If-None-Match) uses the ETag, so
// bucket versioning is neither required nor consulted.
func nativeRevision(etag string) []byte {
	if etag == "" || len(etag) > 1024 || strings.ContainsAny(etag, "\r\n\x00") {
		return nil
	}
	return []byte("native-etag:" + etag)
}
func parseRevision(value []byte) (string, error) {
	if len(value) > 4096 || !bytes.HasPrefix(value, []byte("native-etag:")) {
		return "", fail("unsupported", "")
	}
	s := string(bytes.TrimPrefix(value, []byte("native-etag:")))
	if len(nativeRevision(s)) == 0 {
		return "", fail("unsupported", "")
	}
	return s, nil
}
func (d *Driver) objectEntry(p storage.Path, kind storage.Kind, obj Object) (storage.Entry, error) {
	// A folder is its key (the trailing-slash marker). Its body is not user content:
	// legacy markers are empty, while a marker reasserted during a purge carries a small
	// token so its ETag moves — a zero-byte rewrite would leave the empty-content MD5
	// unchanged and a stale conditional delete would still match it.
	if obj.Key != d.key(p, kind) || obj.Size < 0 || len(nativeRevision(obj.ETag)) == 0 {
		return storage.Entry{}, fail("unsupported", p)
	}
	modified := obj.ModifiedAt
	e := storage.Entry{Path: p, Kind: kind, NativeRef: []byte(obj.Key), Revision: nativeRevision(obj.ETag), Size: obj.Size, ModifiedAt: modified, MediaType: obj.MediaType}
	if kind == storage.Folder {
		e.Size = 0
		e.MediaType = ""
	}
	return e, nil
}
func (d *Driver) rootEntry(e storage.Entry) storage.Entry {
	e.Path = ""
	e.Kind = storage.Folder
	e.Size = 0
	e.NativeRef = []byte(d.binding.ContentRoot)
	h := sha256.Sum256([]byte(d.namespace))
	e.Revision = []byte("object-root:" + hex.EncodeToString(h[:]))
	return e
}
func (d *Driver) Stat(ctx context.Context, p storage.Path) (storage.Entry, error) {
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.Entry{}, err
	}
	defer done()
	if err = storage.ValidatePath(p); err != nil {
		return storage.Entry{}, err
	}
	if p == "" {
		e, err := d.tree.Stat(ctx, p)
		if err != nil {
			return storage.Entry{}, err
		}
		return d.rootEntry(e), nil
	}
	// Byte-exact HEADs settle both real kinds, so nothing lists an ancestor. A
	// folder that was created here has its own marker object (key + "/"), and a
	// folder that has one is the common case in this tree, so it is asked first;
	// a file costs the missed marker probe. The ETag from either HEAD is the
	// revision a conditional effect compares — the one thing a listing may not
	// establish.
	marker, markerErr := d.backend.Head(ctx, d.key(p, storage.Folder))
	if markerErr == nil {
		return d.objectEntry(p, storage.Folder, marker)
	}
	if !storage.IsCode(markerErr, "not_found") {
		return storage.Entry{}, markerErr
	}
	obj, headErr := d.backend.Head(ctx, d.key(p, storage.File))
	if headErr == nil {
		return d.objectEntry(p, storage.File, obj)
	}
	if !storage.IsCode(headErr, "not_found") {
		return storage.Entry{}, headErr
	}
	// Both byte-exact probes above already proved this path has neither a marker
	// object nor a file, so the only remaining native folder is a prefix folder: it
	// exists exactly while it has a child. Ask the tree for that one bounded listing
	// instead of re-observing the two keys that just answered 404 — that was three
	// more HEADs plus the same listing, per observation.
	if probe, ok := d.tree.(prefixFolderReader); ok {
		if _, err := probe.PrefixFolder(ctx, p); err != nil {
			return storage.Entry{}, err
		}
		// A prefix folder has no marker object, so it has no revision and no body.
		return storage.Entry{Path: p, Kind: storage.Folder, NativeRef: []byte(d.binding.ContentRoot + string(p))}, nil
	}
	e, err := d.tree.Stat(ctx, p)
	if err != nil {
		return storage.Entry{}, err
	}
	if e.Path != p || e.Kind != storage.Folder {
		return storage.Entry{}, fail("invalid_backend_response", p)
	}
	// A folder marker is a real object, so it still carries a revision.
	return d.observe(ctx, e)
}

// prefixFolderReader is the optional single-prefix probe objecttree.Tree offers.
// The mutation driver's own two HEADs are the whole observation except for this
// case; drivers that do not declare it fall back to the path-addressed Stat.
type prefixFolderReader interface {
	PrefixFolder(context.Context, storage.Path) (storage.Entry, error)
}

// observe attaches the current native revision. A prefix folder without a marker
// object is a real native folder; it has no revision and cannot be conditionally
// deleted, but children can still be created below it.
func (d *Driver) observe(ctx context.Context, e storage.Entry) (storage.Entry, error) {
	obj, err := d.backend.Head(ctx, d.key(e.Path, e.Kind))
	if storage.IsCode(err, "not_found") && e.Kind == storage.Folder {
		e.Revision = nil
		e.Size = 0
		e.MediaType = ""
		return e, nil
	}
	if err != nil {
		return storage.Entry{}, err
	}
	return d.objectEntry(e.Path, e.Kind, obj)
}

// listedRevision is the revision a listing already proved, in the same spelling
// observe would produce from its own HEAD, so the two are interchangeable.
func (d *Driver) listedRevision(e storage.Entry) []byte {
	if len(e.Revision) > 0 {
		return e.Revision
	}
	// A listing's ETag is only as good as the endpoint's promise that it equals the
	// one a HEAD returns; that promise is probed evidence, never an assumption about
	// a provider. Without it the entry is observed like any other.
	if !d.options.Admission.ListingETag || len(e.NativeETag) == 0 {
		return nil
	}
	return nativeRevision(string(e.NativeETag))
}

// Purge is the cheap deletion a settled purge uses: one conditional request, no ledger and
// no re-observation. See storage.Purger for why those two are the only things it may skip.
func (d *Driver) Purge(ctx context.Context, p storage.Path, kind storage.Kind, revision []byte) error {
	if p == "" || storage.ValidatePath(p) != nil || (kind != storage.File && kind != storage.Folder) {
		return fail("invalid_name", p)
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return err
	}
	defer done()
	// The conditional request names the raw ETag, not the revision token that carries it.
	expected := ""
	if len(revision) > 0 {
		if expected, err = parseRevision(revision); err != nil {
			return err
		}
	}
	// The marker of a folder is one object like any other; its children are separate keys
	// and the walk already removed them, so there is no emptiness to check here.
	_, err = d.backend.Delete(ctx, d.key(p, kind), expected)
	if err == nil {
		return nil
	}
	if storage.IsCode(err, "precondition_failed") {
		return fail("changed", p)
	}
	return err
}

func (d *Driver) List(ctx context.Context, p storage.Path, req storage.PageRequest) (storage.Page, error) {
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.Page{}, err
	}
	defer done()
	page, err := d.tree.List(ctx, p, req)
	if err != nil {
		return storage.Page{}, err
	}
	out := storage.Page{NextCursor: page.NextCursor, Entries: make([]storage.Entry, 0, len(page.Entries))}
	for _, e := range page.Entries {
		if storage.ValidatePath(e.Path) != nil || e.Path == "" || parentPath(e.Path) != p {
			return storage.Page{}, fail("invalid_backend_response", p)
		}
		if e.Kind != storage.File && e.Kind != storage.Folder {
			return storage.Page{}, fail("invalid_backend_response", e.Path)
		}
		// A listing that already reported each entry's ETag has answered the one
		// question this driver's revision asks. Observing it again is a HEAD per
		// child that re-reads bytes we hold: one folder of a thousand files cost a
		// thousand and one requests and answered nothing new. Entries the listing
		// could not name (a common prefix is not an object) still observe.
		if revision := d.listedRevision(e); len(revision) > 0 {
			e.NativeETag = nil
			e.Revision = revision
			out.Entries = append(out.Entries, e)
			continue
		}
		observed, err := d.observe(ctx, e)
		if err != nil {
			return storage.Page{}, err
		}
		out.Entries = append(out.Entries, observed)
	}
	return out, nil
}
func (d *Driver) Open(ctx context.Context, p storage.Path, options storage.ReadOptions) (storage.Read, error) {
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.Read{}, err
	}
	if err = storage.ValidatePath(p); err != nil || p == "" {
		done()
		return storage.Read{}, fail("not_file", p)
	}
	obj, err := d.backend.Head(ctx, d.key(p, storage.File))
	if err != nil {
		done()
		return storage.Read{}, err
	}
	current, err := d.objectEntry(p, storage.File, obj)
	if err != nil {
		done()
		return storage.Read{}, err
	}
	if len(options.PinnedVersion) > 0 && !bytes.Equal(options.PinnedVersion, current.Revision) {
		done()
		return storage.Read{}, fail("changed", p)
	}
	treeOptions := options
	treeOptions.PinnedVersion = nil
	read, err := d.tree.Open(ctx, p, treeOptions)
	if err != nil {
		done()
		return storage.Read{}, err
	}
	if read.Body == nil || read.Entry.Path != p || read.Entry.Kind != storage.File || read.Entry.Size != current.Size {
		if read.Body != nil {
			read.Body.Close()
		}
		done()
		return storage.Read{}, fail("changed", p)
	}
	read.Entry.Revision = current.Revision
	read.Body = &operationBody{ReadCloser: read.Body, done: done}
	return read, nil
}

// StatReference observes one file through an index-proved key: one HEAD, no ancestor
// listing. The reference must be the key this driver derives for the path, otherwise
// the index describes a different native root and the read fails closed.
func (d *Driver) StatReference(ctx context.Context, p storage.Path, ref []byte) (storage.Entry, error) {
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.Entry{}, err
	}
	defer done()
	if err = storage.ValidatePath(p); err != nil || p == "" {
		return storage.Entry{}, fail("not_file", p)
	}
	if !bytes.Equal(ref, []byte(d.key(p, storage.File))) {
		return storage.Entry{}, fail("stale_reference", p)
	}
	obj, err := d.backend.Head(ctx, d.key(p, storage.File))
	if err != nil {
		return storage.Entry{}, err
	}
	return d.objectEntry(p, storage.File, obj)
}

// OpenReference transfers through the same index-proved key in one ranged GET. The
// returned entry carries the revision the caller observed with StatReference: native
// object reads are validated but not version-pinned, exactly as Open documents.
func (d *Driver) OpenReference(ctx context.Context, p storage.Path, ref []byte, options storage.ReadOptions) (storage.Read, error) {
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.Read{}, err
	}
	if err = storage.ValidatePath(p); err != nil || p == "" {
		done()
		return storage.Read{}, fail("not_file", p)
	}
	if !bytes.Equal(ref, []byte(d.key(p, storage.File))) {
		done()
		return storage.Read{}, fail("stale_reference", p)
	}
	tree, ok := d.tree.(storage.ReferenceReader)
	if !ok {
		done()
		return storage.Read{}, fail("unsupported", p)
	}
	treeOptions := options
	// The tree's version is a provider version id; this driver's revision is the ETag.
	treeOptions.PinnedVersion = nil
	read, err := tree.OpenReference(ctx, p, ref, treeOptions)
	if err != nil {
		done()
		return storage.Read{}, err
	}
	if read.Body == nil || read.Entry.Path != p || read.Entry.Kind != storage.File {
		if read.Body != nil {
			read.Body.Close()
		}
		done()
		return storage.Read{}, fail("changed", p)
	}
	read.Entry.Revision = bytes.Clone(options.PinnedVersion)
	read.Body = &operationBody{ReadCloser: read.Body, done: done}
	return read, nil
}

type operationBody struct {
	io.ReadCloser
	once sync.Once
	done func()
	err  error
}

func (b *operationBody) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close(); b.done() })
	return b.err
}
func parentPath(p storage.Path) storage.Path {
	if i := strings.LastIndexByte(string(p), '/'); i >= 0 {
		return p[:i]
	}
	return ""
}
func cloneIntent(in storage.WriteIntent) storage.WriteIntent {
	in.Conditions.ExpectedRevision = bytes.Clone(in.Conditions.ExpectedRevision)
	in.ParentRevision = bytes.Clone(in.ParentRevision)
	if in.Digest != nil {
		in.Digest = &storage.Digest{Algorithm: in.Digest.Algorithm, Value: bytes.Clone(in.Digest.Value)}
	}
	if in.SourceModifiedAt != nil {
		t := *in.SourceModifiedAt
		in.SourceModifiedAt = &t
	}
	return in
}
func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func validateRecoveryIntent(in storage.WriteIntent) error {
	if in.ID == "" || len(in.ID) > 1024 || strings.ContainsAny(in.ID, "\r\n\x00") || in.Target == "" || storage.ValidatePath(in.Target) != nil || in.Size < 0 || in.Size > 9007199254740991 || len(in.MediaType) > 4096 || len(in.Conditions.ExpectedRevision) > 4096 || in.Conditions.Absent == (len(in.Conditions.ExpectedRevision) > 0) {
		return fail("invalid_intent", in.Target)
	}
	return nil
}
func (d *Driver) validateIntent(in storage.WriteIntent) error {
	if err := validateRecoveryIntent(in); err != nil {
		return err
	}
	if in.SourceModifiedAt != nil {
		return fail("unsupported", in.Target)
	}
	if len(d.binding.ContentRoot)+len(in.Target) > 1024 {
		return fail("invalid_name", in.Target)
	}
	if in.ID == "" || len(in.ID) > 1024 || strings.ContainsAny(in.ID, "\r\n\x00") || in.Target == "" || storage.ValidatePath(in.Target) != nil || in.Size < 0 || in.Size > d.stageBound.Load() || in.Conditions.ImplicitFolder || len(in.MediaType) > 1024 || in.Conditions.Absent == (len(in.Conditions.ExpectedRevision) > 0) {
		return fail("invalid_intent", in.Target)
	}
	if len(in.Conditions.ExpectedRevision) > 0 {
		if _, err := parseRevision(in.Conditions.ExpectedRevision); err != nil {
			return err
		}
	}
	if in.Digest != nil && (in.Digest.Algorithm != "sha-256" || len(in.Digest.Value) != sha256.Size) {
		return fail("unsupported", in.Target)
	}
	return nil
}
func randomAttempt() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func (d *Driver) Stage(ctx context.Context, in storage.WriteIntent, body io.Reader, cp storage.Checkpoint) (storage.Staged, error) {
	if err := d.validateIntent(in); err != nil {
		return storage.Staged{}, err
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.Staged{}, err
	}
	defer done()
	unlock, err := d.lock(ctx, "write", in.ID, false)
	if err != nil {
		return storage.Staged{}, err
	}
	defer unlock()
	r, err := d.load("write", in.ID)
	if errors.Is(err, os.ErrNotExist) {
		r = &ledger{Binding: d.namespace, Kind: "write", ID: in.ID, Intent: cloneIntent(in), Phase: "new"}
	} else if err != nil {
		return storage.Staged{}, err
	}
	if !sameJSON(r.Intent, in) {
		return storage.Staged{}, fail("intent_conflict", in.Target)
	}
	return d.stageLocked(ctx, r, body, cp)
}
func (d *Driver) stageLocked(ctx context.Context, r *ledger, body io.Reader, cp storage.Checkpoint) (storage.Staged, error) {
	if body == nil {
		return storage.Staged{}, fail("invalid_body", r.Intent.Target)
	}
	var closeOnce sync.Once
	closeInput := func() {
		if c, ok := body.(io.ReadCloser); ok {
			closeOnce.Do(func() { _ = c.Close() })
		}
	}
	stop := context.AfterFunc(ctx, closeInput)
	defer func() {
		if !stop() {
			closeInput()
		}
	}()
	if r.Phase == "ready" || r.Phase == "applied" {
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(&ctxReader{ctx, body}, r.Intent.Size+1))
		if err != nil {
			return storage.Staged{}, err
		}
		if n != r.Intent.Size || r.Digest == nil || !bytes.Equal(hash.Sum(nil), r.Digest.Value) {
			return storage.Staged{}, fail("intent_conflict", r.Intent.Target)
		}
		return d.staged(r), nil
	}
	if r.Phase != "new" {
		return storage.Staged{}, fail("outcome_unknown", r.Intent.Target)
	}
	parent, err := d.Stat(ctx, parentPath(r.Intent.Target))
	if storage.IsCode(err, "not_found") {
		return storage.Staged{}, fail("parent_missing", r.Intent.Target)
	}
	if err != nil {
		return storage.Staged{}, err
	}
	if parent.Kind != storage.Folder {
		return storage.Staged{}, fail("not_folder", r.Intent.Target)
	}
	if len(r.Intent.ParentRevision) > 0 && !bytes.Equal(parent.Revision, r.Intent.ParentRevision) {
		return storage.Staged{}, fail("changed", r.Intent.Target)
	}
	r.ParentRevision = bytes.Clone(parent.Revision)
	if err = d.checkTarget(ctx, r); err != nil {
		return storage.Staged{}, err
	}
	r.Attempt, err = randomAttempt()
	if err != nil {
		return storage.Staged{}, err
	}
	r.Phase = "receiving"
	if err = d.save(r); err != nil {
		return storage.Staged{}, err
	}
	if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.Staged{}, err
	}
	file, err := spoolOpen(d.root, d.dataName(r), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return storage.Staged{}, err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(&ctxReader{ctx, body}, r.Intent.Size+1))
	if copyErr == nil && n != r.Intent.Size {
		copyErr = fail("size_mismatch", r.Intent.Target)
	}
	if copyErr == nil && r.Intent.Digest != nil && !bytes.Equal(r.Intent.Digest.Value, hash.Sum(nil)) {
		copyErr = fail("digest_mismatch", r.Intent.Target)
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		r.Phase = "not_applied"
		_ = d.save(r)
		return storage.Staged{}, copyErr
	}
	if err = syncDirectory(d.root); err != nil {
		return storage.Staged{}, err
	}
	r.Digest = &storage.Digest{Algorithm: "sha-256", Value: hash.Sum(nil)}
	r.Phase = "ready"
	if err = d.save(r); err != nil {
		return storage.Staged{}, err
	}
	if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.Staged{}, err
	}
	return d.staged(r), nil
}

type ctxReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func checkpoint(ctx context.Context, cp storage.Checkpoint, evidence []byte) error {
	if cp == nil {
		return fail("invalid_checkpoint", "")
	}
	return cp(ctx, bytes.Clone(evidence))
}
func (d *Driver) staged(r *ledger) storage.Staged {
	return storage.Staged{Intent: cloneIntent(r.Intent), Handle: d.receipt(r), Size: r.Intent.Size, Digest: cloneDigest(r.Digest)}
}
func cloneDigest(v *storage.Digest) *storage.Digest {
	if v == nil {
		return nil
	}
	return &storage.Digest{Algorithm: v.Algorithm, Value: bytes.Clone(v.Value)}
}

// parentMoved answers whether the destination's parent still matches what this mutation
// bound. A promised parent (Intent.Parents) has no native representation until its first
// child lands, so being absent is the state it was admitted in, not a change: an object
// store creates the folder with the very write this check is guarding.
func (d *Driver) parentMoved(ctx context.Context, r *ledger) error {
	parent, err := d.Stat(ctx, parentPath(r.Intent.Target))
	if err != nil {
		if storage.IsCode(err, "not_found") && r.Intent.Parents {
			return nil
		}
		return err
	}
	if parent.Kind != storage.Folder || !bytes.Equal(parent.Revision, r.ParentRevision) {
		return fail("changed", r.Intent.Target)
	}
	return nil
}

func (d *Driver) checkTarget(ctx context.Context, r *ledger) error {
	current, err := d.tree.Stat(ctx, r.Intent.Target)
	if err == nil {
		expectedKind := storage.File
		if r.Kind == "mkdir" {
			expectedKind = storage.Folder
		}
		if r.Intent.Conditions.Absent {
			return fail("precondition_failed", r.Intent.Target)
		}
		if current.Kind != expectedKind {
			return fail("precondition_failed", r.Intent.Target)
		}
	} else if !storage.IsCode(err, "not_found") {
		return err
	} else if r.Intent.Conditions.Absent {
		return nil
	} else {
		return fail("changed", r.Intent.Target)
	}
	obj, err := d.backend.Head(ctx, d.key(r.Intent.Target, storage.File))
	if err != nil {
		return err
	}
	if !bytes.Equal(nativeRevision(obj.ETag), r.Intent.Conditions.ExpectedRevision) {
		return fail("changed", r.Intent.Target)
	}
	r.ExpectedETag = obj.ETag
	return nil
}
func (d *Driver) Publish(ctx context.Context, staged storage.Staged, cp storage.Checkpoint) (storage.MutationResult, error) {
	in := staged.Intent
	if err := validateRecoveryIntent(in); err != nil {
		return storage.MutationResult{}, err
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer done()
	unlock, err := d.lock(ctx, "write", in.ID, false)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer unlock()
	r, err := d.load("write", in.ID)
	if err != nil {
		return storage.MutationResult{}, err
	}
	if err = d.validateReceipt(r.Kind, in.ID, staged.Handle); err != nil {
		return storage.MutationResult{}, err
	}
	if err = d.reconcileReceipt(r, staged.Handle); err != nil {
		return storage.MutationResult{}, err
	}
	if sameJSON(in, r.Intent) && (r.Phase == "publishing" || r.Phase == "unknown") {
		return storage.MutationResult{}, fail("outcome_unknown", in.Target)
	}
	var supplied receipt
	_ = json.Unmarshal(staged.Handle, &supplied)
	if !sameJSON(in, r.Intent) || d.validateReceipt(r.Kind, in.ID, staged.Handle) != nil || supplied.Attempt != r.Attempt || staged.Size != in.Size || !sameJSON(staged.Digest, r.Digest) {
		return storage.MutationResult{}, fail("intent_conflict", in.Target)
	}
	return d.publishLocked(ctx, r, cp)
}
func (d *Driver) publishLocked(ctx context.Context, r *ledger, cp storage.Checkpoint) (storage.MutationResult, error) {
	if r.Phase == "applied" && r.Result != nil {
		if err := d.refreshApplied(ctx, r); err != nil {
			return storage.MutationResult{}, err
		}
		return cloneResult(*r.Result), nil
	}
	if r.Phase == "publishing" || r.Phase == "unknown" {
		return storage.MutationResult{}, fail("outcome_unknown", r.Intent.Target)
	}
	if r.Phase != "ready" {
		return storage.MutationResult{}, &storage.NoFinalEffect{Cause: fail("invalid_state", r.Intent.Target)}
	}
	if r.Native != nil {
		return d.publishNativeLocked(ctx, r, cp)
	}
	if err := d.validateIntent(r.Intent); err != nil {
		return storage.MutationResult{}, err
	}
	err := d.parentMoved(ctx, r)
	if err == nil {
		err = d.checkTarget(ctx, r)
	}
	if err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	file, err := d.verifiedSpool(ctx, r)
	if err != nil {
		return storage.MutationResult{}, &storage.NoFinalEffect{Cause: err}
	}
	defer file.Close()
	r.Phase = "publishing"
	if err = d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.MutationResult{}, err
	}
	// Cancellation before the one-shot request is safe only while this lock is
	// held and a terminal local record fences every future call for this identity.
	if err = ctx.Err(); err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	// A delayed checkpoint can allow coordinated parents/targets to change. Native
	// conditional create plus revalidation avoids a Stat/unconditional-write shim.
	err = d.parentMoved(ctx, r)
	if err == nil {
		err = d.checkTarget(ctx, r)
	}
	if err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	// Validate again after the possibly delayed durable callback. The private
	// spool is trusted storage, but corruption must never send acknowledged bytes.
	verified, err := d.verifiedSpool(ctx, r)
	if err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	defer verified.Close()
	file = verified
	kind := storage.File
	if r.Kind == "mkdir" {
		kind = storage.Folder
	}
	meta := map[string]string{"eam-intent": r.ID}
	obj, err := d.backend.Put(ctx, Put{Key: d.key(r.Intent.Target, kind), Body: file, Size: r.Intent.Size, MediaType: r.Intent.MediaType, Metadata: meta, Absent: r.Intent.Conditions.Absent, ExpectedETag: r.ExpectedETag})
	if err != nil {
		if storage.IsCode(err, "precondition_failed") || storage.IsCode(err, "name_conflict") {
			r.Phase = "not_applied"
			return storage.MutationResult{}, errors.Join(err, d.save(r))
		}
		// No HEAD/list/lease/elapsed time can attribute or fence this remote request.
		return storage.MutationResult{}, err
	}
	e, err := d.objectEntry(r.Intent.Target, kind, obj)
	if err != nil {
		return storage.MutationResult{}, err
	}
	if e.Size != r.Intent.Size {
		return storage.MutationResult{}, fail("invalid_backend_response", e.Path)
	}
	r.Phase = "applied"
	result := storage.MutationResult{Entry: &e, Evidence: d.receipt(r)}
	r.Result = &result
	if err = d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	if err = d.refreshApplied(ctx, r); err != nil {
		return storage.MutationResult{}, err
	}
	return cloneResult(*r.Result), checkpoint(ctx, cp, d.receipt(r))
}
func (d *Driver) refreshApplied(ctx context.Context, r *ledger) error {
	if r.MetadataReady || r.Result == nil || r.Result.Entry == nil {
		return nil
	}
	expected := r.Result.Entry
	obj, err := d.backend.Head(ctx, string(expected.NativeRef))
	if err != nil {
		return err
	}
	actual, err := d.objectEntry(expected.Path, expected.Kind, obj)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual.Revision, expected.Revision) || actual.Size != expected.Size {
		return fail("changed", expected.Path)
	}

	r.Result.Entry = &actual
	r.MetadataReady = true
	return d.save(r)
}
func cloneResult(v storage.MutationResult) storage.MutationResult {
	v.Evidence = bytes.Clone(v.Evidence)
	if v.Entry != nil {
		e := *v.Entry
		e.Revision = bytes.Clone(e.Revision)
		e.NativeRef = bytes.Clone(e.NativeRef)
		e.ContentIdentity = bytes.Clone(e.ContentIdentity)
		e.Digest = cloneDigest(e.Digest)
		v.Entry = &e
	}
	return v
}
func (d *Driver) verifiedSpool(ctx context.Context, r *ledger) (*os.File, error) {
	file, err := spoolOpen(d.root, d.dataName(r), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != r.Intent.Size {
		file.Close()
		return nil, fail("invalid_evidence", r.Intent.Target)
	}
	h := sha256.New()
	_, err = io.Copy(h, &ctxReader{ctx, file})
	if err != nil || r.Digest == nil || !bytes.Equal(h.Sum(nil), r.Digest.Value) {
		file.Close()
		return nil, fail("digest_mismatch", r.Intent.Target)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
