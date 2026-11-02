package localstorage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"sort"

	"everythingabout.me/backend/internal/storage"
)

// MutationDriver is admitted only for coordinated exclusive native writers.
// The caller must retain service path guards through native outcome recovery.
// Versions describe native identity and verified bytes, independent of path.
type MutationDriver struct {
	*StagedWriter
	rename   func(*os.Root, string, *os.Root, string, bool) error
	remove   func(*os.Root, string) error
	listScan func(context.Context, *os.Root, storage.Path) ([]nativeEntry, error)
}

var (
	_ storage.Driver              = (*MutationDriver)(nil)
	_ storage.BoundStagedWriter   = (*MutationDriver)(nil)
	_ storage.BoundEffectRecovery = (*MutationDriver)(nil)
	_ storage.FolderWriter        = (*MutationDriver)(nil)
	_ storage.Remover             = (*MutationDriver)(nil)
	_ storage.NativeMover         = (*MutationDriver)(nil)
	_ storage.NativeCopier        = (*MutationDriver)(nil)
)

func NewMutationDriver(driver storage.Driver, options WriteOptions) (*MutationDriver, error) {
	if !actionSupported {
		return nil, storage.NewError("unsupported", "", nil)
	}
	w, err := NewStagedWriter(driver, options)
	if err != nil {
		return nil, err
	}
	w.replacement = true
	w.replace = actionRename
	return &MutationDriver{StagedWriter: w, rename: actionRename, listScan: w.driver.scan, remove: func(root *os.Root, name string) error { return root.Remove(name) }}, nil
}
func (w *MutationDriver) Describe() storage.Descriptor {
	d := w.driver.Describe()
	d.PinnedRead, d.AtomicCreate, d.AtomicReplace = true, true, true
	// Expected-version conditions rely on the declared coordinated writer model;
	// the filesystem supplies atomic visibility, not a native version CAS.
	d.NativeConditionalMutation, d.NativeTreeMove = false, true
	return d
}
func (w *MutationDriver) Close() error { return w.driver.Close() }

// mtime is retained across rename; ctime and path are deliberately excluded.
// Files also carry a verified digest so equal size/timestamp cannot hide edits.
func nativeVersion(e storage.Entry, info os.FileInfo) storage.Entry {
	e.NativeRef = []byte(rootIdentity(info))
	e.ContentIdentity = bytes.Clone(e.NativeRef)
	if e.Kind == storage.Folder {
		h := sha256.Sum256(e.NativeRef)
		e.Version = h[:]
		return e
	}
	b, _ := json.Marshal(struct {
		Identity       string
		Kind           storage.Kind
		Size, Modified int64
		Mode           uint32
		Digest         *storage.Digest
	}{rootIdentity(info), e.Kind, e.Size, info.ModTime().UnixNano(), uint32(info.Mode()), e.Digest})
	h := sha256.Sum256(b)
	e.Version = h[:]
	return e
}
func sameNativeStat(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime() && a.Mode() == b.Mode()
}
func verifiedNative(ctx context.Context, root *os.Root, name string, path storage.Path, expected os.FileInfo) (storage.Entry, *os.File, error) {
	f, err := openReadOnly(root, name)
	if err != nil {
		return storage.Entry{}, nil, mapError(path, err)
	}
	fail := func(err error) (storage.Entry, *os.File, error) { f.Close(); return storage.Entry{}, nil, err }
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || !sameNativeStat(expected, before) {
		return fail(storage.NewError("changed", path, err))
	}
	h := sha256.New()
	n, err := io.Copy(h, stageReader{ctx: ctx, reader: f})
	if err != nil {
		return fail(err)
	}
	after, err := f.Stat()
	if err != nil || n != before.Size() || !sameNativeStat(before, after) {
		return fail(storage.NewError("changed", path, err))
	}
	e, err := entry(path, after)
	if err != nil {
		return fail(err)
	}
	e.Digest = &storage.Digest{Algorithm: "sha-256", Value: h.Sum(nil)}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return nativeVersion(e, after), f, nil
}
func (w *MutationDriver) stat(ctx context.Context, path storage.Path) (storage.Entry, error) {
	if path == "" {
		info, err := w.driver.content.Stat(".")
		if err != nil {
			return storage.Entry{}, err
		}
		e, err := entry(path, info)
		return nativeVersion(e, info), err
	}
	parent, name := splitPath(path)
	root, err := w.driver.directory(ctx, parent)
	if err != nil {
		return storage.Entry{}, err
	}
	defer root.Close()
	return w.nativeAt(ctx, root, parent, name)
}
func (w *MutationDriver) nativeAt(ctx context.Context, root *os.Root, parent storage.Path, name string) (storage.Entry, error) {
	found, err := w.driver.find(ctx, root, parent, name)
	if err != nil {
		return storage.Entry{}, err
	}
	if found.entry.Kind == storage.Folder {
		return nativeVersion(found.entry, found.info), nil
	}
	e, f, err := verifiedNative(ctx, root, name, childPath(parent, name), found.info)
	if f != nil {
		f.Close()
	}
	return e, err
}
func (w *MutationDriver) Stat(ctx context.Context, path storage.Path) (storage.Entry, error) {
	if err := validatePath(path); err != nil {
		return storage.Entry{}, err
	}
	release, err := w.driver.acquire(ctx)
	if err != nil {
		return storage.Entry{}, err
	}
	defer release()
	return w.stat(ctx, path)
}

