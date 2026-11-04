package cos

import (
	"context"
	"io"
	"net/http"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

var _ objectmutation.MultipartBackend = (*mutationBackend)(nil)

func (b *mutationBackend) BeginMultipart(ctx context.Context, in objectmutation.Put) (string, error) {
	headers := http.Header{}
	for k, v := range in.Metadata {
		headers.Set("x-cos-meta-"+k, v)
	}
	result, response, err := b.client.Object.InitiateMultipartUpload(ctx, in.Key, &sdk.InitiateMultipartUploadOptions{ObjectPutHeaderOptions: &sdk.ObjectPutHeaderOptions{ContentType: in.MediaType, XOptionHeader: &headers}})
	if err != nil {
		return "", mutationWriteError(in.Key, err)
	}
	if result == nil || result.UploadID == "" || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", storage.NewError("invalid_backend_response", storage.Path(in.Key), nil)
	}
	return result.UploadID, nil
}
func (b *mutationBackend) PutPart(ctx context.Context, key, id string, number int, body io.Reader, size int64) (string, error) {
	response, err := b.client.Object.UploadPart(ctx, key, id, number, body, &sdk.ObjectUploadPartOptions{ContentLength: size})
	if err != nil {
		return "", mutationWriteError(key, err)
	}
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 || response.Header.Get("ETag") == "" {
		return "", storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	return response.Header.Get("ETag"), nil
}
func (b *mutationBackend) CompleteMultipart(ctx context.Context, in objectmutation.Put, id string, parts []objectmutation.NativePart) (objectmutation.Object, error) {
	headers := http.Header{}
	if in.Absent {
		headers.Set("If-None-Match", "*")
		headers.Set("x-cos-forbid-overwrite", "true")
	}
	if in.ExpectedETag != "" {
		headers.Set("If-Match", in.ExpectedETag)
	}
	options := &sdk.CompleteMultipartUploadOptions{XOptionHeader: &headers}
	for _, part := range parts {
		options.Parts = append(options.Parts, sdk.Object{PartNumber: part.Number, ETag: part.ETag})
	}
	result, response, err := b.client.Object.CompleteMultipartUpload(ctx, in.Key, id, options)
	if err != nil {
		return objectmutation.Object{}, mutationPublishError(in, err)
	}
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 || result == nil {
		return objectmutation.Object{}, storage.NewError("outcome_unknown", storage.Path(in.Key), nil)
	}
	modified := time.Time{}
	if raw := response.Header.Get("Last-Modified"); raw != "" {
		modified, _ = http.ParseTime(raw)
	}
	return objectmutation.Object{Key: in.Key, VersionID: response.Header.Get("x-cos-version-id"), ETag: result.ETag, Size: in.Size, MediaType: in.MediaType, ModifiedAt: modified, Metadata: cloneMutationMetadata(in.Metadata)}, nil
}
func (b *mutationBackend) AbortMultipart(ctx context.Context, key, id string) error {
	_, err := b.client.Object.AbortMultipartUpload(ctx, key, id)
	if err != nil {
		mapped := mutationReadError(ctx, key, err, false)
		if storage.IsCode(mapped, "not_found") {
			return nil
		}
		return mapped
	}
	return nil
}

var _ objectmutation.MultipartInspector = (*mutationBackend)(nil)

// InspectMultipart asks COS whether this exact upload session still exists. A missing
// session (NoSuchUpload) is the terminal observation that none of its parts can ever be
// completed, read, or replayed; any other failure leaves the allocation unknown.
func (b *mutationBackend) InspectMultipart(ctx context.Context, key, id string) (objectmutation.MultipartState, error) {
	result, response, err := b.client.Object.ListParts(ctx, key, id, &sdk.ObjectListPartsOptions{MaxParts: "1000"})
	if err != nil {
		if sdk.IsNotFoundError(err) {
			return objectmutation.MultipartState{}, nil
		}
		return objectmutation.MultipartState{}, mutationReadError(ctx, key, err, false)
	}
	if response == nil || result == nil {
		return objectmutation.MultipartState{}, storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	if response.StatusCode == http.StatusNotFound {
		return objectmutation.MultipartState{}, nil
	}
	return objectmutation.MultipartState{Present: true, Parts: len(result.Parts)}, nil
}
