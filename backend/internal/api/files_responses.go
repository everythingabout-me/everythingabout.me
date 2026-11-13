package api

import (
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
)

// The file API's JSON resources live here as named types rather than inline
// map[string]any literals.
//
// A map documents nothing: a handler can rename a key, and the only copy of the
// contract a web client has is the hand-written interface in web/src/api/client.ts.
// That is exactly the drift this repository has shipped before. A struct gives the
// schema check (files_contract_schema_test.go) something to reflect over, so a
// renamed JSON tag fails a test instead of a browser. Fields are ordered as the
// documentation reads, not as JSON requires: object key order is not part of the
// contract.

// filesQuota is the owner's logical and physical storage accounting.
type filesQuota struct {
	LogicalBytes       int64 `json:"logicalBytes"`
	LogicalLimitBytes  int64 `json:"logicalLimitBytes"`
	PhysicalBytes      int64 `json:"physicalBytes"`
	PhysicalLimitBytes int64 `json:"physicalLimitBytes"`
}

// filesReservations is the part of the quota already promised to open sessions.
type filesReservations struct {
	LogicalBytes  int64 `json:"logicalBytes"`
	PhysicalBytes int64 `json:"physicalBytes"`
}

type filesPrincipalKind struct {
	Kind string `json:"kind"`
}

type filesPrincipalScope struct {
	Path fc.Path `json:"path"`
}

type filesPrincipalUsage struct {
	Bytes   int64 `json:"bytes"`
	Files   int64 `json:"files"`
	Indexed bool  `json:"indexed"`
}

// filesPrincipalResource is GET B/me: the caller's grants, scope and usage.
type filesPrincipalResource struct {
	Principal    filesPrincipalKind  `json:"principal"`
	Scope        filesPrincipalScope `json:"scope"`
	Read         bool                `json:"read"`
	Write        bool                `json:"write"`
	Changes      bool                `json:"changes"`
	Subscribe    bool                `json:"subscribe"`
	Usage        filesPrincipalUsage `json:"usage"`
	Quota        *filesQuota         `json:"quota"`
	Reservations *filesReservations  `json:"reservations"`
	CSRFToken    string              `json:"csrfToken,omitempty"`
}

type filesIdentityCapabilities struct {
	ID          bool `json:"id"`
	Path        bool `json:"path"`
	UniquePaths bool `json:"uniquePaths"`
}

type filesChangeTrackingCapabilities struct {
	Available                   bool     `json:"available"`
	Read                        bool     `json:"read"`
	Subscribe                   bool     `json:"subscribe"`
	Delivery                    []string `json:"delivery"`
	RetentionSeconds            int64    `json:"retentionSeconds"`
	SubscriptionLifetimeSeconds int64    `json:"subscriptionLifetimeSeconds"`
	SnapshotLifetimeSeconds     int64    `json:"snapshotLifetimeSeconds"`
	SnapshotMaxEntries          int64    `json:"snapshotMaxEntries"`
	SnapshotMaxBytes            int64    `json:"snapshotMaxBytes"`
	Observation                 string   `json:"observation"`
	ExternalChanges             string   `json:"externalChanges"`
}

type filesPaginationCapabilities struct {
	DefaultLimit int `json:"defaultLimit"`
	MaxLimit     int `json:"maxLimit"`
}

type filesRateLimitCapabilities struct {
	PerMinute int `json:"perMinute"`
}

type filesNameCapabilities struct {
	CaseSensitive   bool `json:"caseSensitive"`
	MaxSegmentBytes int  `json:"maxSegmentBytes"`
	MaxPathBytes    int  `json:"maxPathBytes"`
	MaxDepth        int  `json:"maxDepth"`
}

// filesCapabilitiesResource is GET B/capabilities.
type filesCapabilitiesResource struct {
	Identity                  filesIdentityCapabilities       `json:"identity"`
	ChangeTracking            filesChangeTrackingCapabilities `json:"changeTracking"`
	Path                      fc.Path                         `json:"path"`
	Read                      bool                            `json:"read"`
	Write                     bool                            `json:"write"`
	Range                     bool                            `json:"range"`
	PinnedRead                bool                            `json:"pinnedRead"`
	StrongContentValidators   bool                            `json:"strongContentValidators"`
	TransferClass             string                          `json:"transferClass"`
	TransferKind              string                          `json:"transferKind"`
	TransferClasses           []string                        `json:"transferClasses"`
	AtomicCreate              bool                            `json:"atomicCreate"`
	Downloads                 bool                            `json:"downloads"`
	Thumbnails                bool                            `json:"thumbnails"`
	Uploads                   bool                            `json:"uploads"`
	Checksums                 []string                        `json:"checksums"`
	SourceModifiedAt          bool                            `json:"sourceModifiedAt"`
	MaxFileBytes              *int64                          `json:"maxFileBytes"`
	MaxSinglePutBytes         int64                           `json:"maxSinglePutBytes"`
	MaxPartBytes              *int64                          `json:"maxPartBytes"`
	Pagination                filesPaginationCapabilities     `json:"pagination"`
	RateLimit                 filesRateLimitCapabilities      `json:"rateLimit"`
	Names                     filesNameCapabilities           `json:"names"`
	SessionExpirySeconds      *int64                          `json:"sessionExpirySeconds"`
	SessionMaxLifetimeSeconds *int64                          `json:"sessionMaxLifetimeSeconds"`
	MaxUploadParts            int                             `json:"maxUploadParts"`
	TerminalRetentionSeconds  *int64                          `json:"terminalRetentionSeconds"`
	Quota                     *filesQuota                     `json:"quota"`
	Reservations              *filesReservations              `json:"reservations"`
}

