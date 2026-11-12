package s3storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// mutationBackend uses a separate SDK client over the existing connection pool.
// Read-client retry policy is never changed by enabling native mutation support.
type mutationBackend struct {
	client *s3.Client
	bucket string
}

var _ objectmutation.Backend = (*mutationBackend)(nil)

// A copy, an upload or a part write lasts as long as its bytes take, so it cannot share one
// fixed deadline with a metadata request. A flat 60 seconds on every call made a large
// provider-side copy fail with an error that named the clock instead of the bytes. These
// mirror the COS driver's budgets, which is the reason both drivers now behave the same at
// large sizes.
const (
	mutationMetadataBudget    = 5 * time.Minute
	mutationMinBytesPerSecond = 5 << 20
	mutationMaxTransferBudget = 6 * time.Hour
)

// mutationMetadata bounds one request whose duration does not scale with object size.
func mutationMetadata(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, mutationMetadataBudget)
}

// mutationTransferBudget adds the time size bytes need at a deliberately pessimistic
// provider rate, so a healthy transfer never hits it and a stalled connection still does.
func mutationTransferBudget(ctx context.Context, size int64) (context.Context, context.CancelFunc) {
	budget := mutationMetadataBudget
	if size > 0 {
		budget += time.Duration(size/mutationMinBytesPerSecond) * time.Second
	}
	if budget > mutationMaxTransferBudget {
		budget = mutationMaxTransferBudget
	}
	return context.WithTimeout(ctx, budget)
}

