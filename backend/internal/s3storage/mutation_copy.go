package s3storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

var _ objectmutation.CopyBackend = (*mutationBackend)(nil)
var _ objectmutation.PartCopyBackend = (*mutationBackend)(nil)

// CopyObject copies one object inside the provider. The source ETag is bound
// with a native condition and an existing target is refused by If-None-Match.
// Metadata is replaced so the intent marker records which operation produced it.
func (b *mutationBackend) CopyObject(ctx context.Context, in objectmutation.Copy) (objectmutation.Object, error) {
	if err := validateMutationCopy(in); err != nil {
		return objectmutation.Object{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	input := &s3.CopyObjectInput{
		Bucket:            aws.String(b.bucket),
		Key:               aws.String(in.Target),
		CopySource:        aws.String(copySource(b.bucket, in.Source)),
		CopySourceIfMatch: aws.String(in.SourceETag),
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       aws.String(in.MediaType),
		Metadata:          cloneMutationMetadata(in.Metadata),
	}
	if in.Absent {
		input.IfNoneMatch = aws.String("*")
	}
	if in.ExpectedETag != "" {
		input.IfMatch = aws.String(in.ExpectedETag)
	}
	out, err := b.client.CopyObject(ctx, input)
	if err != nil {
		return objectmutation.Object{}, mutationCopyError(in.Target, err)
	}
	if out == nil || out.CopyObjectResult == nil || aws.ToString(out.CopyObjectResult.ETag) == "" {
		return objectmutation.Object{}, storage.NewError("outcome_unknown", storage.Path(in.Target), nil)
	}
	modified := time.Time{}
	if out.CopyObjectResult.LastModified != nil {
		modified = *out.CopyObjectResult.LastModified
	}
	return objectmutation.Object{Key: in.Target, VersionID: aws.ToString(out.VersionId), ETag: aws.ToString(out.CopyObjectResult.ETag), Size: in.Size, MediaType: in.MediaType, ModifiedAt: modified, Metadata: cloneMutationMetadata(in.Metadata)}, nil
}

// CopyPart copies one provider-side byte range into an unpublished part session.
func (b *mutationBackend) CopyPart(ctx context.Context, key, uploadID string, number int, source, sourceETag string, offset, length int64) (string, error) {
	if key == "" || uploadID == "" || number < 1 || source == "" || sourceETag == "" || offset < 0 || length <= 0 {
		return "", storage.NewError("invalid_request", storage.Path(key), nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := b.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
		Bucket:            aws.String(b.bucket),
		Key:               aws.String(key),
		UploadId:          aws.String(uploadID),
		PartNumber:        aws.Int32(int32(number)),
		CopySource:        aws.String(copySource(b.bucket, source)),
		CopySourceRange:   aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
		CopySourceIfMatch: aws.String(sourceETag),
	})
	if err != nil {
		return "", mutationCopyError(key, err)
	}
	if out == nil || out.CopyPartResult == nil || aws.ToString(out.CopyPartResult.ETag) == "" {
		return "", storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	return aws.ToString(out.CopyPartResult.ETag), nil
}

// copySource encodes the provider copy source per path segment so a key
// containing '+' or spaces addresses the same object the mutation wrote.
func copySource(bucket, key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return bucket + "/" + strings.Join(segments, "/")
}

func validateMutationCopy(in objectmutation.Copy) error {
	if in.Source == "" || in.Target == "" || in.Source == in.Target || in.SourceETag == "" || strings.ContainsAny(in.SourceETag+in.ExpectedETag+in.MediaType, "\r\n\x00") {
		return storage.NewError("invalid_request", storage.Path(in.Target), nil)
	}
	for key, value := range in.Metadata {
		if key == "" || strings.ContainsAny(key, " \t\r\n\x00:") || strings.ContainsAny(value, "\r\n\x00") {
			return storage.NewError("invalid_request", storage.Path(in.Target), nil)
		}
	}
	return nil
}

// mutationCopyError maps a copy failure onto safe storage semantics. Only a
// provider precondition proves the request had no effect; everything else stays
// ambiguous for recovery to inspect.
func mutationCopyError(key string, err error) error {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NoSuchUpload", "NotFound":
			return storage.NewError("not_found", storage.Path(key), err)
		case "PreconditionFailed", "ConditionalRequestConflict":
			return storage.NewError("precondition_failed", storage.Path(key), err)
		}
	}
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) {
		switch response.HTTPStatusCode() {
		case http.StatusNotFound:
			return storage.NewError("not_found", storage.Path(key), err)
		case http.StatusPreconditionFailed, http.StatusConflict:
			return storage.NewError("precondition_failed", storage.Path(key), err)
		}
	}
	return storage.NewError("outcome_unknown", storage.Path(key), err)
}
