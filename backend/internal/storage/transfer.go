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
	Number  int
	Size    int64
	Method  string
	URL     string
	Headers map[string]string
	// Form makes this grant a form upload rather than a request: the client POSTs
	// multipart/form-data to URL, writing these fields in this order and then the file under
	// FileField. The provider's form signature requires the signed fields to precede the
	// file, so the order is part of the grant. Empty means a plain request with Headers.
	Form      []FormField
	FileField string
	ExpiresAt time.Time
}

// FormField is one multipart/form-data field of a form upload.
type FormField struct {
	Name  string
	Value string
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

// DirectPutWriter is the class A writer for a destination whose provider can create or
// replace the object with one conditional PUT at that destination. The client sends the
// single PUT itself; nothing is staged, so the session's commit verifies the object the
// client's PUT created instead of publishing one.
//
// It is separate from PlannedUploadWriter because the two differ in who publishes: a
// planned upload stages parts and the service completes them, while a direct put has
// already happened before the commit runs. A driver that cannot sign a conditional PUT at
// the destination simply does not implement this interface, and the service keeps the
// multipart path for it.
type DirectPutWriter interface {
	// SupportsDirectPut reports whether this driver's provider can honour the condition on
	// a single PUT. The interface is implemented by every object driver, but only an
	// admitted conditional-write backend answers true.
	SupportsDirectPut() bool
	// BeginDirectPut signs one exact-length conditional PUT at the destination and returns
	// it as a one-part grant. No provider request is made here.
	BeginDirectPut(context.Context, WriteIntent, UploadMetadata, time.Time) (SignedPartGrant, error)
}

// DirectPostWriter is the class A writer for a destination whose conditional create is a
// form upload instead of a conditional PUT. A form request carries no custom header, so it
// is a CORS-simple request: the browser sends no preflight, which halves the round trips a
// small file costs. The condition still exists — it travels inside the signed policy — so a
// driver that cannot sign one simply does not implement this interface.
type DirectPostWriter interface {
	// SupportsDirectPost reports whether the provider's overwrite switch refuses a form
	// upload on an occupied key. Only an admitted connection answers true.
	SupportsDirectPost() bool
	// BeginDirectPost returns the endpoint, the signed fields and the file field name for
	// one create. No provider request is made here.
	BeginDirectPost(context.Context, WriteIntent, UploadMetadata, time.Time) (SignedPartGrant, error)
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
