package localstorage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	maxSegmentBytes = 255
	maxPathBytes    = 4096
	maxDepth        = 256
	maxPageSize     = 1000
)

// Options applies to all bindings of a factory. Zero values use conservative defaults.
type Options struct {
	CaseSensitive           bool
	MaxConcurrentOperations int
	MaxDirectoryEntries     int
}

// Factory shares one bounded admission pool and cursor signing key across bindings.
// A read occupies a pool slot until it is closed or its context is canceled.
type Factory struct {
	options   Options
	slots     chan struct{}
	cursorKey [32]byte
}

var _ storage.Factory = (*Factory)(nil)

func NewFactory(options Options) (*Factory, error) {
	if !safeReadSupported {
		return nil, storage.NewError("unsupported", "", nil)
	}
	if options.MaxConcurrentOperations < 0 || options.MaxConcurrentOperations > 4096 || options.MaxDirectoryEntries < 0 || options.MaxDirectoryEntries > 1000000 {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	if options.MaxConcurrentOperations == 0 {
		options.MaxConcurrentOperations = 32
	}
	if options.MaxDirectoryEntries == 0 {
		options.MaxDirectoryEntries = 100000
	}
	f := &Factory{options: options, slots: make(chan struct{}, options.MaxConcurrentOperations)}
	if _, err := rand.Read(f.cursorKey[:]); err != nil {
		return nil, err
	}
	return f, nil
}

type driver struct {
	factory   *Factory
	content   *os.Root
	system    *os.Root
	binding   string
	lifetime  context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

var _ storage.Driver = (*driver)(nil)

func validIdentity(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

func canonicalRoot(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", storage.NewError("invalid_config", "", nil)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", storage.NewError("invalid_config", "", err)
	}
	return filepath.Clean(canonical), nil
}

func containsRoot(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func (f *Factory) Open(ctx context.Context, cfg storage.Config) (storage.Driver, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case f.slots <- struct{}{}:
	}
	defer func() { <-f.slots }()
	if err := storage.ValidateBinding(cfg); err != nil {
		return nil, err
	}
	if !validIdentity(cfg.OwnerID) || !validIdentity(cfg.ConnectionID) {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	contentName, err := canonicalRoot(cfg.ContentRoot)
	if err != nil {
		return nil, err
	}
	systemName, err := canonicalRoot(cfg.SystemRoot)
	if err != nil {
		return nil, err
	}
	if containsRoot(contentName, systemName) || containsRoot(systemName, contentName) {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	content, err := os.OpenRoot(contentName)
	if err != nil {
		return nil, storage.NewError("invalid_config", "", err)
	}
	system, err := os.OpenRoot(systemName)
	if err != nil {
		content.Close()
		return nil, storage.NewError("invalid_config", "", err)
	}
	fail := func(err error) (storage.Driver, error) { content.Close(); system.Close(); return nil, err }
	// Check identities of the held handles against the canonical paths. This also
	// catches roots aliasing through case-insensitive native spellings.
	ci, err := content.Stat(".")
	if err != nil {
		return fail(storage.NewError("invalid_config", "", err))
	}
	si, err := system.Stat(".")
	if err != nil {
		return fail(storage.NewError("invalid_config", "", err))
	}
	if os.SameFile(ci, si) {
		return fail(storage.NewError("invalid_config", "", nil))
	}
	for _, root := range []struct {
		name string
		info os.FileInfo
	}{{contentName, ci}, {systemName, si}} {
		info, err := os.Stat(root.name)
		if err != nil || !os.SameFile(root.info, info) {
			return fail(storage.NewError("invalid_config", "", err))
		}
	}
	// Walk ancestors by identity as well as comparing strings. This rejects case
	// aliases of an ancestor on case-insensitive filesystems.
	for _, pair := range []struct {
		name  string
		other os.FileInfo
	}{{contentName, si}, {systemName, ci}} {
		for ancestor := filepath.Dir(pair.name); ; ancestor = filepath.Dir(ancestor) {
			info, err := os.Stat(ancestor)
			if err != nil {
				return fail(storage.NewError("invalid_config", "", err))
			}
			if os.SameFile(info, pair.other) {
				return fail(storage.NewError("invalid_config", "", nil))
			}
			if ancestor == filepath.Dir(ancestor) {
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return f.bindHandles(cfg, content, system, contentName, systemName, ci, si), nil
}

func (f *Factory) bindHandles(cfg storage.Config, content, system *os.Root, contentName, systemName string, ci, si os.FileInfo) *driver {
	identity, _ := json.Marshal(struct {
		Owner, Connection                                string
		Generation                                       int64
		Content, System, ContentIdentity, SystemIdentity string
		CaseSensitive                                    bool
	}{cfg.OwnerID, cfg.ConnectionID, cfg.Generation, contentName, systemName, rootIdentity(ci), rootIdentity(si), f.options.CaseSensitive})
	hash := sha256.Sum256(identity)
	lifetime, cancel := context.WithCancel(context.Background())
	return &driver{factory: f, content: content, system: system, binding: hex.EncodeToString(hash[:]), lifetime: lifetime, cancel: cancel}
}

func (d *driver) Describe() storage.Descriptor {
	return storage.Descriptor{RangeRead: true, Names: storage.NameRules{CaseSensitive: d.factory.options.CaseSensitive, MaxSegmentBytes: maxSegmentBytes, MaxPathBytes: maxPathBytes, MaxDepth: maxDepth}}
}

func (d *driver) Close() error {
	d.closeOnce.Do(func() { d.cancel(); d.closeErr = errors.Join(d.content.Close(), d.system.Close()) })
	return d.closeErr
}

func (d *driver) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.lifetime.Err() != nil {
		return nil, storage.NewError("closed", "", nil)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.lifetime.Done():
		return nil, storage.NewError("closed", "", nil)
	case d.factory.slots <- struct{}{}:
		var once sync.Once
		release := func() { once.Do(func() { <-d.factory.slots }) }
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		if d.lifetime.Err() != nil {
			release()
			return nil, storage.NewError("closed", "", nil)
		}
		return release, nil
	}
}

func validatePath(path storage.Path) error {
	if err := storage.ValidatePath(path); err != nil {
		return err
	}
	if len(path) > maxPathBytes {
		return storage.NewError("invalid_name", path, nil)
	}
	if path == "" {
		return nil
	}
	parts := strings.Split(string(path), "/")
	if len(parts) > maxDepth {
		return storage.NewError("invalid_name", path, nil)
	}
	for _, part := range parts {
		if len(part) > maxSegmentBytes {
			return storage.NewError("invalid_name", path, nil)
		}
	}
	return nil
}

func (d *driver) key(name string) string {
	key := norm.NFC.String(name)
	if !d.factory.options.CaseSensitive {
		key = cases.Fold().String(key)
	}
	return key
}

func childPath(parent storage.Path, name string) storage.Path {
	if parent == "" {
		return storage.Path(name)
	}
	return storage.Path(string(parent) + "/" + name)
}

func mapError(path storage.Path, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return storage.NewError("not_found", path, err)
	}
	if errors.Is(err, fs.ErrPermission) {
		return storage.NewError("permission_denied", path, err)
	}
	if errors.Is(err, os.ErrClosed) {
		return storage.NewError("closed", path, err)
	}
	return storage.NewError("unavailable", path, err)
}

func entry(path storage.Path, info os.FileInfo) (storage.Entry, error) {
	e := storage.Entry{Path: path, ModifiedAt: info.ModTime()}
	switch {
	case info.IsDir():
		e.Kind = storage.Folder
	case info.Mode().IsRegular():
		e.Kind = storage.File
		e.Size = info.Size()
	default:
		return storage.Entry{}, storage.NewError("unsupported", path, nil)
	}
	return e, nil
}

type nativeEntry struct {
	name  string
	info  os.FileInfo
	entry storage.Entry
}

// scan checks every immediate child before callers can return any partial page.
func (d *driver) scan(ctx context.Context, root *os.Root, parent storage.Path) ([]nativeEntry, error) {
	file, err := root.Open(".")
	if err != nil {
		return nil, mapError(parent, err)
	}
	defer file.Close()
	all := make([]nativeEntry, 0)
	seen := make(map[string]bool)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, readErr := file.ReadDir(256)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, mapError(parent, readErr)
		}
		for _, de := range batch {
			path := childPath(parent, de.Name())
			if err := validatePath(path); err != nil {
				return nil, err
			}
			key := d.key(de.Name())
			if seen[key] {
				return nil, storage.NewError("ambiguous_path", parent, nil)
			}
			seen[key] = true
			if len(all) >= d.factory.options.MaxDirectoryEntries {
				return nil, storage.NewError("resource_limit", parent, nil)
			}
			info, err := root.Lstat(de.Name())
			if err != nil {
				return nil, mapError(path, err)
			}
			e, err := entry(path, info)
			if err != nil {
				return nil, err
			}
			all = append(all, nativeEntry{name: de.Name(), info: info, entry: e})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })
	return all, nil
}

// find requires exact native spelling even on a case/normalization-insensitive FS.
func (d *driver) find(ctx context.Context, root *os.Root, parent storage.Path, name string) (nativeEntry, error) {
	all, err := d.scan(ctx, root, parent)
	if err != nil {
		return nativeEntry{}, err
	}
	key := d.key(name)
	for _, e := range all {
		if e.name == name {
			return e, nil
		}
		if d.key(e.name) == key {
			return nativeEntry{}, storage.NewError("invalid_name", childPath(parent, name), nil)
		}
	}
	return nativeEntry{}, storage.NewError("not_found", childPath(parent, name), nil)
}

// directory pins each native component independently, then verifies that opening
// did not follow a swapped link to some other inode within the original root.
func (d *driver) directory(ctx context.Context, path storage.Path) (*os.Root, error) {
	root, err := d.content.OpenRoot(".")
	if err != nil {
		return nil, mapError(path, err)
	}
	if path == "" {
		return root, nil
	}
	var parent storage.Path
	for _, name := range strings.Split(string(path), "/") {
		found, err := d.find(ctx, root, parent, name)
		if err != nil {
			root.Close()
			return nil, err
		}
		if found.entry.Kind != storage.Folder {
			root.Close()
			return nil, storage.NewError("not_folder", childPath(parent, name), nil)
		}
		next, err := root.OpenRoot(name)
		root.Close()
		if err != nil {
			return nil, mapError(path, err)
		}
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(found.info, actual) {
			next.Close()
			return nil, storage.NewError("changed", path, err)
		}
		root = next
		parent = childPath(parent, name)
	}
	return root, nil
}

func splitPath(path storage.Path) (storage.Path, string) {
	at := strings.LastIndexByte(string(path), '/')
	if at < 0 {
		return "", string(path)
	}
	return path[:at], string(path[at+1:])
}

func (d *driver) Stat(ctx context.Context, path storage.Path) (storage.Entry, error) {
	if err := validatePath(path); err != nil {
		return storage.Entry{}, err
	}
	release, err := d.acquire(ctx)
	if err != nil {
		return storage.Entry{}, err
	}
	defer release()
	if path == "" {
		info, err := d.content.Stat(".")
		if err != nil {
			return storage.Entry{}, mapError(path, err)
		}
		return entry(path, info)
	}
	parent, name := splitPath(path)
	root, err := d.directory(ctx, parent)
	if err != nil {
		return storage.Entry{}, err
	}
	defer root.Close()
	found, err := d.find(ctx, root, parent, name)
	if err != nil {
		return storage.Entry{}, err
	}
	return found.entry, nil
}

type cursor struct {
	Version int          `json:"v"`
	Binding string       `json:"b"`
	Parent  storage.Path `json:"p"`
	Last    string       `json:"l"`
}

func (d *driver) encodeCursor(parent storage.Path, last string) string {
	data, _ := json.Marshal(cursor{Version: 1, Binding: d.binding, Parent: parent, Last: last})
	mac := hmac.New(sha256.New, d.factory.cursorKey[:])
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (d *driver) decodeCursor(parent storage.Path, token string) (string, error) {
	invalid := func() (string, error) { return "", storage.NewError("invalid_cursor", parent, nil) }
	if len(token) > 8192 {
		return invalid()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return invalid()
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return invalid()
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return invalid()
	}
	mac := hmac.New(sha256.New, d.factory.cursorKey[:])
	mac.Write(data)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return invalid()
	}
	var c cursor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || c.Version != 1 || c.Binding != d.binding || c.Parent != parent || c.Last == "" || strings.Contains(c.Last, "/") {
		return invalid()
	}
	if decoder.Decode(new(any)) != io.EOF || validatePath(storage.Path(c.Last)) != nil {
		return invalid()
	}
	return c.Last, nil
}

func (d *driver) List(ctx context.Context, path storage.Path, request storage.PageRequest) (storage.Page, error) {
	if err := validatePath(path); err != nil {
		return storage.Page{}, err
	}
	if request.Limit < 0 || request.Limit > maxPageSize {
		return storage.Page{}, storage.NewError("invalid_page", path, nil)
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	var last string
	if request.Cursor != "" {
		var err error
		last, err = d.decodeCursor(path, request.Cursor)
		if err != nil {
			return storage.Page{}, err
		}
	}
	release, err := d.acquire(ctx)
	if err != nil {
		return storage.Page{}, err
	}
	defer release()
	root, err := d.directory(ctx, path)
	if err != nil {
		return storage.Page{}, err
	}
	defer root.Close()
	all, err := d.scan(ctx, root, path)
	if err != nil {
		return storage.Page{}, err
	}
	start := sort.Search(len(all), func(i int) bool { return all[i].name > last })
	end := min(start+limit, len(all))
	result := storage.Page{Entries: make([]storage.Entry, 0, end-start)}
	for _, e := range all[start:end] {
		result.Entries = append(result.Entries, e.entry)
	}
	if end < len(all) {
		result.NextCursor = d.encodeCursor(path, all[end-1].name)
	}
	return result, nil
}

func (d *driver) Open(ctx context.Context, path storage.Path, options storage.ReadOptions) (storage.Read, error) {
	if err := validatePath(path); err != nil {
		return storage.Read{}, err
	}
	if len(options.Version) != 0 {
		return storage.Read{}, storage.NewError("unsupported", path, nil)
	}
	if options.Offset < 0 || options.Length != nil && *options.Length <= 0 {
		return storage.Read{}, storage.NewError("invalid_range", path, nil)
	}
	release, err := d.acquire(ctx)
	if err != nil {
		return storage.Read{}, err
	}
	success := false
	defer func() {
		if !success {
			release()
		}
	}()
	if path == "" {
		return storage.Read{}, storage.NewError("not_file", path, nil)
	}
	parent, name := splitPath(path)
	root, err := d.directory(ctx, parent)
	if err != nil {
		return storage.Read{}, err
	}
	defer root.Close()
	found, err := d.find(ctx, root, parent, name)
	if err != nil {
		return storage.Read{}, err
	}
	if found.entry.Kind != storage.File {
		return storage.Read{}, storage.NewError("not_file", path, nil)
	}
	file, err := openReadOnly(root, name)
	if err != nil {
		return storage.Read{}, mapError(path, err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(found.info, info) {
		file.Close()
		return storage.Read{}, storage.NewError("changed", path, err)
	}
	e, err := entry(path, info)
	if err != nil {
		file.Close()
		return storage.Read{}, err
	}
	if options.Offset > e.Size {
		file.Close()
		return storage.Read{}, storage.NewError("invalid_range", path, nil)
	}
	length := e.Size - options.Offset
	if options.Length != nil {
		if *options.Length > length {
			file.Close()
			return storage.Read{}, storage.NewError("invalid_range", path, nil)
		}
		length = *options.Length
	}
	body := &contextReader{ctx: ctx, lifetime: d.lifetime, file: file, reader: io.NewSectionReader(file, options.Offset, length), remaining: length, release: release}
	body.mu.Lock()
	body.stopContext = context.AfterFunc(ctx, func() { body.Close() })
	body.stopLifetime = context.AfterFunc(d.lifetime, func() { body.Close() })
	body.mu.Unlock()
	if err := ctx.Err(); err != nil {
		body.Close()
		return storage.Read{}, err
	}
	if d.lifetime.Err() != nil {
		body.Close()
		return storage.Read{}, storage.NewError("closed", path, nil)
	}
	success = true
	return storage.Read{Body: body, Entry: e, Offset: options.Offset, Length: length}, nil
}

type contextReader struct {
	ctx          context.Context
	lifetime     context.Context
	file         *os.File
	reader       *io.SectionReader
	remaining    int64
	release      func()
	mu           sync.Mutex
	closed       bool
	closeErr     error
	stopContext  func() bool
	stopLifetime func() bool
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.lifetime.Err() != nil {
		return 0, os.ErrClosed
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	if err == io.EOF {
		r.Close()
		if r.remaining > 0 {
			err = io.ErrUnexpectedEOF
		}
	}
	return n, err
}

func (r *contextReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	if r.stopContext != nil {
		r.stopContext()
	}
	if r.stopLifetime != nil {
		r.stopLifetime()
	}
	r.closeErr = r.file.Close()
	r.release()
	return r.closeErr
}
