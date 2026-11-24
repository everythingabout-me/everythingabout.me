// Package objectmutation coordinates admitted object-store writes using private
// durable gateway staging and one-shot native requests. It never interprets a
// timeout or object existence as proof that an old remote effect is terminal.
package objectmutation

import (
	"context"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storageproduct"
	"io"
	"time"
)

// Capabilities are what the selected product promises about writes. They are one
// definition, in storageproduct: a property of the product rather than a measurement of
// one bucket, which is why this package neither probes nor stores them.
//
// A capability that is false does not fail the connection — it removes one publication
// strategy. What it must never do is leave a strategy enabled that the product does not
// honour, because the failure that produces is a silent overwrite rather than an error.
type Capabilities = storageproduct.Capabilities

type Options struct {
	Capabilities    Capabilities
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
	Key       string
	Body      io.Reader
	Size      int64
	MediaType string
	Metadata  map[string]string
	Absent    bool
	// ForbidOverwrite asks only the provider's own refuse-to-replace switch. A form upload
	// cannot send If-None-Match, so the connection's form-post evidence has to measure that
	// switch by itself instead of inferring it from a conditional PUT.
	ForbidOverwrite bool
	ExpectedETag    string // optional real native condition; never Stat-then-write fallback
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
//
// AbortMultipart destroys one upload session. A nil result proves the session existed
// and is now gone, so none of its parts can ever be completed and no publication from
// that allocation can still land. An answer that the session does not exist is NOT
// that proof: providers remove a session when Complete succeeds as well, so the call
// reports abort_session_absent instead. The caller holds this operation's intent
// marker, and only a destination observation bound to that marker may conclude that a
// publication landed.
type MultipartBackend interface {
	BeginMultipart(context.Context, Put) (string, error)
	PutPart(context.Context, string, string, int, io.Reader, int64) (string, error)
	CompleteMultipart(context.Context, Put, string, []NativePart) (Object, error)
	AbortMultipart(context.Context, string, string) error
}

// MultipartInspector reports whether the provider still knows an upload session. It is
// an operator-reconciliation capability, not publication authority: a session the
// provider no longer knows can never be completed and can never serve its parts again.
// That absence is terminal for the session only. A completed upload also removes the
// session, so it never says whether an object was published; binding the destination to
// this operation's intent marker is what answers that. A backend without it cannot prove
// a discarded allocation terminal, so retained liability stays until the provider
// lifecycle is inspected out of band.
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

// DirectPostBackend signs one form upload that creates a destination key only if it is
// absent. A form upload cannot carry If-None-Match, so the condition travels inside the
// signed policy (the provider's overwrite switch plus the exact key and length); a client
// that omits it gets a refused policy, not an unprotected write.
type DirectPostBackend interface {
	// DirectPostSupported reports whether the provider's overwrite switch is honoured by a
	// form upload on this connection.
	DirectPostSupported() bool
	// SignPost returns the endpoint, the policy-signed fields and the file field name. No
	// provider request is made here.
	SignPost(ctx context.Context, key string, size int64, mediaType string, expires time.Time) (storage.SignedPartGrant, error)
}

// DirectPutBackend signs one conditional PUT at a destination key, for a driver whose
// provider can create or replace an object in a single request. The condition travels in
// the signed headers, so the client cannot drop it.
type DirectPutBackend interface {
	SignPut(context.Context, string, int64, string, storage.Conditions, time.Time) (storage.SignedPartGrant, error)
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
