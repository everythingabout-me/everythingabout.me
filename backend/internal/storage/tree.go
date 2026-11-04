package storage

import (
	"context"
	"errors"
	"io"
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
	Version   []byte
	// ContentIdentity is private driver-proven identity independent of pathname.
	// Only admitted coordinated/versioned drivers may supply it; it is not an app file ID.
	ContentIdentity []byte
	Size            int64
	ModifiedAt      time.Time
	MediaType       string
	Digest          *Digest
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
	Offset  int64
	Length  *int64
	Version []byte
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
	Credentials  SecretSource
}
type SecretSource interface {
	Load(context.Context) ([]byte, error)
}
type Descriptor struct {
	MaxFileBytes              int64
	RangeRead                 bool
	PinnedRead                bool
	AtomicCreate              bool
	AtomicReplace             bool
	NativeConditionalMutation bool
	NativeTreeMove            bool
	RetainsVersions           bool
	Names                     NameRules
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
}

func (e *Error) Error() string { return "storage " + e.Code + ": " + string(e.Path) }
func (e *Error) Unwrap() error { return e.Cause }
func NewError(code string, path Path, cause error) error {
	return &Error{Code: code, Path: path, Cause: cause}
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
	if cfg.OwnerID == "" || cfg.ConnectionID == "" || cfg.Generation < 1 || cfg.ContentRoot == "" || cfg.SystemRoot == "" {
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