// List scans and validates all immediate names once, then verifies only the
// selected page through the same pinned parent. File versions require reading
// their bytes; listing cost is linear in sibling names plus returned file bytes.
func (w *MutationDriver) List(ctx context.Context, path storage.Path, request storage.PageRequest) (storage.Page, error) {
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
		last, err = w.driver.decodeCursor(path, request.Cursor)
		if err != nil {
			return storage.Page{}, err
		}
	}
	release, err := w.driver.acquire(ctx)
	if err != nil {
		return storage.Page{}, err
	}
	defer release()
	root, err := w.driver.directory(ctx, path)
	if err != nil {
		return storage.Page{}, err
	}
	defer root.Close()
	all, err := w.listScan(ctx, root, path)
	if err != nil {
		return storage.Page{}, err
	}
	start := sort.Search(len(all), func(i int) bool { return all[i].name > last })
	end := min(start+limit, len(all))
	page := storage.Page{Entries: make([]storage.Entry, 0, end-start)}
	for _, found := range all[start:end] {
		if err := ctx.Err(); err != nil {
			return storage.Page{}, err
		}
		info, err := root.Lstat(found.name)
		if err != nil {
			return storage.Page{}, mapError(found.entry.Path, err)
		}
		if !sameNativeStat(found.info, info) {
			return storage.Page{}, storage.NewError("changed", found.entry.Path, nil)
		}
		e, err := entry(found.entry.Path, info)
		if err != nil {
			return storage.Page{}, err
		}
		if e.Kind == storage.Folder {
			e = nativeVersion(e, info)
		} else {
			var file *os.File
			e, file, err = verifiedNative(ctx, root, found.name, found.entry.Path, info)
			if file != nil {
				file.Close()
			}
			if err != nil {
				return storage.Page{}, err
			}
		}
		page.Entries = append(page.Entries, e)
	}
	if end < len(all) {
		page.NextCursor = w.driver.encodeCursor(path, all[end-1].name)
	}
	return page, nil
}
func (w *MutationDriver) Open(ctx context.Context, path storage.Path, options storage.ReadOptions) (storage.Read, error) {
	if err := validatePath(path); err != nil {
		return storage.Read{}, err
	}
	if path == "" {
		return storage.Read{}, storage.NewError("not_file", path, nil)
	}
	if options.Offset < 0 || options.Length != nil && *options.Length <= 0 {
		return storage.Read{}, storage.NewError("invalid_range", path, nil)
	}
	release, err := w.driver.acquire(ctx)
	if err != nil {
		return storage.Read{}, err
	}
	success := false
	defer func() {
		if !success {
			release()
		}
	}()
	parent, name := splitPath(path)
	root, err := w.driver.directory(ctx, parent)
	if err != nil {
		return storage.Read{}, err
	}
	defer root.Close()
	found, err := w.driver.find(ctx, root, parent, name)
	if err != nil {
		return storage.Read{}, err
	}
	if found.entry.Kind != storage.File {
		return storage.Read{}, storage.NewError("not_file", path, nil)
	}
	e, f, err := verifiedNative(ctx, root, name, path, found.info)
	if err != nil {
		return storage.Read{}, err
	}
	if len(options.Version) != 0 && !bytes.Equal(options.Version, e.Version) {
		f.Close()
		return storage.Read{}, storage.NewError("changed", path, nil)
	}
	length := e.Size - options.Offset
	if length < 0 || options.Length != nil && *options.Length > length {
		f.Close()
		return storage.Read{}, storage.NewError("invalid_range", path, nil)
	}
	if options.Length != nil {
		length = *options.Length
	}
	body := &contextReader{ctx: ctx, lifetime: w.driver.lifetime, file: f, reader: io.NewSectionReader(f, options.Offset, length), remaining: length, release: release}
	body.mu.Lock()
	body.stopContext = context.AfterFunc(ctx, func() { body.Close() })
	body.stopLifetime = context.AfterFunc(w.driver.lifetime, func() { body.Close() })
	body.mu.Unlock()
	if err := ctx.Err(); err != nil {
		body.Close()
		return storage.Read{}, err
	}
	success = true
	return storage.Read{Body: body, Entry: e, Offset: options.Offset, Length: length}, nil
}
