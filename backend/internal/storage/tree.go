package storage

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"everythingabout.me/backend/internal/filecontract"
)

// Path is exact native root-relative spelling. The empty path is the bound root.
// Normalization is used only for comparisons, never to rewrite native names.
type Path string
type Kind string

const (
	File   Kind = "file"
	Folder Kind = "folder"
)

type Digest struct {
	Algorithm string
	Value     []byte
}
type Entry struct {
	Path      Path
	Kind      Kind
	NativeRef []byte
	// Revision is the driver's opaque token for this exact native state. It is
	// what a conditional effect compares against and what proves an effect was
	// applied to the object we observed. Its derivation belongs to the driver:
	// an object store derives it from the object ETag, a versioned provider from
	// its version id. It is not a retained version; see Descriptor.RetainsVersions.
	Revision []byte
	// ContentIdentity is private driver-proven identity independent of pathname.
	// Only admitted coordinated/versioned drivers may supply it; it is not an app file ID.
	ContentIdentity []byte
	// NativeETag is the provider's current ETag for this entry, spelled exactly as
	// the provider writes it, set only when the observation that produced the entry
	// already carried it. It exists because the
	// mutation driver's revision is an ETag: without this field it re-reads every
	// listed child with its own HEAD to recover a value the listing had already
	// answered, which is one extra request per entry per listing. A driver that has
	// no ETag to offer leaves it nil and nothing changes.
	NativeETag []byte
	Size       int64
	ModifiedAt time.Time
	MediaType  string
	Digest     *Digest
}
type PageRequest struct {
	Cursor string
	Limit  int
}
type Page struct {
	Entries    []Entry
	NextCursor string
}
type ReadOptions struct {
	Offset int64
	Length *int64
	// PinnedVersion selects one provider version id for this read, which needs a
	// provider that retains versions. A revision cannot do this: it names the
	// current state, not a retained copy.
	PinnedVersion []byte
}
type Read struct {
	Body   io.ReadCloser
	Entry  Entry
	Offset int64
	Length int64
}
type Tree interface {
	Stat(context.Context, Path) (Entry, error)
	List(context.Context, Path, PageRequest) (Page, error)
	Open(context.Context, Path, ReadOptions) (Read, error)
}
type Driver interface {
	Tree
	Describe() Descriptor
	Close() error
}

// ReferenceReader serves content through a native reference (the object key) that the
// owner-scoped index already proved for this exact path. Path-addressed Stat and Open
// must re-establish namespace truth by walking ancestors; a reference-addressed read
// skips that walk, so serving content does not list every ancestor directory.
//
// The reference is not authority to read arbitrary objects: an implementation must
// compare it with the key it derives for the path and refuse anything else as
// "stale_reference". Bytes and validators still come from native responses; the index
// only says where the file is. StatReference is one native observation (HEAD) and
// OpenReference is one ranged transfer (GET), so a conditional revalidation can be
// answered after the observation, before any body is opened.
type ReferenceReader interface {
	StatReference(ctx context.Context, p Path, ref []byte) (Entry, error)
	OpenReference(ctx context.Context, p Path, ref []byte, options ReadOptions) (Read, error)
}
type Factory interface {
	Open(context.Context, Config) (Driver, error)
}
type Config struct {
	ConnectionID string
	Generation   int64
	OwnerID      string
	ContentRoot  string
	SystemRoot   string
	// RootAtBucket says ContentRoot is the bucket itself rather than a prefix inside
	// it, so ContentRoot is empty. It exists because the empty string is also how an
	// unconfigured root is spelled; a connection that really wants the bucket top says
	// so, and one that forgot to say anything still fails.
	RootAtBucket bool
	Credentials  SecretSource
}
type SecretSource interface {
	Load(context.Context) ([]byte, error)
}

