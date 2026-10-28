// Package storage defines the object storage interface. Business code depends only on it;
// the concrete implementation (Tencent Cloud COS for now) lives in internal/cos.
package storage

import (
	"context"
	"io"
	"time"
)

type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	StorageClass string
}

type Page struct {
	Objects    []ObjectInfo
	NextMarker string
}

// MultipartPart is one part's number and ETag in a multipart upload.
type MultipartPart struct {
	PartNumber int
	ETag       string
}

// PartInfo describes a part the server already holds, so a client can resume after a drop.
type PartInfo struct {
	PartNumber int    `json:"partNumber"`
	Size       int64  `json:"size"`
	ETag       string `json:"etag"`
}

type URLOptions struct {
	// ExpiresSeconds defaults to 900.
	ExpiresSeconds int
	// DownloadName, when set, is passed through response-content-disposition.
	DownloadName string
	ContentType  string
	// Process is a storage-side processing parameter (Tencent Cloud data processing) and is
	// signed along with the URL. For example "imageMogr2/thumbnail/640x>" yields a proportional
	// thumbnail without going through this service.
	Process string
}

// Credentials go straight back to the browser; the field names match what the COS JS SDK expects.
type Credentials struct {
	TmpSecretID  string `json:"tmpSecretId"`
	TmpSecretKey string `json:"tmpSecretKey"`
	SessionToken string `json:"sessionToken"`
	StartTime    int64  `json:"startTime"`
	ExpiredTime  int64  `json:"expiredTime"`
}

type Storage interface {
	Head(ctx context.Context, key string) (*ObjectInfo, error)
	// Get reads an object's content. Only for controlled small files (parsing data files);
	// do not push large files through here.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Copy is a server-side copy and does not use local bandwidth.
	Copy(ctx context.Context, sourceKey, targetKey string) error
	Remove(ctx context.Context, key string) error
	// PutEmpty writes a zero-byte object, used as a directory placeholder.
	PutEmpty(ctx context.Context, key string) error
	// Put writes all of r into key. size must be the exact byte count (it becomes
	// Content-Length); an empty contentType is treated as application/octet-stream.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (*ObjectInfo, error)
	// List enumerates page by page; return false from the callback to stop early.
	List(ctx context.Context, prefix string, onPage func(Page) (bool, error)) error
	// GetURL builds a presigned URL for downloads, inline by default.
	GetURL(ctx context.Context, key string, opts URLOptions) (string, error)
	// PutURL builds a presigned PUT URL for uploads. The client sends bytes straight to object
	// storage without passing through this service; the URL expires after expiresSeconds.
	PutURL(ctx context.Context, key string, expiresSeconds int) (string, error)

	// Multipart upload (objects over 5 GiB). The client PUTs each part straight to a part URL,
	// and the server merges them with CompleteMultipart at the end.
	InitiateMultipart(ctx context.Context, key string) (uploadID string, err error)
	MultipartPartURL(ctx context.Context, key, uploadID string, partNumber, expiresSeconds int) (string, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart) (*ObjectInfo, error)
	AbortMultipart(ctx context.Context, key, uploadID string) error
	// ListParts lists the parts already uploaded (for resuming). A missing upload gives an
	// empty list.
	ListParts(ctx context.Context, key, uploadID string) ([]PartInfo, error)
	// TemporaryCredentials mints the temporary credentials a direct browser upload needs.
	TemporaryCredentials(ctx context.Context, key string, expiresSeconds int) (*Credentials, error)
}
