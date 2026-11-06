// Package cos is the Tencent Cloud COS implementation of storage.Storage.
package cos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tencentyun/cos-go-sdk-v5"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/tencent"
)

type Client struct {
	bucket        string
	region        string
	appID         string
	client        *cos.Client
	publishClient *cos.Client
	signer        *cos.Client
	cred          tencent.Credentials
	transport     *http.Transport
}

func (c *Client) Close() error {
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	return nil
}

func New(cfg config.COS) (*Client, error) {
	bucketURL, err := url.Parse(fmt.Sprintf("https://%s.cos.%s.myqcloud.com", cfg.Bucket, cfg.Region))
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 128, MaxIdleConnsPerHost: 32, MaxConnsPerHost: 64,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, ExpectContinueTimeout: time.Second,
	}
	httpClient := &http.Client{
		Timeout:   60 * time.Second,
		Transport: &cos.AuthorizationTransport{SecretID: cfg.SecretID, SecretKey: cfg.SecretKey, Transport: transport},
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: bucketURL}, httpClient)
	publishClient := cos.NewClient(&cos.BaseURL{BucketURL: bucketURL}, httpClient)
	publishClient.Conf.RetryOpt.Count = 1
	var signer *cos.Client
	if cfg.SignSecretID != "" || cfg.SignSecretKey != "" {
		if cfg.SignSecretID == "" || cfg.SignSecretKey == "" || cfg.SignSecretID == cfg.SecretID {
			return nil, fmt.Errorf("COS signing requires one distinct complete credential pair limited to UploadPart and GetObject")
		}
		signer = cos.NewClient(&cos.BaseURL{BucketURL: bucketURL}, &http.Client{Timeout: 60 * time.Second, Transport: &cos.AuthorizationTransport{SecretID: cfg.SignSecretID, SecretKey: cfg.SignSecretKey, Transport: transport}})
	}
	return &Client{
		bucket:        cfg.Bucket,
		region:        cfg.Region,
		appID:         config.COSAppID(cfg.Bucket),
		client:        client,
		publishClient: publishClient,
		signer:        signer,
		transport:     transport,
		cred:          tencent.Credentials{SecretID: cfg.SecretID, SecretKey: cfg.SecretKey},
	}, nil
}

func (c *Client) Head(ctx context.Context, key string) (*storage.ObjectInfo, error) {
	resp, err := c.client.Object.Head(ctx, key, nil)
	if err != nil {
		if cos.IsNotFoundError(err) {
			return nil, nil
		}
		return nil, err
	}
	info := &storage.ObjectInfo{
		Key:          key,
		ETag:         strings.Trim(resp.Header.Get("ETag"), `"`),
		StorageClass: resp.Header.Get("x-cos-storage-class"),
		ContentType:  resp.Header.Get("Content-Type"),
	}
	info.Size, _ = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if modified := resp.Header.Get("Last-Modified"); modified != "" {
		if parsed, err := http.ParseTime(modified); err == nil {
			info.LastModified = parsed
		}
	}
	return info, nil
}

func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := c.client.Object.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Client) Copy(ctx context.Context, sourceKey, targetKey string) error {
	source := fmt.Sprintf("%s.cos.%s.myqcloud.com/%s", c.bucket, c.region, sourceKey)
	_, _, err := c.client.Object.Copy(ctx, targetKey, source, nil)
	return err
}

func (c *Client) Remove(ctx context.Context, key string) error {
	// A successful retry cannot prove an earlier timed-out delete is quiescent.
	// The journal owns recovery and retains the path after an uncertain reply.
	_, err := c.publishClient.Object.Delete(ctx, key)
	return err
}

func (c *Client) PutEmpty(ctx context.Context, key string) error {
	_, err := c.client.Object.Put(ctx, key, bytes.NewReader(nil), nil)
	return err
}

// sizedReader lets the SDK read the length (for progress and the 5GB check) while also
// passing Content-Length to COS explicitly, so the upload does not degrade into chunked.
type sizedReader struct {
	io.Reader
	size int64
}

func (r sizedReader) Size() int64 { return r.size }

func (c *Client) Put(
	ctx context.Context,
	key string,
	r io.Reader,
	size int64,
	contentType string,
) (*storage.ObjectInfo, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	opt := &cos.ObjectPutOptions{
		ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{
			ContentLength: size,
			ContentType:   contentType,
		},
	}
	resp, err := c.client.Object.Put(ctx, key, sizedReader{Reader: r, size: size}, opt)
	if err != nil {
		return nil, err
	}
	return &storage.ObjectInfo{
		Key:          key,
		Size:         size,
		ETag:         strings.Trim(resp.Header.Get("ETag"), `"`),
		StorageClass: resp.Header.Get("x-cos-storage-class"),
	}, nil
}

