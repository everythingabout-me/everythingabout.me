// Package storageproduct names the object stores this service publishes to and the
// promises each one's wire protocol makes about writes.
package storageproduct

import (
	"fmt"
	"strings"

	"everythingabout.me/backend/internal/storage"
)

// Profile is one product's storage behaviour: which driver implementation serves it, how
// bytes reach it, and which write strategies that product actually supports.
//
// Every claim here is a property of the product, not of one bucket. That is the point of
// the table. A per-endpoint probe could only answer "did this exact request succeed just
// now", so it produced an evidence file that had to be carried between machines, could
// disagree with the code indefinitely, and left a newly added provider unable to publish
// until an operator ran a command on the host. A product name is reviewed in a diff and
// ages with the product's own compatibility suite instead.
//
// The cost, stated plainly so nobody has to rediscover it: a stale row is worse than a
// stale probe. A probe that under-reports refuses to publish, which is loud and safe; a
// row that over-reports publishes into a store that silently replaces bytes, which is
// user-visible loss. So every row below cites where its answer comes from, and the one
// capability whose absence silently overwrites a newer file — ConditionalPut — stays
// false unless the product documents it.
type Profile struct {
	// ID is what a connection stores, what appears in errors, and what the settings page
	// offers. It names a product, never a protocol family.
	ID string
	// Backend selects the driver implementation: "cos", "s3" or "local".
	Backend string
	// PathStyle is this product's S3 addressing default. A connection may still override
	// it, because a reverse proxy can move a virtual-host endpoint to a path.
	PathStyle bool
	// TransferClass chooses how bytes travel to this product. Class A means the client and
	// the provider talk directly and this service only signs; class C relays through this
	// service. A product is class A only when the driver can sign a URL for it — COS can
	// today, and the S3-family driver cannot yet — so the class records what this build can
	// actually do, never an aspiration.
	TransferClass string
	Capabilities
}

// Capabilities are the write promises a product makes. False means "this service does not
// use that strategy on this product"; it never means "unknown, try it and see".
type Capabilities struct {
	// ConditionalCreate: a PUT carrying If-None-Match: * on an occupied key is refused.
	ConditionalCreate bool
	// ConditionalPut: a PUT carrying a stale If-Match is refused (412) instead of
	// replacing the object. This decides whether a direct class A replacement is allowed
	// at all, because that grant's only guard is the If-Match the browser sends: the
	// client's PUT *is* the publication, so an unenforced header lets a superseded upload
	// overwrite a newer file for as long as its grant lives (up to an hour).
	ConditionalPut bool
	// AtomicPublish: the bytes read back after a conditional create are the ones the
	// create returned, so a publication cannot half-land and be reported as done.
	AtomicPublish bool
	// MultipartUploads: Begin/UploadPart/Complete work with If-None-Match on Complete.
	MultipartUploads bool
	// FormPost: a form upload — which cannot send any conditional header, so the signed
	// policy is its whole guarantee — is refused on an occupied key.
	FormPost bool
	// SingleWriter: this service is the only publisher of this product's namespace. True
	// for every product here, because that is the premise of the tree — these bytes are
	// ours. It gates the one shortcut that assumes nobody else writes: adopting a
	// brand-new owner's provably empty root without a baseline scan.
	SingleWriter bool
}

// Validate refuses a capability set that cannot publish at all. Conditional create and
// atomic publication are what every publication strategy rests on; a product that
// promises neither is not a store this service can write to, and the error has to say
// which promise is missing rather than "unsupported".
func (c Capabilities) Validate() error {
	missing := make([]string, 0, 2)
	if !c.ConditionalCreate {
		missing = append(missing, "conditionalCreate")
	}
	if !c.AtomicPublish {
		missing = append(missing, "atomicPublish")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the product does not promise %s", strings.Join(missing, " and "))
	}
	return nil
}

