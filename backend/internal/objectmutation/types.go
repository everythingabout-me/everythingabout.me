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
	ConnectionID      string    `json:"connectionId"`
	Endpoint          string    `json:"endpoint"`
	Bucket            string    `json:"bucket"`
	Region            string    `json:"region"`
	CheckedAt         time.Time `json:"checkedAt"`
	ProbeReference    string    `json:"probeReference"`
	AtomicPublish     bool      `json:"atomicPublish"`
	ConditionalCreate bool      `json:"conditionalCreate"`
	SingleGateway     bool      `json:"singleGateway"`
	// MultipartUploads additionally attests native part persistence and conditional
	// create on Complete for this exact provider endpoint. Existing PUT evidence
	// alone never enables numbered native uploads.
	MultipartUploads bool `json:"multipartUploads,omitempty"`
	// SignedMultipartParts attests COS Content-MD5 rejection and no-SSE part ETag=MD5.
	SignedMultipartParts bool `json:"signedMultipartParts,omitempty"`
	// ListingETag additionally attests, for this exact endpoint, that a delimiter
	// listing reports each object's ETag byte-identically to the HEAD the mutation
	// driver would otherwise issue to learn it. Without that evidence the two
	// channels could disagree and every captured entry would look changed; with it a
	// listing answers each child at no extra request, which is the difference between
	// one request per folder and one per entry for every capture, move, copy, delete
	// and reconcile pass. Probed on 18 objects of a real COS bucket, 16 of them
	// multipart and 2 zero-byte markers: no mismatch.
	ListingETag bool `json:"listingETag,omitempty"`
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

// Copy is one provider-side copy request. SourceETag binds the exact source
// revision and Absent refuses to replace an existing target, so the request needs
// no Stat-then-write shim. Metadata carries the intent marker that lets recovery
// attribute its own effect after a lost reply.
type Copy struct {
	Source, Target string
	SourceETag     string
	Size           int64
	MediaType      string
	Metadata       map[string]string
	Absent         bool
	ExpectedETag   string
}

// CopyBackend copies one object inside the provider without streaming its bytes
// through the gateway. It must bind the source revision and refuse an existing
// target through native conditions; it must never fall back to a read/put pair.
type CopyBackend interface {
	CopyObject(context.Context, Copy) (Object, error)
}

// PartCopyBackend copies numbered ranges of one source object provider-side for
// objects above the provider's single-request copy limit. Completion remains one
// conditional request, and AbortMultipart discards the unpublished parts.
type PartCopyBackend interface {
	MultipartBackend
	CopyPart(ctx context.Context, key, uploadID string, number int, source, sourceETag string, offset, length int64) (string, error)
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

// MultipartInspector reports whether the provider still knows an upload session. It is
// an operator-reconciliation capability, not publication authority: a session the
// provider no longer knows can never be completed and can never serve its parts again.
// A backend without it cannot prove a discarded allocation terminal, so retained
// liability stays until the provider lifecycle is inspected out of band.
type MultipartInspector interface {
	InspectMultipart(context.Context, string, string) (MultipartState, error)
}
type MultipartState struct {
	Present bool
	Parts   int // parts the provider still reports when Present
}

// SignedPartBackend is a class A part-signing capability. It signs exact
// lengths and never a content digest; ListParts reports what the provider holds.
type SignedPartBackend interface {
	SignedPartsSupported() bool
	SignPart(context.Context, string, string, int, int64, time.Time) (storage.SignedPartGrant, error)
	ListParts(context.Context, string, string, int, int) ([]storage.NativeUploadPart, bool, error)
}

// ReadSignerBackend issues direct read grants with a dedicated GetObject-only
// identity. A driver without it cannot serve class A reads.
type ReadSignerBackend interface {
	ReadSigningSupported() bool
	PresignRead(context.Context, string, storage.ReadGrantOptions) (storage.ReadGrant, error)
}

type NativePart struct {
	Number int
	ETag   string
}

type Backend interface {
	Head(context.Context, string) (Object, error)
	Open(context.Context, string, string) (io.ReadCloser, error) // key, required current ETag (If-Match)
	Put(context.Context, Put) (Object, error)
	Delete(context.Context, string, string) (Object, error) // key, expected ETag; unversioned current-key deletion
}