func (c *Client) List(ctx context.Context, prefix string, onPage func(storage.ObjectPage) (bool, error)) error {
	marker := ""
	for {
		result, _, err := c.client.Bucket.Get(ctx, &cos.BucketGetOptions{
			Prefix:  prefix,
			Marker:  marker,
			MaxKeys: 1000,
		})
		if err != nil {
			return err
		}

		page := storage.ObjectPage{NextMarker: result.NextMarker, Objects: make([]storage.ObjectInfo, 0, len(result.Contents))}
		for _, object := range result.Contents {
			info := storage.ObjectInfo{
				Key:          object.Key,
				Size:         object.Size,
				ETag:         strings.Trim(object.ETag, `"`),
				StorageClass: object.StorageClass,
			}
			if object.LastModified != "" {
				if parsed, err := time.Parse(time.RFC3339, object.LastModified); err == nil {
					info.LastModified = parsed
				}
			}
			page.Objects = append(page.Objects, info)
		}

		if len(result.Contents) > 0 {
			proceed, err := onPage(page)
			if err != nil {
				return err
			}
			if !proceed {
				return nil
			}
		}
		if !result.IsTruncated || len(result.Contents) == 0 {
			return nil
		}

		next := result.NextMarker
		if next == "" {
			next = result.Contents[len(result.Contents)-1].Key
		}
		if next == marker {
			// Defensive: never loop forever if the marker fails to advance
			return errors.New("COS listing did not advance; bucket configuration looks wrong")
		}
		marker = next
	}
}

func (c *Client) GetURL(ctx context.Context, key string, opts storage.URLOptions) (string, error) {
	expires := opts.ExpiresSeconds
	if expires <= 0 {
		expires = 900
	}

	query := url.Values{}
	if opts.DownloadName != "" {
		query.Set("response-content-disposition",
			"inline; filename*=UTF-8''"+url.PathEscape(opts.DownloadName))
	}
	if opts.ContentType != "" {
		query.Set("response-content-type", opts.ContentType)
	}
	// Data processing parameters must be signed along with the rest of the query, or COS
	// considers the signature mismatched. For example Process = "imageMogr2/thumbnail/640x>".
	if opts.Process != "" {
		query.Set(opts.Process, "")
	}

	var options *cos.PresignedURLOptions
	if len(query) > 0 {
		options = &cos.PresignedURLOptions{Query: &query}
	}
	signed, err := c.client.Object.GetPresignedURL3(
		ctx, http.MethodGet, key, time.Duration(expires)*time.Second, options,
	)
	if err != nil {
		return "", err
	}
	return signed.String(), nil
}

// PutURL builds a presigned PUT URL for uploads. Only Host is signed, not Content-Type, so
// the client can send any Content-Type without the signature falling over.
func (c *Client) PutURL(ctx context.Context, key string, expiresSeconds int) (string, error) {
	if expiresSeconds <= 0 {
		expiresSeconds = 3600
	}
	signed, err := c.client.Object.GetPresignedURL(
		ctx,
		http.MethodPut,
		key,
		c.cred.SecretID,
		c.cred.SecretKey,
		time.Duration(expiresSeconds)*time.Second,
		nil,
	)
	if err != nil {
		return "", err
	}
	return signed.String(), nil
}

var uploadActions = []string{
	"name/cos:PutObject",
	"name/cos:PostObject",
	"name/cos:InitiateMultipartUpload",
	"name/cos:UploadPart",
	"name/cos:CompleteMultipartUpload",
	"name/cos:AbortMultipartUpload",
	"name/cos:ListMultipartUploads",
	"name/cos:ListParts",
}

// stsResource assembles the resource path for an STS policy. key must be percent-encoded
// per URL segment: STS decodes the resource path in the policy before comparing it against
// the request, and that decoder treats + as a space, so a filename containing + (for
// example "选手甲+选手乙vsPlayerC+PlayerD.MOV") means the policy grants a different object than the
// real key and the direct upload is rejected by COS with 403 AccessDenied.
func stsResource(region, appID, bucket, key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = escapeKeySegment(segment)
	}
	return fmt.Sprintf("qcs::cos:%s:uid/%s:%s/%s",
		region, appID, bucket, strings.Join(segments, "/"))
}

