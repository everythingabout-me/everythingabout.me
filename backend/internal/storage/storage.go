// Package storage 定义对象存储的接口。业务代码只依赖它，
// 具体实现（目前是腾讯云 COS）放在 internal/cos。
package storage

import (
	"context"
	"time"
)

type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	StorageClass string
}

type Page struct {
	Objects    []ObjectInfo
	NextMarker string
}

type URLOptions struct {
	// ExpiresSeconds 默认 900 秒。
	ExpiresSeconds int
	// DownloadName 填了会通过 response-content-disposition 指定文件名。
	DownloadName string
	ContentType  string
}

// CredentialsJSON 直接返回给浏览器，字段名和 COS JS SDK 期望的一致。
type Credentials struct {
	TmpSecretID  string `json:"tmpSecretId"`
	TmpSecretKey string `json:"tmpSecretKey"`
	SessionToken string `json:"sessionToken"`
	StartTime    int64  `json:"startTime"`
	ExpiredTime  int64  `json:"expiredTime"`
}

type Storage interface {
	Head(ctx context.Context, key string) (*ObjectInfo, error)
	// Copy 是服务端复制，不经过本地带宽。
	Copy(ctx context.Context, sourceKey, targetKey string) error
	Remove(ctx context.Context, key string) error
	// PutEmpty 写一个零字节对象，用于目录占位。
	PutEmpty(ctx context.Context, key string) error
	// List 分页列举，回调返回 false 可提前结束。
	List(ctx context.Context, prefix string, onPage func(Page) (bool, error)) error
	// GetURL 生成下载用的预签名 URL，默认 inline。
	GetURL(ctx context.Context, key string, opts URLOptions) (string, error)
	// TemporaryCredentials 生成前端直传所需的临时凭证。
	TemporaryCredentials(ctx context.Context, key string, expiresSeconds int) (*Credentials, error)
}
