// Package objecttree implements read-only native prefix trees shared by object stores.
// Provider authentication and wire formats remain in their respective adapters.
package objecttree

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/unicode/norm"
)

// Object is metadata supplied by a native HEAD, list or GET. VersionID is the
// provider's immutable version id, and is empty unless the provider retains
// versions. It is never the revision a conditional effect compares.
type Object struct {
	Key        string
	Size       int64
	ModifiedAt time.Time
	MediaType  string
	VersionID  string
}
type NativePage struct {
	Objects   []Object
	Prefixes  []string
	Next      string
	Truncated bool
}
type Response struct {
	Object       Object
	Body         io.ReadCloser
	Status       int
	ContentRange string
	Length       int64
}

// Backend is the small object-store boundary needed by the two P1 adapters.
// List must use delimiter / and return exact, decoded-once key spelling.
type Backend interface {
	Head(context.Context, string) (Object, error)
	List(context.Context, string, string) (NativePage, error)
	Get(context.Context, string, string, string) (Response, error)
}

// P1 bounds complete-directory collision checking. Oversized or nonterminating
// native listings fail without returning a partial directory.
//
// The bound is memory, not policy: one directory is materialized as a map of
// path spellings plus entries, about 250 bytes each, so 250k children costs roughly
// 60 MB for the length of the walk. It used to be 100k, which refused a flat folder
// that any photo or health export can honestly produce.
const maxDirectoryEntries = 250000
const maxNativePages = 1000

type Tree struct {
	backend  Backend
	root     string
	scope    string
	secret   []byte
	lifetime context.Context
	cancel   context.CancelFunc
}

// Bind creates a cheap root view over an existing pooled native client. Roots
// are exact relative prefixes; one optional trailing slash is accepted.
func Bind(backend Backend, cfg storage.Config, secret []byte) (*Tree, error) {
	if err := storage.ValidateBinding(cfg); err != nil {
		return nil, err
	}
	root, err := validateRoot(cfg.ContentRoot)
	if err != nil {
		return nil, err
	}
	system, err := validateRoot(cfg.SystemRoot)
	if err != nil {
		return nil, err
	}
	a, b := norm.NFC.String(root), norm.NFC.String(system)
	if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	if backend == nil || len(secret) < 32 {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	identity, _ := json.Marshal([]any{cfg.ConnectionID, cfg.Generation, cfg.OwnerID, root, system})
	lifetime, cancel := context.WithCancel(context.Background())
	return &Tree{backend: backend, root: root, scope: string(identity), secret: secret, lifetime: lifetime, cancel: cancel}, nil
}
func validateRoot(root string) (string, error) {
	root = strings.TrimSuffix(root, "/")
	if root == "" || len(root)+1 > 1024 || storage.ValidatePath(storage.Path(root)) != nil {
		return "", storage.NewError("invalid_config", "", nil)
	}
	return root + "/", nil
}
func NewSecret() ([]byte, error) { b := make([]byte, 32); _, err := rand.Read(b); return b, err }
func (t *Tree) Describe() storage.Descriptor {
	return storage.Descriptor{RangeRead: true, TransferClasses: []string{storage.TransferRelay}, Names: storage.NameRules{CaseSensitive: true, MaxPathBytes: 1024}}
}
func (t *Tree) Close() error { t.cancel(); return nil }
func (t *Tree) operation(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if t.lifetime.Err() != nil {
		return nil, nil, fail("closed", "")
	}
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.lifetime, cancel)
	return operation, func() { stop(); cancel() }, nil
}
func (t *Tree) check(ctx context.Context, path storage.Path) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := storage.ValidatePath(path); err != nil {
		return err
	}
	if len(t.root)+len(path) > 1024 {
		return storage.NewError("invalid_name", path, nil)
	}
	return nil
}
func (t *Tree) key(path storage.Path) string    { return t.root + string(path) }
func fail(code string, path storage.Path) error { return storage.NewError(code, path, nil) }

