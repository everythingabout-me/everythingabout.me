package cos

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

type mutationBackend struct {
	client         *sdk.Client
	bucket, region string
	// The form-post signature is computed here, not by the SDK, so the backend keeps the
	// credential the connection was built with. It never leaves the process.
	secretID, secretKey, token string
}

var _ objectmutation.Backend = (*mutationBackend)(nil)

// MutationBackend reuses the native transport, not the read client's retry state.
func (c *Client) MutationBackend() (objectmutation.Backend, error) {
	if c == nil || c.client == nil || c.client.BaseURL == nil || c.client.BaseURL.BucketURL == nil || c.bucket == "" || c.cred.SecretID == "" || c.cred.SecretKey == "" || c.transport == nil {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	native := c.native
	if native == nil {
		native = c.transport
	}
	base := *c.client.BaseURL
	token := ""
	if credentials := c.client.GetCredential(); credentials != nil && credentials.SecretID == c.cred.SecretID && credentials.SecretKey == c.cred.SecretKey {
		token = credentials.SessionToken
	}
	client := sdk.NewClient(&base, &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &sdk.AuthorizationTransport{SecretID: c.cred.SecretID, SecretKey: c.cred.SecretKey, SessionToken: token, Transport: mutationTransport{native}}})
	client.Conf.RetryOpt.Count = 1
	client.Conf.RetryOpt.Interval = 0
	client.Conf.RetryOpt.AutoSwitchHost = false
	return &mutationBackend{client: client, bucket: c.bucket, region: c.region, secretID: c.cred.SecretID, secretKey: c.cred.SecretKey, token: token}, nil
}

type mutationTransport struct{ base http.RoundTripper }

func (t mutationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.GetBody = nil
	return t.base.RoundTrip(copy)
}

