package cos

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

var _ objectmutation.SignedMultipartBackend = (*mutationBackend)(nil)

// SignedPartsSupported never falls back to the broadly privileged mutation signer.
func (b *mutationBackend) SignedPartsSupported() bool { return b.signer != nil }

func (b *mutationBackend) SignPart(ctx context.Context, key, id string, number int, size int64, md5 string, expires time.Time) (storage.SignedPartGrant, error) {
	var out storage.SignedPartGrant
	if !b.SignedPartsSupported() {
		return out, storage.NewError("unsupported", storage.Path(key), nil)
	}
	query := url.Values{"partNumber": {strconv.Itoa(number)}, "uploadId": {id}}
	headers := http.Header{"Content-Md5": {md5}, "Content-Length": {strconv.FormatInt(size, 10)}, "Content-Type": {"application/octet-stream"}}
	duration := time.Until(expires)
	if duration <= 0 || duration > 6*time.Minute {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	signed, err := b.signer.Object.GetPresignedURL2(ctx, http.MethodPut, key, duration, &sdk.PresignedURLOptions{Query: &query, Header: &headers}, true)
	if err != nil {
		return out, err
	}
	return storage.SignedPartGrant{Method: http.MethodPut, URL: signed.String(), Headers: map[string]string{"Content-MD5": md5, "Content-Type": "application/octet-stream"}, ExpiresAt: expires.UTC()}, nil
}
func (b *mutationBackend) QueryPart(ctx context.Context, key, id string, number int) (objectmutation.NativePartInfo, bool, error) {
	result, response, err := b.client.Object.ListParts(ctx, key, id, &sdk.ObjectListPartsOptions{MaxParts: "1", PartNumberMarker: strconv.Itoa(number - 1)})
	if err != nil {
		return objectmutation.NativePartInfo{}, false, mutationReadError(ctx, key, err, false)
	}
	if result == nil || response == nil || response.StatusCode != http.StatusOK {
		return objectmutation.NativePartInfo{}, false, storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	if len(result.Parts) > 1 {
		return objectmutation.NativePartInfo{}, false, storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	for _, part := range result.Parts {
		if part.PartNumber == number {
			return objectmutation.NativePartInfo{Size: part.Size, ETag: part.ETag}, true, nil
		}
	}
	return objectmutation.NativePartInfo{}, false, nil
}
