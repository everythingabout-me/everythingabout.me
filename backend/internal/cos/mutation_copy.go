package cos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

var _ objectmutation.CopyBackend = (*mutationBackend)(nil)
var _ objectmutation.PartCopyBackend = (*mutationBackend)(nil)

// CopyObject copies one object inside COS: the gateway neither reads nor writes
// its bytes. The source ETag is bound with a native condition and an existing
// target is refused by x-cos-forbid-overwrite. Metadata is replaced so the
// intent marker records which operation produced the target.
func (b *mutationBackend) CopyObject(ctx context.Context, in objectmutation.Copy) (objectmutation.Object, error) {
	if err := validateMutationCopy(in); err != nil {
		return objectmutation.Object{}, err
	}
	headers := http.Header{}
	if in.Absent {
		headers.Set("If-None-Match", "*")
		headers.Set("x-cos-forbid-overwrite", "true")
	}
	if in.ExpectedETag != "" {
		headers.Set("If-Match", in.ExpectedETag)
	}
	for key, value := range in.Metadata {
		headers.Set("x-cos-meta-"+key, value)
	}
	options := &sdk.ObjectCopyOptions{ObjectCopyHeaderOptions: &sdk.ObjectCopyHeaderOptions{
		XCosCopySourceIfMatch: in.SourceETag,
		XCosMetadataDirective: "Replaced",
		ContentType:           in.MediaType,
		XOptionHeader:         &headers,
	}}
	result, response, err := b.client.Object.Copy(ctx, in.Target, b.copySource(in.Source), options)
	if err != nil {
		return objectmutation.Object{}, mutationCopyError(in.Target, err)
	}
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 || result == nil || result.ETag == "" {
		return objectmutation.Object{}, storage.NewError("outcome_unknown", storage.Path(in.Target), nil)
	}
	modified := time.Time{}
	if raw := response.Header.Get("Last-Modified"); raw != "" {
		modified, _ = http.ParseTime(raw)
	}
	return objectmutation.Object{Key: in.Target, VersionID: response.Header.Get("x-cos-version-id"), ETag: result.ETag, Size: in.Size, MediaType: in.MediaType, ModifiedAt: modified, Metadata: cloneMutationMetadata(in.Metadata)}, nil
}

// CopyPart copies one provider-side byte range into an unpublished part session.
func (b *mutationBackend) CopyPart(ctx context.Context, key, uploadID string, number int, source, sourceETag string, offset, length int64) (string, error) {
	if key == "" || uploadID == "" || number < 1 || source == "" || sourceETag == "" || offset < 0 || length <= 0 {
		return "", storage.NewError("invalid_request", storage.Path(key), nil)
	}
	options := &sdk.ObjectCopyPartOptions{XCosCopySourceRange: fmt.Sprintf("bytes=%d-%d", offset, offset+length-1), XCosCopySourceIfMatch: sourceETag}
	result, response, err := b.client.Object.CopyPart(ctx, key, uploadID, number, b.copySource(source), options)
	if err != nil {
		return "", mutationCopyError(key, err)
	}
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 || result == nil || result.ETag == "" {
		return "", storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	return result.ETag, nil
}

func (b *mutationBackend) copySource(key string) string {
	return fmt.Sprintf("%s.cos.%s.myqcloud.com/%s", b.bucket, b.region, key)
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

// mutationCopyError maps the failure of a copy request. COS answers PutObject -
// Copy with 200 and an embedded error document, so the XML code decides whether
// the request certainly had no effect.
func mutationCopyError(key string, err error) error {
	var response *sdk.ErrorResponse
	if errors.As(err, &response) {
		switch response.Code {
		case "NoSuchKey", "NoSuchUpload":
			return storage.NewError("not_found", storage.Path(key), err)
		case "PreconditionFailed", "FileAlreadyExists", "Conflict":
			return storage.NewError("precondition_failed", storage.Path(key), err)
		}
		if response.Response != nil {
			switch response.Response.StatusCode {
			case http.StatusNotFound:
				return storage.NewError("not_found", storage.Path(key), err)
			case http.StatusPreconditionFailed, http.StatusConflict:
				return storage.NewError("precondition_failed", storage.Path(key), err)
			}
		}
	}
	return storage.NewError("outcome_unknown", storage.Path(key), err)
}