// Descriptor advertises what this driver has actually been proven to do. Two
// flags look similar and are not: PinnedRead and RetainsVersions both concern
// provider version history, while Entry.Revision concerns the current state and
// exists on every driver.
type Descriptor struct {
	MaxFileBytes int64
	RangeRead    bool
	// PinnedRead: a reader may select one provider version through
	// ReadOptions.PinnedVersion. Requires retained versions.
	PinnedRead bool
	// RetainsVersions: the provider keeps superseded copies after a delete or
	// replace, so removing bytes does not free them. This system does not ask for
	// version history and every deployed driver reports false.
	RetainsVersions           bool
	AtomicCreate              bool
	AtomicReplace             bool
	NativeConditionalMutation bool
	NativeTreeMove            bool
	// ImplicitParents: the provider makes a folder when its first child is written, so the
	// index may hold an ancestor row the provider has no marker object for. A filesystem
	// needs the directory before the file exists, so it reports false and an upload still
	// materializes its ancestors before admission binds them.
	ImplicitParents bool
	// TransferClasses is the subset of presigned-parts, resumable-session and
	// relay this driver can serve. The connection selects exactly one of them.
	TransferClasses []string
	Names           NameRules
}
type NameRules struct {
	CaseSensitive   bool
	MaxSegmentBytes int
	MaxPathBytes    int
	MaxDepth        int
	ReservedNames   []string
}

// Error carries safe storage semantics. Provider messages are retained privately as
// causes for logging, never copied to a public HTTP failure body.
type Error struct {
	Code  string
	Path  Path
	Cause error
	// Provider holds what the provider answered with when it refused a request: the
	// HTTP status, the provider's own error code and its request id. A driver sets it
	// whenever it got a response, and leaves it nil for an error that never reached the
	// provider (a connection reset, a TLS failure), which is itself the distinction
	// between "the provider said no" and "we never heard back".
	//
	// It may cross to a client, unlike Cause: these three facts are what a person needs
	// to report a failure — they are the provider's words for it, in a form the provider
	// itself can look up — and none of them is response body text.
	Provider *ProviderError
}

// ProviderError is the identity of one refused provider request.
type ProviderError struct {
	// Status is the HTTP status the provider answered, or 0 when no response arrived.
	Status int
	// Code is the provider's own error code (for COS: SlowDown, AccessDenied, ...).
	Code string
	// RequestID is the provider's id for this request, the only handle its support has.
	RequestID string
}

// Diagnostic renders the provider facts for a message a person or a log will read.
// It is empty when no response arrived, because inventing "connection reset" here
// would replace the one fact that distinguishes it — the absence of a response.
func (p *ProviderError) Diagnostic() string {
	if p == nil {
		return ""
	}
	var parts []string
	if p.Status != 0 {
		parts = append(parts, "HTTP "+strconv.Itoa(p.Status))
	}
	if p.Code != "" {
		parts = append(parts, p.Code)
	}
	if p.RequestID != "" {
		parts = append(parts, "request "+p.RequestID)
	}
	return strings.Join(parts, ", ")
}

// Error deliberately omits Cause: provider response bodies are never surfaced to
// clients, so callers that want operator-facing detail must add it themselves
// (see the admission errors in storagebackend).
func (e *Error) Error() string {
	text := "storage " + e.Code
	if e.Path != "" {
		text += ": " + string(e.Path)
	}
	// The provider's status and request id are safe text and are the difference between
	// a failure a person can report and one only the server log knows about.
	if facts := e.Provider.Diagnostic(); facts != "" {
		text += " (" + facts + ")"
	}
	return text
}
func (e *Error) Unwrap() error { return e.Cause }
func NewError(code string, path Path, cause error) error {
	return &Error{Code: code, Path: path, Cause: cause}
}

// NewProviderError records what the provider answered alongside the code this
// application gives the failure.
func NewProviderError(code string, path Path, status int, providerCode, requestID string, cause error) error {
	return &Error{Code: code, Path: path, Cause: cause, Provider: &ProviderError{Status: status, Code: providerCode, RequestID: requestID}}
}
func IsCode(err error, code string) bool { var e *Error; return errors.As(err, &e) && e.Code == code }
func ValidatePath(path Path) error {
	if _, err := filecontract.FromRelative(string(path)); err != nil {
		return NewError("invalid_name", path, err)
	}
	return nil
}

// ValidateBinding checks identity fields shared by all factories. Adapter-specific
// validation must also establish physical separation of content and system roots.
func ValidateBinding(cfg Config) error {
	if cfg.OwnerID == "" || cfg.ConnectionID == "" || cfg.Generation < 1 || cfg.SystemRoot == "" || (cfg.ContentRoot == "" && !cfg.RootAtBucket) {
		return NewError("invalid_config", "", nil)
	}
	if err := ValidatePath(Path(cfg.OwnerID)); err != nil {
		return NewError("invalid_config", "", err)
	}
	for _, c := range cfg.OwnerID {
		if c == '/' {
			return NewError("invalid_config", "", nil)
		}
	}
	return nil
}