// children consumes every native page before returning results, so collisions
// across page boundaries and incomplete/progress-free listings fail closed.
// Memory is proportional to the immediate directory, not its descendants.
func (t *Tree) children(ctx context.Context, path storage.Path) ([]storage.Entry, error) {
	prefix := t.key(path)
	if path != "" {
		prefix += "/"
	}
	entries := map[string]storage.Entry{}
	spellings := map[string]string{}
	add := func(key string, folder bool, obj Object) error {
		if !strings.HasPrefix(key, prefix) {
			return fail("invalid_backend_response", path)
		}
		relative := strings.TrimPrefix(key, prefix)
		if relative == "" && folder && obj.Size == 0 {
			return nil
		}
		if folder {
			if !strings.HasSuffix(relative, "/") {
				return fail("invalid_backend_response", path)
			}
			relative = strings.TrimSuffix(relative, "/")
		}
		if relative == "" {
			return fail("invalid_name", path)
		}
		if strings.Contains(relative, "/") {
			return fail("invalid_backend_response", path)
		}
		child := storage.Path(relative)
		if path != "" {
			child = path + "/" + child
		}
		if err := storage.ValidatePath(child); err != nil {
			return err
		}
		if len(t.root)+len(child) > 1024 {
			return fail("invalid_name", child)
		}
		canonical := norm.NFC.String(relative)
		if previous, ok := spellings[canonical]; ok && previous != relative {
			return fail("ambiguous_path", child)
		}
		spellings[canonical] = relative
		kind := storage.File
		if folder {
			kind = storage.Folder
		}
		if previous, ok := entries[relative]; ok {
			if previous.Kind != kind || !folder {
				return fail("ambiguous_path", child)
			}
			return nil // a common prefix and its empty marker denote one folder
		}
		if len(entries) >= maxDirectoryEntries {
			return fail("resource_limit", path)
		}
		entry := storage.Entry{Path: child, Kind: kind, NativeRef: []byte(t.key(child))}
		if !folder {
			if obj.Size < 0 {
				return fail("invalid_backend_response", child)
			}
			entry.Size = obj.Size
			entry.ModifiedAt = obj.ModifiedAt
			entry.MediaType = obj.MediaType
			entry.Revision = versionToken(obj.VersionID)
		}
		entries[relative] = entry
		return nil
	}
	cursor := ""
	seen := map[string]bool{"": true}
	for pageCount := 0; ; pageCount++ {
		if pageCount >= maxNativePages {
			return nil, fail("resource_limit", path)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := t.backend.List(ctx, prefix, cursor)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Objects {
			folder := strings.HasSuffix(obj.Key, "/")
			if folder && obj.Size != 0 {
				return nil, fail("ambiguous_path", path)
			}
			if err := add(obj.Key, folder, obj); err != nil {
				return nil, err
			}
		}
		for _, key := range page.Prefixes {
			if err := add(key, true, Object{}); err != nil {
				return nil, err
			}
		}
		if !page.Truncated {
			if page.Next != "" {
				return nil, fail("invalid_backend_response", path)
			}
			break
		}
		if page.Next == "" || seen[page.Next] || len(page.Objects)+len(page.Prefixes) == 0 {
			return nil, fail("incomplete_listing", path)
		}
		seen[page.Next] = true
		cursor = page.Next
	}
	result := make([]storage.Entry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func (t *Tree) resolve(ctx context.Context, path storage.Path) (storage.Entry, error) {
	if err := t.check(ctx, path); err != nil {
		return storage.Entry{}, err
	}
	current := storage.Entry{Path: "", Kind: storage.Folder, NativeRef: []byte(t.root)}
	if path == "" {
		return current, nil
	}
	for _, segment := range strings.Split(string(path), "/") {
		if current.Kind != storage.Folder {
			return storage.Entry{}, fail("not_found", path)
		}
		children, err := t.children(ctx, current.Path)
		if err != nil {
			return storage.Entry{}, err
		}
		next := storage.Path(segment)
		if current.Path != "" {
			next = current.Path + "/" + next
		}
		found := false
		for _, entry := range children {
			if entry.Path == next {
				current = entry
				found = true
				break
			}
		}
		if !found {
			return storage.Entry{}, fail("not_found", path)
		}
	}
	return current, nil
}
func (t *Tree) Stat(ctx context.Context, path storage.Path) (storage.Entry, error) {
	ctx, done, err := t.operation(ctx)
	if err != nil {
		return storage.Entry{}, err
	}
	defer done()
	if err := t.check(ctx, path); err != nil {
		return storage.Entry{}, err
	}
	if path == "" {
		return storage.Entry{Path: "", Kind: storage.Folder, NativeRef: []byte(t.root)}, nil
	}
	// One listing of the parent settles this path: its kind, its exact spelling and
	// any sibling collision are all decided there. Walking from the root turned one
	// observation into one request per path segment, which made every deep read and
	// every action step pay for the depth. A file still needs its own HEAD, because
	// a listing never establishes a revision.
	parent := parentPathOf(path)
	children, err := t.children(ctx, parent)
	if err != nil {
		return storage.Entry{}, err
	}
	for _, entry := range children {
		if entry.Path != path {
			continue
		}
		if entry.Kind == storage.Folder {
			return entry, nil
		}
		obj, err := t.backend.Head(ctx, t.key(path))
		if err != nil {
			return storage.Entry{}, err
		}
		if obj.Size < 0 {
			return storage.Entry{}, fail("invalid_backend_response", path)
		}
		return t.entry(path, obj), nil
	}
	return storage.Entry{}, fail("not_found", path)
}
func parentPathOf(p storage.Path) storage.Path {
	if index := strings.LastIndexByte(string(p), '/'); index >= 0 {
		return p[:index]
	}
	return ""
}
func (t *Tree) entry(path storage.Path, obj Object) storage.Entry {
	return storage.Entry{Path: path, Kind: storage.File, NativeRef: []byte(t.key(path)), Revision: versionToken(obj.VersionID), Size: obj.Size, ModifiedAt: obj.ModifiedAt, MediaType: obj.MediaType}
}
// versionToken encodes a provider version id as this tree's revision token. A
// provider that retains no versions yields no token; the mutation driver then
// derives its revision from the object ETag instead.
func versionToken(v string) []byte {
	if v == "" || v == "null" {
		return nil
	}
	return []byte("native-version:" + v)
}
func parseVersionToken(v []byte) (string, error) {
	if len(v) == 0 {
		return "", nil
	}
	if len(v) > 4096 || !bytes.HasPrefix(v, []byte("native-version:")) || len(v) == len("native-version:") {
		return "", fail("unsupported", "")
	}
	raw := string(v[len("native-version:"):])
	if raw == "null" || strings.ContainsAny(raw, "\r\n\x00") {
		return "", fail("unsupported", "")
	}
	return raw, nil
}

type cursorData struct {
	Scope string
	Path  storage.Path
	Last  storage.Path
}

func (t *Tree) encodeCursor(path, last storage.Path) string {
	raw, _ := json.Marshal(cursorData{t.scope, path, last})
	mac := hmac.New(sha256.New, t.secret)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(append(raw, mac.Sum(nil)...))
}
func (t *Tree) decodeCursor(path storage.Path, cursor string) (storage.Path, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) <= sha256.Size || len(raw) > 16384 {
		return "", fail("invalid_cursor", path)
	}
	data, signature := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, t.secret)
	mac.Write(data)
	var c cursorData
	if !hmac.Equal(signature, mac.Sum(nil)) || json.Unmarshal(data, &c) != nil || c.Scope != t.scope || c.Path != path || c.Last == "" {
		return "", fail("invalid_cursor", path)
	}
	return c.Last, nil
}
func (t *Tree) List(ctx context.Context, path storage.Path, request storage.PageRequest) (storage.Page, error) {
	ctx, done, err := t.operation(ctx)
	if err != nil {
		return storage.Page{}, err
	}
	defer done()
	if err := t.check(ctx, path); err != nil {
		return storage.Page{}, err
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return storage.Page{}, fail("invalid_cursor", path)
	}
	var last storage.Path
	if request.Cursor != "" {
		var err error
		last, err = t.decodeCursor(path, request.Cursor)
		if err != nil {
			return storage.Page{}, err
		}
	}
	entry, err := t.resolve(ctx, path)
	if err != nil {
		return storage.Page{}, err
	}
	if entry.Kind != storage.Folder {
		return storage.Page{}, fail("not_folder", path)
	}
	children, err := t.children(ctx, path)
	if err != nil {
		return storage.Page{}, err
	}
	start := sort.Search(len(children), func(i int) bool { return children[i].Path > last })
	end := start + limit
	if end > len(children) {
		end = len(children)
	}
	page := storage.Page{Entries: children[start:end]}
	if end < len(children) {
		page.NextCursor = t.encodeCursor(path, children[end-1].Path)
	}
	return page, nil
}
func (t *Tree) Open(ctx context.Context, path storage.Path, opts storage.ReadOptions) (storage.Read, error) {
	ctx, done, err := t.operation(ctx)
	if err != nil {
		return storage.Read{}, err
	}
	retained := false
	defer func() {
		if !retained {
			done()
		}
	}()
	if err := t.check(ctx, path); err != nil {
		return storage.Read{}, err
	}
	if opts.Offset < 0 || (opts.Length != nil && (*opts.Length <= 0 || opts.Offset > math.MaxInt64-*opts.Length)) {
		return storage.Read{}, fail("invalid_range", path)
	}
	entry, err := t.resolve(ctx, path)
	if err != nil {
		return storage.Read{}, err
	}
	if entry.Kind != storage.File {
		return storage.Read{}, fail("not_file", path)
	}
	read, err := t.fetch(ctx, path, t.key(path), opts, done)
	if err != nil {
		return storage.Read{}, err
	}
	retained = true
	return read, nil
}

// StatReference observes a file through a key the caller's index proved for this exact
// path. Unlike Stat it never lists ancestors: a byte-exact HEAD is the whole proof, and
// a reference that is not the key this tree derives for the path is a stale index.
func (t *Tree) StatReference(ctx context.Context, path storage.Path, ref []byte) (storage.Entry, error) {
	ctx, done, err := t.operation(ctx)
	if err != nil {
		return storage.Entry{}, err
	}
	defer done()
	if err := t.check(ctx, path); err != nil {
		return storage.Entry{}, err
	}
	if path == "" || string(ref) != t.key(path) {
		return storage.Entry{}, fail("stale_reference", path)
	}
	obj, err := t.backend.Head(ctx, string(ref))
	if err != nil {
		return storage.Entry{}, err
	}
	if obj.Key != string(ref) || obj.Size < 0 {
		return storage.Entry{}, fail("invalid_backend_response", path)
	}
	return t.entry(path, obj), nil
}

// OpenReference transfers through the same proven key without resolving the path.
func (t *Tree) OpenReference(ctx context.Context, path storage.Path, ref []byte, opts storage.ReadOptions) (storage.Read, error) {
	ctx, done, err := t.operation(ctx)
	if err != nil {
		return storage.Read{}, err
	}
	retained := false
	defer func() {
		if !retained {
			done()
		}
	}()
	if err := t.check(ctx, path); err != nil {
		return storage.Read{}, err
	}
	if path == "" || string(ref) != t.key(path) {
		return storage.Read{}, fail("stale_reference", path)
	}
	if opts.Offset < 0 || (opts.Length != nil && (*opts.Length <= 0 || opts.Offset > math.MaxInt64-*opts.Length)) {
		return storage.Read{}, fail("invalid_range", path)
	}
	read, err := t.fetch(ctx, path, string(ref), opts, done)
	if err != nil {
		return storage.Read{}, err
	}
	retained = true
	return read, nil
}

// fetch issues exactly one ranged or full GET for an already proven key.
func (t *Tree) fetch(ctx context.Context, path storage.Path, key string, opts storage.ReadOptions, done func()) (storage.Read, error) {
	selected, err := parseVersionToken(opts.PinnedVersion)
	if err != nil {
		return storage.Read{}, err
	}
	ranged := opts.Offset > 0 || opts.Length != nil
	rangeHeader := ""
	if ranged {
		rangeHeader = "bytes=" + strconv.FormatInt(opts.Offset, 10) + "-"
		if opts.Length != nil {
			rangeHeader += strconv.FormatInt(opts.Offset+*opts.Length-1, 10)
		}
	}
	response, err := t.backend.Get(ctx, key, rangeHeader, selected)
	if err != nil {
		return storage.Read{}, err
	}
	reject := func(code string) (storage.Read, error) {
		if response.Body != nil {
			response.Body.Close()
		}
		return storage.Read{}, fail(code, path)
	}
	if response.Body == nil || response.Length < 0 {
		return reject("invalid_backend_response")
	}
	if selected != "" && response.Object.VersionID != selected {
		return reject("changed")
	}
	fullSize := response.Length
	if ranged {
		if response.Status != http.StatusPartialContent {
			return reject("invalid_backend_response")
		}
		start, end, total, err := parseContentRange(response.ContentRange)
		if err != nil || start != opts.Offset || end-start+1 != response.Length || opts.Length != nil && response.Length != *opts.Length || opts.Length == nil && end != total-1 {
			return reject("invalid_backend_response")
		}
		fullSize = total
	} else if response.Status != http.StatusOK || response.ContentRange != "" {
		return reject("invalid_backend_response")
	}
	response.Object.Size = fullSize
	return storage.Read{Body: &exactBody{body: response.Body, remaining: response.Length, cleanup: done}, Entry: t.entry(path, response.Object), Offset: opts.Offset, Length: response.Length}, nil
}
func parseContentRange(value string) (int64, int64, int64, error) {
	invalid := errors.New("invalid content range")
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, invalid
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes "), "/")
	if len(parts) != 2 {
		return 0, 0, 0, invalid
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, invalid
	}
	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, 0, err
	}
	end, err := strconv.ParseInt(bounds[1], 10, 64)
	if err != nil {
		return 0, 0, 0, err
	}
	total, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, invalid
	}
	return start, end, total, nil
}

type exactBody struct {
	body      io.ReadCloser
	remaining int64
	done      bool
	cleanup   func()
}

func (b *exactBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.done {
		return 0, io.EOF
	}
	if b.remaining == 0 {
		var extra [1]byte
		n, err := b.body.Read(extra[:])
		if n > 0 {
			b.done = true
			return 0, storage.NewError("invalid_backend_response", "", nil)
		}
		if err == io.EOF {
			b.done = true
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	if err == io.EOF && b.remaining > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
func (b *exactBody) Close() error {
	if b.cleanup != nil {
		b.cleanup()
	}
	return b.body.Close()
}
