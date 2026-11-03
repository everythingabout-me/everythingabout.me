// Package objectmutation coordinates admitted object-store writes using private
// durable gateway staging and one-shot native requests. It never interprets a
// timeout or object existence as proof that an old remote effect is terminal.
package objectmutation

import (
	"context"
	"everythingabout.me/backend/internal/storage"
	"io"
	"time"
)

// Admission is deployment evidence, not an assertion of S3 compatibility. The
// file must describe positive/negative isolated probes for this exact connection.
// Capability assertions apply only to the selected publication strategy.
type Admission struct {
	// CreateOnly admits native no-overwrite publication on an unversioned bucket.
	// It grants no replace/delete authority or exclusion of external writers.
	CreateOnly                 bool      `json:"createOnly,omitempty"`
	ConnectionID               string    `json:"connectionId"`
	Endpoint                   string    `json:"endpoint"`
	Bucket                     string    `json:"bucket"`
	Region                     string    `json:"region"`
	CheckedAt                  time.Time `json:"checkedAt"`
	ProbeReference             string    `json:"probeReference"`
	AtomicPublish              bool      `json:"atomicPublish"`
	VersionedReads             bool      `json:"versionedReads"`
	ConditionalCreate          bool      `json:"conditionalCreate"`
	CurrentKeyDelete           bool      `json:"currentKeyDelete"`
	CoordinatedExclusiveWriter bool      `json:"coordinatedExclusiveWriter"`
	SingleGateway              bool      `json:"singleGateway"`
	LegacyWritersDrained       bool      `json:"legacyWritersDrained"`
	// MultipartUploads additionally attests native part persistence and conditional
	// create on Complete for this exact provider endpoint. Existing PUT evidence
	// alone never enables numbered native uploads.
	MultipartUploads bool `json:"multipartUploads,omitempty"`
	// SignedMultipartParts attests COS Content-MD5 rejection and no-SSE part ETag=MD5.
	SignedMultipartParts bool `json:"signedMultipartParts,omitempty"`
}
type Options struct {
	Admission       Admission
	SpoolRoot       string
	MaxPublishBytes int64
}

type Object struct {
	Key        string
	VersionID  string
	ETag       string
	Size       int64
	ModifiedAt time.Time
	MediaType  string
	Metadata   map[string]string
}
type Put struct {
	Key          string
	Body         io.Reader
	Size         int64
	MediaType    string
	Metadata     map[string]string
	Absent       bool
	ExpectedETag string // optional real native condition; never Stat-then-write fallback
}

// Backend executes exactly ONE SDK attempt per mutating request, with redirects
// and replay disabled. A successful result means the sole native request has a
// complete terminal response; ambiguous errors never get an automatic replay.
// MultipartBackend keeps numbered payloads unpublished until one conditional
// Complete request. Part retries may replay identical bytes only; completion and
// allocation never use implicit SDK retries.
type MultipartBackend interface {
	BeginMultipart(context.Context, Put) (string, error)
	PutPart(context.Context, string, string, int, io.Reader, int64) (string, error)
	CompleteMultipart(context.Context, Put, string, []NativePart) (Object, error)
	AbortMultipart(context.Context, string, string) error
}

// SignedMultipartBackend is intentionally COS-specific capability, not a generic
// S3 compatibility claim. SignPart binds exact content MD5/length and native slot.
type SignedMultipartBackend interface {
	SignedPartsSupported() bool
	SignPart(context.Context, string, string, int, int64, string, time.Time) (storage.SignedPartGrant, error)
	QueryPart(context.Context, string, string, int) (NativePartInfo, bool, error)
}
type NativePartInfo struct {
	Size int64
	ETag string
}
type NativePart struct {
	Number int
	ETag   string
}

type Backend interface {
	Head(context.Context, string) (Object, error)
	Open(context.Context, string, string) (io.ReadCloser, error) // exact retained native version
	Put(context.Context, Put) (Object, error)
	Delete(context.Context, string, string) (Object, error) // key, expected ETag; unversioned current-key deletion
}
