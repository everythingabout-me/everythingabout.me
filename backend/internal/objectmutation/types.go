// Package objectmutation coordinates admitted object-store writes using private
// durable gateway staging and one-shot native requests. It never interprets a
// timeout or object existence as proof that an old remote effect is terminal.
package objectmutation

import (
	"context"
	"io"
	"time"
)

// Admission is deployment evidence, not an assertion of S3 compatibility. The
// file must describe positive/negative isolated probes for this exact connection.
// All flags are required for the coordinated single-gateway strategy in P4.
type Admission struct {
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
type Backend interface {
	Head(context.Context, string) (Object, error)
	Open(context.Context, string, string) (io.ReadCloser, error) // exact retained native version
	Put(context.Context, Put) (Object, error)
	Delete(context.Context, string, string) (Object, error) // key, expected ETag; unversioned current-key deletion
}
