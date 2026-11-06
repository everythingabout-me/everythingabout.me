package cos

import (
	"context"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

var _ objectmutation.SignedPartBackend = (*mutationBackend)(nil)
var _ objectmutation.ReadSignerBackend = (*mutationBackend)(nil)

// SignedPartsSupported never falls back to the broadly privileged mutation signer.
func (b *mutationBackend) SignedPartsSupported() bool { return b.signer != nil }

func (b *mutationBackend) SignPart(ctx context.Context, key, id string, number int, size int64, expires time.Time) (storage.SignedPartGrant, error) {
	var out storage.SignedPartGrant
	if !b.SignedPartsSupported() {
		return out, storage.NewError("unsupported", storage.Path(key), nil)
	}
	query := url.Values{"partNumber": {strconv.Itoa(number)}, "uploadId": {id}}
	// A class A grant binds the exact part length and no content digest. The
	// length is deterministic for the session geometry, so it can be signed.
	headers := http.Header{"Content-Length": {strconv.FormatInt(size, 10)}, "Content-Type": {"application/octet-stream"}}
	duration := time.Until(expires)
	if duration <= 0 || duration > time.Hour {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	signed, err := b.signer.Object.GetPresignedURL2(ctx, http.MethodPut, key, duration, &sdk.PresignedURLOptions{Query: &query, Header: &headers}, true)
	if err != nil {
		return out, err
	}
	return storage.SignedPartGrant{
		Number: number, Size: size, Method: http.MethodPut, URL: signed.String(),
		Headers:   map[string]string{"Content-Length": strconv.FormatInt(size, 10), "Content-Type": "application/octet-stream"},
		ExpiresAt: expires.UTC(),
	}, nil
}

func (b *mutationBackend) ListParts(ctx context.Context, key, id string, after, limit int) ([]storage.NativeUploadPart, bool, error) {
	if limit < 1 {
		limit = 1000
	}
	if limit > 1000 {
		limit = 1000
	}
	marker := ""
	if after > 0 {
		marker = strconv.Itoa(after)
	}
	result, response, err := b.client.Object.ListParts(ctx, key, id, &sdk.ObjectListPartsOptions{MaxParts: strconv.Itoa(limit), PartNumberMarker: marker})
	if err != nil {
		return nil, false, mutationReadError(ctx, key, err, false)
	}
	if result == nil || response == nil || response.StatusCode != http.StatusOK {
		return nil, false, storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	parts := make([]storage.NativeUploadPart, 0, len(result.Parts))
	for _, part := range result.Parts {
		parts = append(parts, storage.NativeUploadPart{Number: part.PartNumber, Size: part.Size, ETag: part.ETag})
	}
	return parts, result.IsTruncated, nil
}

func (b *mutationBackend) ReadSigningSupported() bool { return b.signer != nil }

func (b *mutationBackend) PresignRead(ctx context.Context, key string, options storage.ReadGrantOptions) (storage.ReadGrant, error) {
	var out storage.ReadGrant
	if b.signer == nil {
		return out, storage.NewError("unsupported", storage.Path(key), nil)
	}
	expires := options.ExpiresAt
	if expires.IsZero() {
		expires = time.Now().UTC().Add(time.Hour)
	}
	duration := time.Until(expires)
	if duration <= 0 || duration > time.Hour {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	disposition := "attachment"
	if options.Disposition == "inline" {
		disposition = "inline"
	}
	mediaType := options.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	filename := path.Base(key)
	query := url.Values{}
	query.Set("response-content-type", mediaType)
	query.Set("response-content-disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(filename))
	signed, err := b.signer.Object.GetPresignedURL3(ctx, http.MethodGet, key, duration, &sdk.PresignedURLOptions{Query: &query})
	if err != nil {
		return out, err
	}
	return storage.ReadGrant{Kind: "url", URL: signed.String(), ExpiresAt: expires.UTC()}, nil
}