// The products this service supports. The list is deliberately short: every row is a
// promise this service has to keep, and a promise nobody uses is a place for the table to
// drift.
//
// Only the COS driver can sign class A URLs in this build, so COS is the only product that
// serves bytes directly; the S3-family rows say relay and their comment says what would
// change that. Do not mark a product presigned-parts without checking that its driver
// implements objectmutation.DirectPutBackend and ReadSignerBackend — a connection that
// advertises class A without them fails at the first upload, after the session opened.
var (
	// AWS S3 documents both conditional headers on PutObject, multipart upload with
	// conditions on Complete, and presigned POST policies whose conditions it enforces.
	//
	// It relays rather than signing, and that is a fact about this driver rather than about
	// AWS: the S3 wire protocol can serve class A, but the driver has no presigner — an
	// S3 presigned URL cannot carry the conditional header a publication needs, and a URL
	// this service cannot sign is not a URL it may hand a browser. Bytes relay through the
	// application until that signer exists, which is one line here and one interface in
	// internal/s3storage.
	awsS3 = Profile{
		ID: "aws-s3", Backend: "s3", TransferClass: storage.TransferRelay,
		Capabilities: Caps(true, true, true, true, true),
	}

	// Tencent COS enforces conditional GET and DELETE but ignores If-Match on PUT: the
	// service's own measured transcript shows it replacing the object on a stale ETag. So
	// COS publishes for real through multipart, where the client's parts stay unpublished
	// until the server completes them, and never through a direct single-PUT replacement.
	cos = Profile{
		ID: "cos", Backend: "cos", TransferClass: storage.TransferPresignedParts,
		Capabilities: Caps(true, false, true, true, true),
	}

	// MinIO supports conditional writes with both If-Match and If-None-Match on PutObject —
	// more than AWS itself — and enforces presigned POST policies. It runs the S3 driver,
	// which has no signer yet, so it relays.
	minio = Profile{
		ID: "minio", Backend: "s3", PathStyle: true, TransferClass: storage.TransferRelay,
		Capabilities: Caps(true, true, true, true, true),
	}

	// RustFS serves the same driver as AWS S3 above, so it relays for the same reason.
	// It implements conditional writes (its s3-tests cover both If-Match and
	// If-None-Match on PutObject; a write with If-None-Match accepts only "*", which is
	// exactly what a conditional create sends) and standard multipart.
	//
	// One known gap: RustFS keeps multipart part listing and part lookup outside its default
	// compatibility gate. Class A uses that listing only to resume an interrupted upload, so
	// a fresh upload finishes and a resume may fail with a typed error — which is the
	// product fact, not a reason to hide the store behind the application.
	//
	// FormPost is false, and not because a form upload is merely unproven: CVE-2026-27607
	// is that RustFS ignores presigned POST policy conditions — content-length-range, key
	// prefix and content type. A form upload's signed policy is its entire guard, so this
	// service never issues one here. No probe could have found that: the failure is a
	// condition that is accepted and not enforced, which looks like success.
	rustfs = Profile{
		ID: "rustfs", Backend: "s3", PathStyle: true, TransferClass: storage.TransferRelay,
		Capabilities: Caps(true, true, true, true, false),
	}

	// Local is a filesystem on this host: there is no provider for a client to reach, so
	// bytes relay through the application and the edge protocol does not apply.
	local = Profile{ID: "local", Backend: "local", TransferClass: storage.TransferRelay}
)

// Caps builds one row's capability set. SingleWriter is true for every object product:
// see its field comment.
func Caps(conditionalCreate, conditionalPut, atomicPublish, multipart, formPost bool) Capabilities {
	return Capabilities{
		ConditionalCreate: conditionalCreate,
		ConditionalPut:    conditionalPut,
		AtomicPublish:     atomicPublish,
		MultipartUploads:  multipart,
		FormPost:          formPost,
		SingleWriter:      true,
	}
}

var byID = map[string]Profile{
	awsS3.ID:  awsS3,
	cos.ID:    cos,
	minio.ID:  minio,
	rustfs.ID: rustfs,
	local.ID:  local,
}

// Lookup resolves one product name. The error names the accepted names, because this is
// reached from a settings request and from a startup script: an unknown product has to be
// fixed at the place that named it, not discovered later as a failed write.
func Lookup(id string) (Profile, error) {
	p, ok := byID[id]
	if !ok {
		return Profile{}, fmt.Errorf("unknown storage product %q: expected one of %v", id, IDs())
	}
	return p, nil
}

// IDs lists every supported product, in the order a person should read them: the
// general-purpose stores first, this deployment's own store next, then the two that are
// single-host or local.
func IDs() []string {
	return []string{awsS3.ID, minio.ID, rustfs.ID, cos.ID, local.ID}
}

// ForBackend lists the products that select one driver implementation. The environment
// forms use it to name the choices when a deployment states a backend but no product.
func ForBackend(backend string) []string {
	out := make([]string, 0, len(byID))
	for _, id := range IDs() {
		if byID[id].Backend == backend {
			out = append(out, id)
		}
	}
	return out
}

// ProductForBackend is the default product for a backend whose configuration predates the
// product name. COS and Local have exactly one product; the S3 driver does not, so it has
// no default and the caller has to name one.
func ProductForBackend(backend string) (string, bool) {
	switch backend {
	case "cos":
		return cos.ID, true
	case "local":
		return local.ID, true
	default:
		return "", false
	}
}