func (f *Factory) MutationBackend() (objectmutation.Backend, error) {
	if f == nil || f.backend == nil || f.backend.client == nil || f.backend.bucket == "" {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	options := f.backend.client.Options()
	options.RetryMaxAttempts = 1
	options.Retryer = aws.NopRetryer{}
	options.HTTPClient = mutationHTTPClient{client: options.HTTPClient}
	return &mutationBackend{client: s3.New(options), bucket: f.backend.bucket}, nil
}

// Remove replayable body factories after signing, without forwarding application
// credentials or modifying signed headers. The shared HTTP client already refuses
// redirects; the AWS retryer cannot issue a second SDK attempt.
type mutationHTTPClient struct{ client s3.HTTPClient }

func (c mutationHTTPClient) Do(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.GetBody = nil
	return c.client.Do(copy)
}

func (b *mutationBackend) Head(ctx context.Context, key string) (objectmutation.Object, error) {
	ctx, cancel := mutationMetadata(ctx)
	defer cancel()
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if err != nil {
		return objectmutation.Object{}, mutationReadError(ctx, key, err, false)
	}
	if out.ContentLength == nil || *out.ContentLength < 0 {
		return objectmutation.Object{}, storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	return objectmutation.Object{Key: key, Size: *out.ContentLength, VersionID: aws.ToString(out.VersionId), ETag: aws.ToString(out.ETag), MediaType: aws.ToString(out.ContentType), ModifiedAt: aws.ToTime(out.LastModified), Metadata: cloneMutationMetadata(out.Metadata)}, nil
}
func (b *mutationBackend) Open(ctx context.Context, key, etag string) (io.ReadCloser, error) {
	if etag == "" {
		return nil, storage.NewError("unsupported", storage.Path(key), nil)
	}
	// The body streams for as long as its bytes take, so the caller's context is the only
	// deadline here: a fixed clock on the response is the same mistake this driver already
	// made on mutations, and it fires exactly on the files big enough to need the time.
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key), IfMatch: aws.String(etag)})
	if err != nil {
		return nil, mutationReadError(ctx, key, err, true)
	}
	if out.Body == nil || out.ContentLength == nil || *out.ContentLength < 0 || aws.ToString(out.ETag) != etag || out.ContentRange != nil {
		if out.Body != nil {
			out.Body.Close()
		}
		return nil, storage.NewError("changed", storage.Path(key), nil)
	}
	return &mutationBody{ReadCloser: out.Body, cancel: func() {}, remaining: *out.ContentLength}, nil
}
func (b *mutationBackend) Put(ctx context.Context, in objectmutation.Put) (objectmutation.Object, error) {
	if in.MediaType == "" {
		in.MediaType = "application/octet-stream"
	}
	if err := validateMutationPut(in); err != nil {
		return objectmutation.Object{}, err
	}
	if in.ForbidOverwrite {
		// S3 has no overwrite switch; only the conditional header expresses this, and a form
		// upload cannot send one. The form-post evidence stays false for this backend.
		return objectmutation.Object{}, storage.NewError("unsupported", storage.Path(in.Key), nil)
	}
	ctx, cancel := mutationTransferBudget(ctx, in.Size)
	defer cancel()
	input := &s3.PutObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(in.Key), Body: in.Body, ContentLength: aws.Int64(in.Size), ContentType: aws.String(in.MediaType), Metadata: cloneMutationMetadata(in.Metadata)}
	if in.Absent {
		input.IfNoneMatch = aws.String("*")
	}
	if in.ExpectedETag != "" {
		input.IfMatch = aws.String(in.ExpectedETag)
	}
	out, err := b.client.PutObject(ctx, input)
	if err != nil {
		return objectmutation.Object{}, mutationWriteError(in.Key, err)
	}
	return objectmutation.Object{Key: in.Key, VersionID: aws.ToString(out.VersionId), ETag: aws.ToString(out.ETag), Size: in.Size, MediaType: in.MediaType, Metadata: cloneMutationMetadata(in.Metadata)}, nil
}
func (b *mutationBackend) Delete(ctx context.Context, key, expectedETag string) (objectmutation.Object, error) {
	if key == "" || strings.ContainsAny(expectedETag, "\r\n\x00") {
		return objectmutation.Object{}, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	ctx, cancel := mutationMetadata(ctx)
	defer cancel()
	input := &s3.DeleteObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)}
	if expectedETag != "" {
		input.IfMatch = aws.String(expectedETag)
	}
	// No VersionId: deletion changes the current key and may create a native marker.
	out, err := b.client.DeleteObject(ctx, input)
	if err != nil {
		return objectmutation.Object{}, mutationWriteError(key, err)
	}
	return objectmutation.Object{Key: key, VersionID: aws.ToString(out.VersionId)}, nil
}
func cloneMutationMetadata(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func validateMutationPut(in objectmutation.Put) error {
	if in.Key == "" || in.Size < 0 || in.Size > 9007199254740991 || in.Body == nil || in.Absent && in.ExpectedETag != "" || strings.ContainsAny(in.ExpectedETag+in.MediaType, "\r\n\x00") {
		return storage.NewError("invalid_request", storage.Path(in.Key), nil)
	}
	for key, value := range in.Metadata {
		if key == "" || strings.ContainsAny(key, " \t\r\n\x00:") || strings.ContainsAny(value, "\r\n\x00") {
			return storage.NewError("invalid_request", storage.Path(in.Key), nil)
		}
	}
	return nil
}
func mutationWriteError(key string, err error) error {
	code := "outcome_unknown"
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		switch response.HTTPStatusCode() {
		case http.StatusPreconditionFailed:
			code = "precondition_failed"
		}
		return storage.NewProviderError(code, storage.Path(key), response.HTTPStatusCode(), providerCode(err), providerRequestID(err), err)
	}
	return storage.NewError(code, storage.Path(key), err)
}
func mutationReadError(ctx context.Context, key string, err error, versioned bool) error {
	if ctx.Err() != nil {
		return storage.NewError("backend_error", storage.Path(key), ctx.Err())
	}
	code := "backend_error"
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		switch response.HTTPStatusCode() {
		case 404:
			code = "not_found"
			if versioned {
				code = "changed"
			}
		case 412:
			code = "changed"
		}
		return storage.NewProviderError(code, storage.Path(key), response.HTTPStatusCode(), providerCode(err), providerRequestID(err), err)
	}
	return storage.NewError(code, storage.Path(key), err)
}

type mutationBody struct {
	io.ReadCloser
	cancel    context.CancelFunc
	remaining int64
	once      sync.Once
	closeErr  error
}

func (b *mutationBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if err == io.EOF && b.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func (b *mutationBody) Close() error {
	b.once.Do(func() { b.closeErr = b.ReadCloser.Close(); b.cancel() })
	return b.closeErr
}