// escapeKeySegment keeps only RFC 3986 unreserved characters and percent-encodes every other
// byte (CJK UTF-8 bytes, and + % # ? and friends) as %XX.
func escapeKeySegment(segment string) string {
	var b strings.Builder
	b.Grow(len(segment))
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

type stsCredentialResponse struct {
	Credentials struct {
		TmpSecretID  string `json:"TmpSecretId"`
		TmpSecretKey string `json:"TmpSecretKey"`
		Token        string `json:"Token"`
	} `json:"Credentials"`
	ExpiredTime int64 `json:"ExpiredTime"`
	StartTime   int64 `json:"StartTime"`
}

func (c *Client) TemporaryCredentials(
	ctx context.Context,
	key string,
	expiresSeconds int,
) (*storage.Credentials, error) {
	if expiresSeconds <= 0 || expiresSeconds > 7200 {
		expiresSeconds = 3600
	}

	resource := stsResource(c.region, c.appID, c.bucket, key)
	policy := map[string]any{
		"version": "2.0",
		"statement": []any{map[string]any{
			"effect":   "allow",
			"action":   uploadActions,
			"resource": []string{resource},
		}},
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}

	var response stsCredentialResponse
	err = tencent.Call(ctx, c.cred, "sts", "2018-08-13", c.region, "GetFederationToken", map[string]any{
		"Name":            "eam-cos-sts",
		"Policy":          string(policyJSON),
		"DurationSeconds": expiresSeconds,
	}, &response)
	if err != nil {
		return nil, err
	}

	return &storage.Credentials{
		TmpSecretID:  response.Credentials.TmpSecretID,
		TmpSecretKey: response.Credentials.TmpSecretKey,
		SessionToken: response.Credentials.Token,
		StartTime:    response.StartTime,
		ExpiredTime:  response.ExpiredTime,
	}, nil
}

func (c *Client) InitiateMultipart(ctx context.Context, key string) (string, error) {
	result, _, err := c.client.Object.InitiateMultipartUpload(ctx, key, nil)
	if err != nil {
		return "", err
	}
	return result.UploadID, nil
}

// MultipartPartURL signs partNumber and uploadId as part of the query string.
func (c *Client) MultipartPartURL(
	ctx context.Context,
	key, uploadID string,
	partNumber, expiresSeconds int,
) (string, error) {
	if expiresSeconds <= 0 {
		expiresSeconds = 3600
	}
	query := url.Values{}
	query.Set("partNumber", strconv.Itoa(partNumber))
	query.Set("uploadId", uploadID)

	signed, err := c.client.Object.GetPresignedURL(
		ctx,
		http.MethodPut,
		key,
		c.cred.SecretID,
		c.cred.SecretKey,
		time.Duration(expiresSeconds)*time.Second,
		&cos.PresignedURLOptions{Query: &query},
	)
	if err != nil {
		return "", err
	}
	return signed.String(), nil
}

func (c *Client) CompleteMultipart(
	ctx context.Context,
	key, uploadID string,
	parts []storage.MultipartPart,
) (*storage.ObjectInfo, error) {
	opt := &cos.CompleteMultipartUploadOptions{
		Parts: make([]cos.Object, 0, len(parts)),
	}
	for _, part := range parts {
		opt.Parts = append(opt.Parts, cos.Object{
			PartNumber: part.PartNumber,
			ETag:       part.ETag,
		})
	}
	if _, _, err := c.client.Object.CompleteMultipartUpload(ctx, key, uploadID, opt); err != nil {
		return nil, err
	}
	// The Complete response carries no size, so HEAD once for the final size and ETag.
	return c.Head(ctx, key)
}

func (c *Client) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := c.client.Object.AbortMultipartUpload(ctx, key, uploadID)
	return err
}

// ListParts lists the parts already uploaded. When the upload session is gone (aborted or
// reaped after expiry) it returns an empty list, so the API can suggest starting over
// instead of returning a 500.
func (c *Client) ListParts(ctx context.Context, key, uploadID string) ([]storage.PartInfo, error) {
	var (
		parts  []storage.PartInfo
		marker string
	)
	for {
		opt := &cos.ObjectListPartsOptions{MaxParts: "1000"}
		if marker != "" {
			opt.PartNumberMarker = marker
		}
		result, _, err := c.client.Object.ListParts(ctx, key, uploadID, opt)
		if err != nil {
			if cos.IsNotFoundError(err) {
				return nil, nil
			}
			return nil, err
		}
		for _, part := range result.Parts {
			parts = append(parts, storage.PartInfo{
				PartNumber: part.PartNumber,
				Size:       part.Size,
				ETag:       strings.Trim(part.ETag, `"`),
			})
		}
		if !result.IsTruncated || result.NextPartNumberMarker == "" {
			return parts, nil
		}
		marker = result.NextPartNumberMarker
	}
}
