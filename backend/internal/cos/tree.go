package cos

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"everythingabout.me/backend/internal/objecttree"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

// TreeFactory binds read-only native trees to the Client's existing SDK and pool.
// Closing a bound driver does not close the shared legacy client.
type TreeFactory struct {
	client *Client
	secret []byte
}

func NewTreeFactory(client *Client) (*TreeFactory, error) {
	if client == nil || client.client == nil || client.bucket == "" {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	secret, err := objecttree.NewSecret()
	if err != nil {
		return nil, err
	}
	return &TreeFactory{client: client, secret: secret}, nil
}
func (c *Client) TreeFactory() (*TreeFactory, error) { return NewTreeFactory(c) }
func (f *TreeFactory) Open(ctx context.Context, cfg storage.Config) (storage.Driver, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return objecttree.Bind(cosTreeBackend{f.client}, cfg, f.secret)
}

type cosTreeBackend struct{ client *Client }

func (b cosTreeBackend) Head(ctx context.Context, key string) (objecttree.Object, error) {
	response, err := b.client.client.Object.Head(ctx, key, nil)
	if err != nil {
		return objecttree.Object{}, cosTreeError(ctx, err, false)
	}
	if response == nil || response.StatusCode != http.StatusOK {
		return objecttree.Object{}, storage.NewError("invalid_backend_response", "", nil)
	}
	obj, err := cosTreeObject(key, response.Header)
	return obj, err
}
func (b cosTreeBackend) List(ctx context.Context, prefix, cursor string) (objecttree.NativePage, error) {
	result, _, err := b.client.client.Bucket.Get(ctx, &sdk.BucketGetOptions{Prefix: prefix, Delimiter: "/", Marker: cursor, MaxKeys: 1000})
	if err != nil {
		return objecttree.NativePage{}, cosTreeError(ctx, err, false)
	}
	if result == nil || result.EncodingType != "" {
		return objecttree.NativePage{}, storage.NewError("invalid_backend_response", "", nil)
	}
	page := objecttree.NativePage{Prefixes: result.CommonPrefixes, Truncated: result.IsTruncated}
	if result.IsTruncated {
		page.Next = result.NextMarker
	}
	for _, obj := range result.Contents {
		modified := time.Time{}
		if obj.LastModified != "" {
			var err error
			modified, err = time.Parse(time.RFC3339, obj.LastModified)
			if err != nil {
				return objecttree.NativePage{}, storage.NewError("invalid_backend_response", "", err)
			}
		}
		// A native listing already answers each entry's ETag. Dropping it here made every
		// listed child cost another HEAD to recover it, which turned one folder listing
		// into one request per child.
		page.Objects = append(page.Objects, objecttree.Object{Key: obj.Key, Size: obj.Size, ModifiedAt: modified, ETag: obj.ETag})
	}
	// COS's native delimiter listing supplies NextMarker. Do not synthesize one:
	// doing so can skip common prefixes or hide an incomplete native response.
	return page, nil
}
func (b cosTreeBackend) Get(ctx context.Context, key, byteRange, version string) (objecttree.Response, error) {
	var response *sdk.Response
	var err error
	options := &sdk.ObjectGetOptions{Range: byteRange}
	if version != "" {
		response, err = b.client.client.Object.Get(ctx, key, options, url.QueryEscape(version))
	} else {
		response, err = b.client.client.Object.Get(ctx, key, options)
	}
	if err != nil {
		return objecttree.Response{}, cosTreeError(ctx, err, version != "")
	}
	if response == nil {
		return objecttree.Response{}, storage.NewError("invalid_backend_response", "", nil)
	}
	obj, err := cosTreeObject(key, response.Header)
	if err != nil {
		response.Body.Close()
		return objecttree.Response{}, err
	}
	return objecttree.Response{Object: obj, Body: response.Body, Status: response.StatusCode, ContentRange: response.Header.Get("Content-Range"), Length: obj.Size}, nil
}
func cosTreeObject(key string, header http.Header) (objecttree.Object, error) {
	size, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		return objecttree.Object{}, storage.NewError("invalid_backend_response", "", nil)
	}
	modified := time.Time{}
	if value := header.Get("Last-Modified"); value != "" {
		modified, err = http.ParseTime(value)
		if err != nil {
			return objecttree.Object{}, storage.NewError("invalid_backend_response", "", err)
		}
	}
	// The ETag is kept exactly as COS spells it (quoted): the same bytes go back in
	// If-Match, and a listing reports the identical value, so a revision captured
	// from either place compares equal.
	return objecttree.Object{Key: key, Size: size, ModifiedAt: modified, MediaType: header.Get("Content-Type"), VersionID: header.Get("x-cos-version-id"), ETag: header.Get("ETag")}, nil
}
func cosTreeError(ctx context.Context, err error, versioned bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var response *sdk.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		code := "backend_error"
		switch response.Response.StatusCode {
		case http.StatusNotFound:
			code = "not_found"
			if versioned {
				code = "changed"
			}
		case http.StatusPreconditionFailed:
			code = "changed"
		case http.StatusRequestedRangeNotSatisfiable:
			code = "invalid_range"
		case http.StatusNotImplemented:
			code = "unsupported"
		}
		return storage.NewProviderError(code, "", response.Response.StatusCode, response.Code, response.RequestID, err)
	}
	return storage.NewError("backend_error", "", err)
}

var _ storage.Factory = (*TreeFactory)(nil)
