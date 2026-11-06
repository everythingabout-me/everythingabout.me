package storage

import (
	"context"
	"time"
)

// Transfer classes are fixed by the storage connection. A driver advertises the
// classes it can serve in Descriptor.TransferClasses, and the connection selects
// exactly one at configuration time. A transfer never changes class and never
// falls back to another class.
const (
	TransferPresignedParts   = "presigned-parts"   // class A
	TransferResumableSession = "resumable-session" // class B
	TransferRelay            = "relay"             // class C
)

// SignedPartGrant is one class A part URL. URL is the provider URL; the API may
// rewrite it to the optional same-origin edge relay. Headers are the exact
// client-set headers bound by the signature (Content-Type and part length); no
// content digest is signed.
type SignedPartGrant struct {
	Number    int
	Size      int64
	Method    string
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

// PlannedUpload is the class A plan persisted at session open. Handle is the
// private native multipart/session identity and never appears in a client
// payload. Parts is the first signed window.
type PlannedUpload struct {
	Handle    []byte
	PartSize  int64
	Parts     []SignedPartGrant
	ExpiresAt time.Time
}

// NativeUploadPart is one part the provider actually holds. ETag is provider
// evidence for multipart completion, not a content digest and not a service ID.
type NativeUploadPart struct {
	Number int
	Size   int64
	ETag   string
}

// PlannedUploadWriter is the class A writer. The driver never receives bytes:
// it opens unpublished provider state, signs exact-length part URLs, reports
// what the provider holds, and seals the provider listing for publication. The
// application still performs the single publishing call and index settlement.
type PlannedUploadWriter interface {
	// BeginPlannedUpload creates or resumes the native multipart allocation,
	// persists its handle under the intent fence, and returns the first window.
	BeginPlannedUpload(context.Context, WriteIntent, UploadMetadata, time.Time) (PlannedUpload, error)
	// SignUploadParts signs or renews exact-length part URLs for one session.
	SignUploadParts(context.Context, WriteIntent, []int, UploadMetadata, time.Time) ([]SignedPartGrant, error)
	// ListUploadParts reports the provider's own part listing. The bool reports
	// whether more parts exist after the returned page.
	ListUploadParts(context.Context, WriteIntent, int, int, UploadMetadata) ([]NativeUploadPart, bool, error)
	// SealPlannedUpload verifies provider coverage and returns durable staged
	// evidence without receiving any bytes. Publication remains the service's.
	SealPlannedUpload(context.Context, WriteIntent, UploadMetadata, Checkpoint) (Staged, error)
	// DiscardPlannedUpload aborts unpublished provider state only.
	DiscardPlannedUpload(context.Context, WriteIntent, UploadMetadata) error
}

// ReadGrantOptions selects one provider representation. Disposition is
// "attachment" or the safe inline allowlist.
type ReadGrantOptions struct {
	MediaType   string
	Disposition string
	ExpiresAt   time.Time
}

// ReadGrant is a direct read authority for exactly one file. Kind is "url" or
// "bearer"; Headers are sent with a bearer request. Conditions lists only what
// the route enforces natively.
type ReadGrant struct {
	Kind       string
	URL        string
	Headers    map[string]string
	ExpiresAt  time.Time
	Conditions []string
}

// ReadGranter issues class A/B direct read grants. A class C connection serves
// bytes through the ordinary content endpoint instead.
type ReadGranter interface {
	GrantRead(context.Context, Path, ReadGrantOptions) (ReadGrant, error)
}