func (b *mutationBackend) Head(ctx context.Context, key string) (objectmutation.Object, error) {
	response, err := b.client.Object.Head(ctx, key, nil)
	if err != nil {
		return objectmutation.Object{}, mutationReadError(ctx, key, err, false)
	}
	if response == nil || response.StatusCode != http.StatusOK {
		return objectmutation.Object{}, storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	return mutationObject(key, response.Header)
}
func (b *mutationBackend) Open(ctx context.Context, key, etag string) (io.ReadCloser, error) {
	if etag == "" || strings.ContainsAny(etag, "\r\n\x00") {
		return nil, storage.NewError("unsupported", storage.Path(key), nil)
	}
	headers := make(http.Header)
	headers.Set("If-Match", etag)
	response, err := b.client.Object.Get(ctx, key, &sdk.ObjectGetOptions{XOptionHeader: &headers})
	if err != nil {
		return nil, mutationReadError(ctx, key, err, true)
	}
	if response == nil {
		return nil, storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	object, err := mutationObject(key, response.Header)
	if err != nil || response.StatusCode != 200 || object.ETag != etag || response.Header.Get("Content-Range") != "" {
		response.Body.Close()
		return nil, storage.NewError("changed", storage.Path(key), err)
	}
	return &mutationBody{ReadCloser: response.Body, remaining: object.Size}, nil
}
func (b *mutationBackend) Put(ctx context.Context, in objectmutation.Put) (objectmutation.Object, error) {
	if in.MediaType == "" {
		in.MediaType = "application/octet-stream"
	}
	if err := validateMutationPut(in); err != nil {
		return objectmutation.Object{}, err
	}
	headers := make(http.Header)
	if in.Absent {
		headers.Set("If-None-Match", "*")
		headers.Set("x-cos-forbid-overwrite", "true")
	}
	if in.ForbidOverwrite {
		// Only the provider's own switch, which is all a form upload can rely on.
		headers.Set("x-cos-forbid-overwrite", "true")
	}
	if in.ExpectedETag != "" {
		headers.Set("If-Match", in.ExpectedETag)
	}
	for key, value := range in.Metadata {
		headers.Set("x-cos-meta-"+key, value)
	}
	var body io.Reader = sizedReader{Reader: in.Body, size: in.Size}
	if in.Size == 0 {
		body = strings.NewReader("")
	}
	// The body streams here, so the request lasts as long as the upload takes.
	ctx, cancel := transferBudget(ctx, in.Size)
	defer cancel()
	response, err := b.client.Object.Put(ctx, in.Key, body, &sdk.ObjectPutOptions{ObjectPutHeaderOptions: &sdk.ObjectPutHeaderOptions{ContentLength: in.Size, ContentType: in.MediaType, XOptionHeader: &headers}})
	if err != nil {
		return objectmutation.Object{}, mutationPublishError(in, err)
	}
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return objectmutation.Object{}, storage.NewError("outcome_unknown", storage.Path(in.Key), nil)
	}
	modified := time.Time{}
	if raw := response.Header.Get("Last-Modified"); raw != "" {
		modified, _ = http.ParseTime(raw)
	}
	return objectmutation.Object{Key: in.Key, VersionID: response.Header.Get("x-cos-version-id"), ETag: response.Header.Get("ETag"), Size: in.Size, MediaType: in.MediaType, ModifiedAt: modified, Metadata: cloneMutationMetadata(in.Metadata)}, nil
}
func (b *mutationBackend) Delete(ctx context.Context, key, expectedETag string) (objectmutation.Object, error) {
	if key == "" || strings.ContainsAny(expectedETag, "\r\n\x00") {
		return objectmutation.Object{}, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	headers := make(http.Header)
	if expectedETag != "" {
		headers.Set("If-Match", expectedETag)
	}
	response, err := b.client.Object.Delete(ctx, key, &sdk.ObjectDeleteOptions{XOptionHeader: &headers})
	if err != nil {
		return objectmutation.Object{}, mutationWriteError(key, err)
	}
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return objectmutation.Object{}, storage.NewError("outcome_unknown", storage.Path(key), nil)
	}
	return objectmutation.Object{Key: key, VersionID: response.Header.Get("x-cos-version-id")}, nil
}
func mutationObject(key string, h http.Header) (objectmutation.Object, error) {
	size, err := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		return objectmutation.Object{}, storage.NewError("invalid_backend_response", storage.Path(key), err)
	}
	modified := time.Time{}
	if raw := h.Get("Last-Modified"); raw != "" {
		modified, err = http.ParseTime(raw)
		if err != nil {
			return objectmutation.Object{}, storage.NewError("invalid_backend_response", storage.Path(key), err)
		}
	}
	metadata := make(map[string]string)
	for name, values := range h {
		if strings.HasPrefix(strings.ToLower(name), "x-cos-meta-") {
			if len(values) != 1 {
				return objectmutation.Object{}, storage.NewError("invalid_backend_response", storage.Path(key), nil)
			}
			metadata[strings.TrimPrefix(strings.ToLower(name), "x-cos-meta-")] = values[0]
		}
	}
	return objectmutation.Object{Key: key, Size: size, VersionID: h.Get("x-cos-version-id"), ETag: h.Get("ETag"), ModifiedAt: modified, MediaType: h.Get("Content-Type"), Metadata: metadata}, nil
}
func cloneMutationMetadata(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
func validateMutationPut(in objectmutation.Put) error {
	if in.Key == "" || in.Size < 0 || in.Size > 9007199254740991 || in.Body == nil || in.Absent && in.ExpectedETag != "" || strings.ContainsAny(in.MediaType+in.ExpectedETag, "\r\n\x00") {
		return storage.NewError("invalid_request", storage.Path(in.Key), nil)
	}
	for key, value := range in.Metadata {
		if key == "" || strings.ContainsAny(key, " \t\r\n\x00:") || strings.ContainsAny(value, "\r\n\x00") {
			return storage.NewError("invalid_request", storage.Path(in.Key), nil)
		}
	}
	return nil
}

// mutationPublishError recognizes COS's occupied-target response to the native
// If-None-Match: * create condition. A 304 from another mutation remains unknown.
func mutationPublishError(in objectmutation.Put, err error) error {
	var response *sdk.ErrorResponse
	if in.Absent && errors.As(err, &response) && response.Response != nil && response.Response.StatusCode == http.StatusNotModified {
		return storage.NewError("precondition_failed", storage.Path(in.Key), err)
	}
	return mutationWriteError(in.Key, err)
}

func mutationWriteError(key string, err error) error {
	code := "outcome_unknown"
	var response *sdk.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		switch response.Response.StatusCode {
		case 412:
			code = "precondition_failed"
		case 409:
			// The overwrite switch answers FileAlreadyExists with 409. Only that code is a
			// statement about the condition; any other 409 says nothing about it and stays
			// unresolved, which is why this is not a blanket status mapping.
			if response.Code == "FileAlreadyExists" {
				code = "precondition_failed"
			}
		}
	}
	return storage.NewError(code, storage.Path(key), err)
}
func mutationReadError(ctx context.Context, key string, err error, versioned bool) error {
	if ctx.Err() != nil {
		return storage.NewError("backend_error", storage.Path(key), ctx.Err())
	}
	code := "backend_error"
	var response *sdk.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		switch response.Response.StatusCode {
		case 404:
			code = "not_found"
			if versioned {
				code = "changed"
			}
		case 412:
			code = "changed"
		}
	}
	return storage.NewError(code, storage.Path(key), err)
}

type mutationBody struct {
	io.ReadCloser
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
	b.once.Do(func() { b.closeErr = b.ReadCloser.Close() })
	return b.closeErr
}
