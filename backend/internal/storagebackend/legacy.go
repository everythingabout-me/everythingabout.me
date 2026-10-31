package storagebackend

import (
	"context"
	"errors"
	"io"
	"strings"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/storage"
)

// This bridge only supports legacy internal metadata parsers until P2 wires the
// new read API. It never gives a new backend native write authority or public URLs.
type readOnlyLegacy struct{ backend *Backend }

func unsupported() error {
	return httpx.NewError(501, "this storage backend is read-only during the file API migration")
}
func splitLegacyKey(key string) (string, storage.Path, error) {
	if !strings.HasPrefix(key, "drive/") {
		return "", "", storage.NewError("invalid_name", "", nil)
	}
	owner, path, ok := strings.Cut(strings.TrimPrefix(key, "drive/"), "/")
	if !ok || owner == "" {
		return "", "", storage.NewError("invalid_name", "", nil)
	}
	if strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	if err := storage.ValidatePath(storage.Path(path)); err != nil {
		return "", "", err
	}
	return owner, storage.Path(path), nil
}
func (l *readOnlyLegacy) Head(ctx context.Context, key string) (*storage.ObjectInfo, error) {
	owner, path, err := splitLegacyKey(key)
	if err != nil {
		return nil, err
	}
	d, err := l.backend.Open(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	e, err := d.Stat(ctx, path)
	if storage.IsCode(err, "not_found") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &storage.ObjectInfo{Key: key, Size: e.Size, LastModified: e.ModifiedAt}, nil
}
func (l *readOnlyLegacy) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	owner, path, err := splitLegacyKey(key)
	if err != nil {
		return nil, err
	}
	d, err := l.backend.Open(ctx, owner)
	if err != nil {
		return nil, err
	}
	r, err := d.Open(ctx, path, storage.ReadOptions{})
	if err != nil {
		d.Close()
		return nil, err
	}
	return &boundRead{ReadCloser: r.Body, driver: d}, nil
}

type boundRead struct {
	io.ReadCloser
	driver storage.Driver
}

func (r *boundRead) Close() error                                    { return errors.Join(r.ReadCloser.Close(), r.driver.Close()) }
func (l *readOnlyLegacy) Copy(context.Context, string, string) error { return unsupported() }
func (l *readOnlyLegacy) Remove(context.Context, string) error       { return unsupported() }
func (l *readOnlyLegacy) PutEmpty(context.Context, string) error     { return unsupported() }
func (l *readOnlyLegacy) Put(context.Context, string, io.Reader, int64, string) (*storage.ObjectInfo, error) {
	return nil, unsupported()
}
func (l *readOnlyLegacy) List(context.Context, string, func(storage.ObjectPage) (bool, error)) error {
	return unsupported()
}
func (l *readOnlyLegacy) GetURL(context.Context, string, storage.URLOptions) (string, error) {
	return "", unsupported()
}
func (l *readOnlyLegacy) PutURL(context.Context, string, int) (string, error) {
	return "", unsupported()
}
func (l *readOnlyLegacy) InitiateMultipart(context.Context, string) (string, error) {
	return "", unsupported()
}
func (l *readOnlyLegacy) MultipartPartURL(context.Context, string, string, int, int) (string, error) {
	return "", unsupported()
}
func (l *readOnlyLegacy) CompleteMultipart(context.Context, string, string, []storage.MultipartPart) (*storage.ObjectInfo, error) {
	return nil, unsupported()
}
func (l *readOnlyLegacy) AbortMultipart(context.Context, string, string) error { return unsupported() }
func (l *readOnlyLegacy) ListParts(context.Context, string, string) ([]storage.PartInfo, error) {
	return nil, unsupported()
}
func (l *readOnlyLegacy) TemporaryCredentials(context.Context, string, int) (*storage.Credentials, error) {
	return nil, unsupported()
}

var _ storage.Storage = (*readOnlyLegacy)(nil)
