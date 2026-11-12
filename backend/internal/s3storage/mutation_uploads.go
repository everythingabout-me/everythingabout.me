package s3storage

import (
	"context"
	"errors"
	"io"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var _ objectmutation.MultipartBackend = (*mutationBackend)(nil)

func (b *mutationBackend) BeginMultipart(ctx context.Context, in objectmutation.Put) (string, error) {
	ctx, cancel := mutationMetadata(ctx)
	defer cancel()
	out, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(b.bucket), Key: aws.String(in.Key), ContentType: aws.String(in.MediaType), Metadata: cloneMutationMetadata(in.Metadata)})
	if err != nil {
		return "", mutationWriteError(in.Key, err)
	}
	if out == nil || aws.ToString(out.UploadId) == "" {
		return "", storage.NewError("invalid_backend_response", storage.Path(in.Key), nil)
	}
	return aws.ToString(out.UploadId), nil
}
func (b *mutationBackend) PutPart(ctx context.Context, key, id string, number int, body io.Reader, size int64) (string, error) {
	ctx, cancel := mutationTransferBudget(ctx, size)
	defer cancel()
	// The body is a single-pass stream whose digest is finalized during transport.
	// Keep SigV4 request authentication without the SDK's pre-signing body rewind.
	out, err := b.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(id), PartNumber: aws.Int32(int32(number)), Body: body, ContentLength: aws.Int64(size)}, func(options *s3.Options) {
		options.APIOptions = append(options.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	})
	if err != nil {
		return "", mutationWriteError(key, err)
	}
	if out == nil || aws.ToString(out.ETag) == "" {
		return "", storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	return aws.ToString(out.ETag), nil
}
func (b *mutationBackend) CompleteMultipart(ctx context.Context, in objectmutation.Put, id string, parts []objectmutation.NativePart) (objectmutation.Object, error) {
	ctx, cancel := mutationMetadata(ctx)
	defer cancel()
	manifest := &types.CompletedMultipartUpload{}
	for _, p := range parts {
		manifest.Parts = append(manifest.Parts, types.CompletedPart{PartNumber: aws.Int32(int32(p.Number)), ETag: aws.String(p.ETag)})
	}
	input := &s3.CompleteMultipartUploadInput{Bucket: aws.String(b.bucket), Key: aws.String(in.Key), UploadId: aws.String(id), MultipartUpload: manifest}
	if in.Absent {
		input.IfNoneMatch = aws.String("*")
	}
	if in.ExpectedETag != "" {
		input.IfMatch = aws.String(in.ExpectedETag)
	}
	out, err := b.client.CompleteMultipartUpload(ctx, input)
	if err != nil {
		return objectmutation.Object{}, mutationWriteError(in.Key, err)
	}
	return objectmutation.Object{Key: in.Key, VersionID: aws.ToString(out.VersionId), ETag: aws.ToString(out.ETag), Size: in.Size, MediaType: in.MediaType, Metadata: cloneMutationMetadata(in.Metadata)}, nil
}

var _ objectmutation.MultipartInspector = (*mutationBackend)(nil)

// InspectMultipart asks the provider whether this exact upload session still exists. A
// missing session (NoSuchUpload) is the terminal observation that none of its parts can
// ever be completed, read, or replayed; any other failure leaves the allocation unknown.
// It is evidence about the session only: a completed upload also removes the session, so
// this answer never says whether a publication landed.
func (b *mutationBackend) InspectMultipart(ctx context.Context, key, id string) (objectmutation.MultipartState, error) {
	ctx, cancel := mutationMetadata(ctx)
	defer cancel()
	out, err := b.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(id)})
	if err != nil {
		mapped := mutationReadError(ctx, key, err, false)
		if storage.IsCode(mapped, "not_found") {
			return objectmutation.MultipartState{}, nil
		}
		return objectmutation.MultipartState{}, mapped
	}
	if out == nil {
		return objectmutation.MultipartState{}, storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	return objectmutation.MultipartState{Present: true, Parts: len(out.Parts)}, nil
}

func (b *mutationBackend) AbortMultipart(ctx context.Context, key, id string) error {
	ctx, cancel := mutationMetadata(ctx)
	defer cancel()
	_, err := b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(id)})
	if err != nil {
		mapped := mutationReadError(ctx, key, err, false)
		if !storage.IsCode(mapped, "not_found") {
			return mapped
		}
		// NoSuchUpload says only that the session is gone, and S3 removes a session
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
