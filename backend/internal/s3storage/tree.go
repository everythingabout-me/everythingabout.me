// Package s3storage provides a read-only native tree through the official AWS
// SDK. Credentials are explicit and never loaded from the process environment.
package s3storage

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"everythingabout.me/backend/internal/objecttree"
	"everythingabout.me/backend/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Options configures one connection shared by its owner/root views. Endpoint
// must explicitly include http(s) and no path, credentials, query or fragment.
type Options struct {
	Endpoint        string
	Region          string
	Bucket          string
	PathStyle       bool
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}
type Factory struct {
	backend *backend
	secret  []byte
}

var sharedTransport = &http.Transport{
	Proxy:             http.ProxyFromEnvironment,
	DialContext:       (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2: true, MaxIdleConns: 128, MaxIdleConnsPerHost: 32, MaxConnsPerHost: 64,
	IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, ExpectContinueTimeout: time.Second,
}

func NewFactory(options Options) (*Factory, error) {
	endpoint, err := url.Parse(options.Endpoint)
	if err != nil || endpoint == nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") || options.Region == "" || options.Bucket == "" || options.AccessKeyID == "" || options.SecretAccessKey == "" {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	for _, value := range []string{options.Region, options.Bucket, options.AccessKeyID, options.SecretAccessKey, options.SessionToken} {
		if strings.ContainsFunc(value, unicode.IsControl) {
			return nil, storage.NewError("invalid_config", "", nil)
		}
	}
	if strings.ContainsAny(options.Bucket, "/\\ ") || strings.ContainsAny(options.Region, "/\\ ") {
		return nil, storage.NewError("invalid_config", "", nil)
	}
	secret, err := objecttree.NewSecret()
	if err != nil {
		return nil, err
	}
	client := s3.New(s3.Options{
		Region: options.Region, BaseEndpoint: aws.String(options.Endpoint), UsePathStyle: options.PathStyle,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(options.AccessKeyID, options.SecretAccessKey, options.SessionToken)),
		HTTPClient:  recordingClient{&http.Client{Transport: sharedTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, RetryMaxAttempts: 2,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &Factory{backend: &backend{client: client, bucket: options.Bucket}, secret: secret}, nil
}
func (f *Factory) Open(ctx context.Context, cfg storage.Config) (storage.Driver, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return objecttree.Bind(f.backend, cfg, f.secret)
}

type backend struct {
	client *s3.Client
	bucket string
}

func (b *backend) Head(ctx context.Context, key string) (objecttree.Object, error) {
	output, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if err != nil {
		return objecttree.Object{}, nativeError(ctx, err, false)
	}
	if output.ContentLength == nil {
		return objecttree.Object{}, storage.NewError("invalid_backend_response", "", nil)
	}
	return objecttree.Object{Key: key, Size: *output.ContentLength, ModifiedAt: aws.ToTime(output.LastModified), MediaType: aws.ToString(output.ContentType), VersionID: aws.ToString(output.VersionId)}, nil
}
func (b *backend) List(ctx context.Context, prefix, cursor string) (objecttree.NativePage, error) {
	input := &s3.ListObjectsV2Input{Bucket: aws.String(b.bucket), Prefix: aws.String(prefix), Delimiter: aws.String("/"), MaxKeys: aws.Int32(1000)}
	if cursor != "" {
		input.ContinuationToken = aws.String(cursor)
	}
	output, err := b.client.ListObjectsV2(ctx, input)
	if err != nil {
		return objecttree.NativePage{}, nativeError(ctx, err, false)
	}
	if output.IsTruncated == nil || output.EncodingType != "" {
		return objecttree.NativePage{}, storage.NewError("invalid_backend_response", "", nil)
	}
	page := objecttree.NativePage{Truncated: *output.IsTruncated}
	if page.Truncated {
		page.Next = aws.ToString(output.NextContinuationToken)
	}
	for _, obj := range output.Contents {
		if obj.Key == nil || obj.Size == nil {
			return objecttree.NativePage{}, storage.NewError("invalid_backend_response", "", nil)
		}
		page.Objects = append(page.Objects, objecttree.Object{Key: *obj.Key, Size: *obj.Size, ModifiedAt: aws.ToTime(obj.LastModified)})
	}
	for _, prefix := range output.CommonPrefixes {
		if prefix.Prefix == nil {
			return objecttree.NativePage{}, storage.NewError("invalid_backend_response", "", nil)
		}
		page.Prefixes = append(page.Prefixes, *prefix.Prefix)
	}
	return page, nil
}

// The response recorder retains the actual status through SDK deserialization;
// checking ContentRange alone would accept a server that ignored Range with 200.
type statusKey struct{}
type recordingClient struct{ client *http.Client }

func (c recordingClient) Do(request *http.Request) (*http.Response, error) {
	response, err := c.client.Do(request)
	if response != nil {
		if status, ok := request.Context().Value(statusKey{}).(*int); ok {
			*status = response.StatusCode
		}
	}
	return response, err
}
func (b *backend) Get(ctx context.Context, key, byteRange, version string) (objecttree.Response, error) {
	status := 0
	ctx = context.WithValue(ctx, statusKey{}, &status)
	input := &s3.GetObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)}
	if byteRange != "" {
		input.Range = aws.String(byteRange)
	}
	if version != "" {
		input.VersionId = aws.String(version)
	}
	output, err := b.client.GetObject(ctx, input)
	if err != nil {
		return objecttree.Response{}, nativeError(ctx, err, version != "")
	}
	if output.ContentLength == nil {
		output.Body.Close()
		return objecttree.Response{}, storage.NewError("invalid_backend_response", "", nil)
	}
	obj := objecttree.Object{Key: key, Size: *output.ContentLength, ModifiedAt: aws.ToTime(output.LastModified), MediaType: aws.ToString(output.ContentType), VersionID: aws.ToString(output.VersionId)}
	return objecttree.Response{Object: obj, Body: output.Body, Status: status, Length: *output.ContentLength, ContentRange: aws.ToString(output.ContentRange)}, nil
}
func nativeError(ctx context.Context, err error, versioned bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var malformed *smithy.DeserializationError
	if errors.As(err, &malformed) {
		return storage.NewError("invalid_backend_response", "", err)
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		code := "backend_error"
		switch response.HTTPStatusCode() {
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
		return storage.NewProviderError(code, "", response.HTTPStatusCode(), providerCode(err), providerRequestID(err), err)
	}
	return storage.NewError("backend_error", "", err)
}

// providerCode and providerRequestID name what the provider said and how to look it
// up, without copying its message body (which can echo keys and is not ours to relay).
func providerCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

func providerRequestID(err error) string {
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.Response != nil {
		if id := response.Response.Header.Get("x-amz-request-id"); id != "" {
			return id
		}
		return response.Response.Header.Get("x-amz-id-2")
	}
	return ""
}

var _ storage.Factory = (*Factory)(nil)
