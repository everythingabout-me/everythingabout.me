// Package cos 是 storage.Storage 的腾讯云 COS 实现。
package cos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	bucket string
	region string
	appID  string
	client *cos.Client
	cred   tencent.Credentials
}

func New(cfg config.COS) (*Client, error) {
	bucketURL, err := url.Parse(fmt.Sprintf("https://%s.cos.%s.myqcloud.com", cfg.Bucket, cfg.Region))
	if err != nil {
		return nil, err
	}
	client := cos.NewClient(
		&cos.BaseURL{BucketURL: bucketURL},
		&http.Client{
			Timeout: 60 * time.Second,
			Transport: &cos.AuthorizationTransport{
				SecretID:  cfg.SecretID,
				SecretKey: cfg.SecretKey,
			},
		},
	)
	return &Client{
		bucket: cfg.Bucket,
		region: cfg.Region,
		appID:  config.COSAppID(cfg.Bucket),
		client: client,
		cred:   tencent.Credentials{SecretID: cfg.SecretID, SecretKey: cfg.SecretKey},
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
	_, err := c.client.Object.Delete(ctx, key)
	return err
}

func (c *Client) PutEmpty(ctx context.Context, key string) error {
	_, err := c.client.Object.Put(ctx, key, bytes.NewReader(nil), nil)
	return err
}

// sizedReader 让 SDK 能读出长度（progress + 5GB 检查），同时把
// Content-Length 显式传给 COS，避免退化成 chunked 上传。
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

func (c *Client) List(ctx context.Context, prefix string, onPage func(storage.Page) (bool, error)) error {
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

		page := storage.Page{NextMarker: result.NextMarker, Objects: make([]storage.ObjectInfo, 0, len(result.Contents))}
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
			// 防御性判断：marker 没前进就不要无限循环
			return errors.New("COS 列举没有前进，可能桶配置异常")
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
	// 数据万象的处理参数必须和签名一起带上，否则 COS 会认为签名不匹配。
	// 例如 Process = "imageMogr2/thumbnail/640x>"。
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

// PutURL 生成上传用的预签名 PUT URL。只签 Host，不签 Content-Type，
// 这样客户端可以自由发 Content-Type，不会因为多带一个头就签名不匹配。
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

	resource := fmt.Sprintf("qcs::cos:%s:uid/%s:%s/%s", c.region, c.appID, c.bucket, key)
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

// MultipartPartURL 把 partNumber 和 uploadId 放进 query 一起签名。
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
	// Complete 的响应不带大小，HEAD 一次拿最终大小和 ETag。
	return c.Head(ctx, key)
}

func (c *Client) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := c.client.Object.AbortMultipartUpload(ctx, key, uploadID)
	return err
}

// ListParts 列出已经传上来的分片。上传会话不存在（被 abort 或过期回收）
// 时返回空列表，让接口能给出"重新发起"的提示而不是 500。
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
