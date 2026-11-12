package cos

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

var _ objectmutation.MultipartBackend = (*mutationBackend)(nil)

func (b *mutationBackend) BeginMultipart(ctx context.Context, in objectmutation.Put) (string, error) {
	ctx, cancel := metadataDeadline(ctx)
	defer cancel()
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
	ctx, cancel := transferBudget(ctx, size)
	defer cancel()
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
	// A completion assembles the uploaded parts provider-side, so its duration follows the
	// object's size and not a fixed number of seconds: the 60 second client timeout this
	// used to sit under cut exactly the large uploads that needed the most time.
	ctx, cancel := transferBudget(ctx, in.Size)
	defer cancel()
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
	// Recovery calls this with context.WithoutCancel, so without a per-request budget it
	// would have no deadline at all once the client timeout is gone.
	ctx, cancel := metadataDeadline(ctx)
	defer cancel()
	_, err := b.client.Object.AbortMultipartUpload(ctx, key, id)
	if err != nil {
		mapped := mutationReadError(ctx, key, err, false)
		if !storage.IsCode(mapped, "not_found") {
			return mapped
		}
		// NoSuchUpload says only that the session is gone, and COS removes a session
		// when Complete succeeds just as it does when an abort wins. Reporting this as
		// a successful abort would let the caller record "nothing applied" for an
		// upload that already published. Report the observation itself; the caller
		// holds this operation's intent marker and must attribute the destination
		// object before it concludes anything about publication.
		return abortSessionAbsent(key, mapped)
	}
	return nil
}

// abortSessionAbsent keeps the provider's own facts — the 404, NoSuchUpload and the
// request id — under the code that states what was actually observed.
func abortSessionAbsent(key string, observed error) error {
	var e *storage.Error
	if errors.As(observed, &e) && e.Provider != nil {
		return storage.NewProviderError("abort_session_absent", storage.Path(key), e.Provider.Status, e.Provider.Code, e.Provider.RequestID, observed)
	}
	return storage.NewError("abort_session_absent", storage.Path(key), observed)
}

var _ objectmutation.MultipartInspector = (*mutationBackend)(nil)

// InspectMultipart asks COS whether this exact upload session still exists. A missing
// session (NoSuchUpload) is the terminal observation that none of its parts can ever be
// completed, read, or replayed; any other failure leaves the allocation unknown. It is
// evidence about the session only: a completed upload also removes the session, so this
// answer never says whether a publication landed.
func (b *mutationBackend) InspectMultipart(ctx context.Context, key, id string) (objectmutation.MultipartState, error) {
	ctx, cancel := metadataDeadline(ctx)
	defer cancel()
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