// filesUploadSession is one resumable session (POST/GET B/uploads, GET
// B/uploads/{id}). Transport is opaque outside the client's transfer executor, so
// it stays an interface here.
type filesUploadSession struct {
	Class         string    `json:"class"`
	ID            string    `json:"id"`
	Path          string    `json:"path"`
	State         string    `json:"state"`
	Size          int64     `json:"size"`
	PartSize      int64     `json:"partSize"`
	PartCount     int       `json:"partCount"`
	Ordering      string    `json:"ordering"`
	NextPart      *int      `json:"nextPart"`
	ReceivedParts int       `json:"receivedParts"`
	ReceivedBytes int64     `json:"receivedBytes"`
	CreatedAt     time.Time `json:"createdAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	PartsURL      string    `json:"partsUrl"`
	CommitURL     string    `json:"commitUrl"`
	SignURL       string    `json:"signUrl,omitempty"`
	OperationID   string    `json:"operationId"`
	Result        any       `json:"result"`
	Transport     any       `json:"transport,omitempty"`
	BytesPresent  bool      `json:"bytesPresent,omitempty"`
}

type filesFormField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// filesPartGrant is one class A part URL. A form upload carries its signed values
// in Fields and its file in FileField instead of request headers.
type filesPartGrant struct {
	Number    int               `json:"number"`
	Size      int64             `json:"size"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Fields    []filesFormField  `json:"fields,omitempty"`
	FileField string            `json:"fileField,omitempty"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

// filesSignedParts is the answer to POST B/uploads/{id}/parts/sign.
type filesSignedParts struct {
	Parts     []*filesPartGrant `json:"parts"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

// filesUploadReceipt is a class C durable part receipt.
type filesUploadReceiptResource struct {
	Number int    `json:"number"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// filesProviderPart is one part the provider reports for a class A session.
type filesProviderPart struct {
	Number int    `json:"number"`
	Size   int64  `json:"size"`
	ETag   string `json:"etag"`
}

// filesUploadPartsPage is GET B/uploads/{id}/parts for either class: receipts
// carry Watermark (class C ACK order), provider parts do not.
type filesUploadPartsPage struct {
	Items      []any   `json:"items"`
	NextCursor *string `json:"nextCursor"`
	Watermark  *int64  `json:"watermark,omitempty"`
}

type filesOperationProgress struct {
	Completed int64  `json:"completed"`
	Total     *int64 `json:"total"`
	Unit      string `json:"unit"`
}

// filesOperationRetry is the backoff record of a worker that keeps failing; nil
// while the worker is making progress.
type filesOperationRetry struct {
	Attempts      int       `json:"attempts"`
	Code          string    `json:"code"`
	LastAttemptAt time.Time `json:"lastAttemptAt"`
	NextAttemptAt time.Time `json:"nextAttemptAt"`
}

// filesOperationResource is GET B/operations/{id} and the 202 body of an
// asynchronous mutation.
type filesOperationResource struct {
	ID              string                 `json:"id"`
	Kind            string                 `json:"kind"`
	State           string                 `json:"state"`
	Progress        filesOperationProgress `json:"progress"`
	Result          any                    `json:"result"`
	Error           *fc.Problem            `json:"error"`
	CancelRequested bool                   `json:"cancelRequested"`
	Retrying        *filesOperationRetry   `json:"retrying"`
	CreatedAt       time.Time              `json:"createdAt"`
	FinishedAt      *time.Time             `json:"finishedAt"`
}

// filesDownloadGrantResource is one read grant; a refused item is an fc.Problem
// in the same position.
type filesDownloadGrantResource struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers"`
	ExpiresAt    time.Time         `json:"expiresAt"`
	Size         int64             `json:"size"`
	ContentType  string            `json:"contentType"`
	AcceptRanges bool              `json:"acceptRanges"`
	ETag         string            `json:"etag,omitempty"`
	Conditions   []string          `json:"conditions"`
}

type filesDownloadGrantPage struct {
	Items []any `json:"items"`
}

// filesChangeNotification is the SSE data of one availability wakeup.
type filesChangeNotification struct {
	SubscriptionID string `json:"subscriptionId"`
	Type           string `json:"type"`
	NotificationID string `json:"notificationId"`
}

// filesSubscriptionList is GET B/subscriptions; the resource deliberately has no
// pagination for one principal's active watches.
type filesSubscriptionList struct {
	Items []filesSubscription `json:"items"`
}

// filesChangeSnapshotPage is GET B/change-snapshots/{id}: the snapshot resource
// plus one page of complete metadata.
type filesChangeSnapshotPage struct {
	filesChangeSnapshot
	Items      []fc.Metadata `json:"items"`
	NextCursor *string       `json:"nextCursor"`
}
