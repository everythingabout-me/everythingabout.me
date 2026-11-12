// Package connections resolves an owner's native tree to the storage connection
// that holds it. A connection is a row, so adding a provider is an INSERT; the
// environment only seeds the first row of a fresh database.
//
// The row is operator data. It never reaches a file client, and no file API path
// selects a provider: clients still call one `/api/files` contract and the service
// decides where the bytes live.
package connections

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"everythingabout.me/backend/internal/config"
)

// Roles. Exactly one authority row backs an owner's binding; backup rows are
// destinations that are never on the write path.
const (
	RoleAuthority = "authority"
	RoleBackup    = "backup"
)

// Connection is one row of storage_connections.
type Connection struct {
	ID string
	// OwnerID is empty for the deployment default every account may use, and set
	// for a connection one account added for itself.
	OwnerID    string
	Name       string
	Backend    string
	Role       string
	Identity   string
	Config     Spec
	Secret     Secret
	Generation int64
	Admission  json.RawMessage
	Enabled    bool
}

// Spec is everything about a connection except its credentials and role. It
// mirrors the subset of config.Storage a driver is constructed from, so the
// conversion stays explicit and testable instead of reflecting over config.
type Spec struct {
	// RootAtBucket roots an owner-scoped connection's tree at the bucket itself instead
	// of under a prefix. It is a flag rather than an empty prefix because the empty
	// string already means "not configured".
	RootAtBucket        bool      `json:"rootAtBucket,omitempty"`
	CoordinatedWrites   bool      `json:"coordinatedWrites,omitempty"`
	MaxSinglePutBytes   int64     `json:"maxSinglePutBytes,omitempty"`
	ObjectAdmissionFile string    `json:"objectAdmissionFile,omitempty"`
	ObjectSpoolRoot     string    `json:"objectSpoolRoot,omitempty"`
	TransferClass       string    `json:"transferClass,omitempty"`
	COS                 COSSpec   `json:"cos,omitempty"`
	S3                  S3Spec    `json:"s3,omitempty"`
	Local               LocalSpec `json:"local,omitempty"`
}

type COSSpec struct {
	Bucket string `json:"bucket,omitempty"`
	Region string `json:"region,omitempty"`
}

type S3Spec struct {
	Endpoint      string `json:"endpoint,omitempty"`
	Region        string `json:"region,omitempty"`
	Bucket        string `json:"bucket,omitempty"`
	ContentPrefix string `json:"contentPrefix,omitempty"`
	SystemPrefix  string `json:"systemPrefix,omitempty"`
	PathStyle     bool   `json:"pathStyle,omitempty"`
}

type LocalSpec struct {
	ContentRoot   string `json:"contentRoot,omitempty"`
	SystemRoot    string `json:"systemRoot,omitempty"`
	CaseSensitive bool   `json:"caseSensitive,omitempty"`
}

// Secret holds this connection's credentials. They live in the row because adding
// a provider must be one INSERT; the accepted cost is that a database dump carries
// usable credentials.
type Secret struct {
	COS COSCredential `json:"cos,omitempty"`
	S3  S3Credential  `json:"s3,omitempty"`
}

type COSCredential struct {
	SecretID  string `json:"secretId,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
}

type S3Credential struct {
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	SessionToken    string `json:"sessionToken,omitempty"`
}

// ErrNotFound reports a connection name that no row defines. It is not a fallback
// condition: a binding that names a missing row means the row was deleted while
// owners still point at it.
var ErrNotFound = errors.New("storage connection not found")

// Identity hashes where the tree lives, not how it is reached and not who may
// reach it. Credentials are excluded so rotating a key does not create a second
// row; the name is excluded so two rows cannot claim the same tree. The account is
// excluded too: an owner-scoped row's tree is its prefix, so the location alone says
// where the tree is, and two accounts naming one location must be rejected by the
// unique index rather than silently given two trees in one place.
func (c Connection) ComputeIdentity() (string, error) {
	shape := struct {
		Backend      string
		COSBucket    string
		COSRegion    string
		S3Endpoint   string
		S3Region     string
		S3Bucket     string
		S3Content    string
		S3System     string
		S3PathStyle  bool
		RootAtBucket bool
		LocalContent string
		LocalSystem  string
		LocalCase    bool
	}{
		Backend:      c.Backend,
		COSBucket:    c.Config.COS.Bucket,
		COSRegion:    c.Config.COS.Region,
		S3Endpoint:   c.Config.S3.Endpoint,
		S3Region:     c.Config.S3.Region,
		S3Bucket:     c.Config.S3.Bucket,
		S3Content:    c.Config.S3.ContentPrefix,
		S3System:     c.Config.S3.SystemPrefix,
		S3PathStyle:  c.Config.S3.PathStyle,
		RootAtBucket: c.Config.RootAtBucket,
		LocalContent: c.Config.Local.ContentRoot,
		LocalSystem:  c.Config.Local.SystemRoot,
		LocalCase:    c.Config.Local.CaseSensitive,
	}
	data, err := json.Marshal(shape)
	if err != nil {
		return "", fmt.Errorf("storage connection %q: hash identity: %w", c.Name, err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// Validate checks what a driver construction would otherwise report as a missing
// environment variable. The row is not the environment, so the error names the row
// and the field that disagreed.
func (c Connection) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("storage connection row has no name")
	}
	if err := ValidateName(c.Name); err != nil {
		return err
	}
	switch c.Role {
	case RoleAuthority, RoleBackup:
	default:
		return fmt.Errorf("storage connection %q: role must be %q or %q, got %q", c.Name, RoleAuthority, RoleBackup, c.Role)
	}
	if c.Generation < 1 {
		return fmt.Errorf("storage connection %q: generation must be positive, got %d", c.Name, c.Generation)
	}
	if c.Config.RootAtBucket {
		if c.Backend == "local" {
			return fmt.Errorf("storage connection %q: rootAtBucket needs an object backend; a local root is already a directory", c.Name)
		}
		if c.OwnerID == "" {
			return fmt.Errorf("storage connection %q: rootAtBucket needs a connection owned by one account, because a shared connection would put every account's tree at the bucket root", c.Name)
		}
	}
	missing := func(what string) error { return fmt.Errorf("storage connection %q: %s is empty", c.Name, what) }
	switch c.Backend {
	case "cos":
		if c.Config.COS.Bucket == "" {
			return missing("cos.bucket")
		}
		if c.Config.COS.Region == "" {
			return missing("cos.region")
		}
		if c.Secret.COS.SecretID == "" {
			return missing("secret.cos.secretId")
		}
		if c.Secret.COS.SecretKey == "" {
			return missing("secret.cos.secretKey")
		}
	case "s3":
		for field, value := range map[string]string{
			"s3.endpoint": c.Config.S3.Endpoint, "s3.region": c.Config.S3.Region, "s3.bucket": c.Config.S3.Bucket,
			"secret.s3.accessKeyId": c.Secret.S3.AccessKeyID, "secret.s3.secretAccessKey": c.Secret.S3.SecretAccessKey,
		} {
			if value == "" {
				return missing(field)
			}
		}
	case "local":
		if !filepath.IsAbs(c.Config.Local.ContentRoot) || !filepath.IsAbs(c.Config.Local.SystemRoot) {
			return fmt.Errorf("storage connection %q: local content and system roots must be absolute, got %q and %q", c.Name, c.Config.Local.ContentRoot, c.Config.Local.SystemRoot)
		}
		if filepath.Clean(c.Config.Local.ContentRoot) == filepath.Clean(c.Config.Local.SystemRoot) {
			return fmt.Errorf("storage connection %q: local content and system roots must differ", c.Name)
		}
	default:
		return fmt.Errorf("storage connection %q: unsupported backend %q", c.Name, c.Backend)
	}
	return nil
}

// ValidateAccountAdded is the rule for a row an account asked for through the API.
// It is stricter than Validate because that request comes from a signed-in account, not
// an operator: `local` would let it read and write arbitrary host paths, and authority is
// the operator's decision, not something an account can grant itself. Naming an endpoint
// is the account's call — attaching your own NAS is the point of the feature — and the
// probe is what decides whether that endpoint can actually hold a tree. The operator CLI
// is not bound by this: it already has host access.
func (c Connection) ValidateAccountAdded() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.OwnerID == "" {
		return fmt.Errorf("storage connection %q: an account's own connection must name that account", c.Name)
	}
	if c.Role != RoleBackup {
		return fmt.Errorf("storage connection %q: an account may only add a backup destination, got role %q; authority is the operator's decision", c.Name, c.Role)
	}
	switch c.Backend {
	case "cos":
		return nil
	case "s3":
		if _, err := S3EndpointHost(c.Config.S3.Endpoint); err != nil {
			return fmt.Errorf("storage connection %q: %w", c.Name, err)
		}
		return nil
	default:
		return fmt.Errorf("storage connection %q: an account may only add a cos bucket or an s3-compatible endpoint, got backend %q", c.Name, c.Backend)
	}
}

// S3EndpointHost is the host an account-named endpoint points at, and it is the value
// the operator's list is matched against. The URL must be explicit and bare: the driver
// signs and addresses the request itself, so credentials, a path, a query or a fragment
// would let a caller address something other than the bucket it named.
func S3EndpointHost(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("endpoint %q must be an http(s) URL for the storage service alone, with no credentials, path, query or fragment (for example https://nas.example.com:9000)", endpoint)
	}
	return strings.ToLower(parsed.Hostname()), nil
}

// Apply folds this row into the process configuration so the existing driver
// factory can be reused unchanged. Everything connection-specific comes from the
// row; everything deployment-specific (database, HTTP, mail) stays with the process.
func (c Connection) Apply(base config.Config) (config.Config, error) {
	if err := c.Validate(); err != nil {
		return base, err
	}
	out := base
	out.Storage = config.Storage{
		Backend:             c.Backend,
		ConnectionID:        c.Name,
		Generation:          c.Generation,
		OwnerScoped:         c.OwnerID != "",
		RootAtBucket:        c.Config.RootAtBucket,
		CoordinatedWrites:   c.Config.CoordinatedWrites,
		MaxSinglePutBytes:   c.Config.MaxSinglePutBytes,
		ObjectAdmissionFile: c.Config.ObjectAdmissionFile,
		ObjectSpoolRoot:     c.Config.ObjectSpoolRoot,
		TransferClass:       c.Config.TransferClass,
		Local: config.Local{
			ContentRoot:   c.Config.Local.ContentRoot,
			SystemRoot:    c.Config.Local.SystemRoot,
			CaseSensitive: c.Config.Local.CaseSensitive,
		},
		S3: config.S3{
			Endpoint:        c.Config.S3.Endpoint,
			Region:          c.Config.S3.Region,
			Bucket:          c.Config.S3.Bucket,
			AccessKeyID:     c.Secret.S3.AccessKeyID,
			SecretAccessKey: c.Secret.S3.SecretAccessKey,
			SessionToken:    c.Secret.S3.SessionToken,
			PathStyle:       c.Config.S3.PathStyle,
			ContentPrefix:   c.Config.S3.ContentPrefix,
			SystemPrefix:    c.Config.S3.SystemPrefix,
		},
	}
	out.COS = config.COS{
		SecretID:  c.Secret.COS.SecretID,
		SecretKey: c.Secret.COS.SecretKey,
		Bucket:    c.Config.COS.Bucket,
		Region:    c.Config.COS.Region,
	}
	return out, nil
}

// FromConfig is how a fresh database gets its first row: the current environment
// becomes one authority connection so an existing deployment keeps its binding.
func FromConfig(cfg config.Config) (Connection, error) {
	c := Connection{
		Name:       cfg.Storage.ConnectionID,
		Backend:    cfg.BackendName(),
		Role:       RoleAuthority,
		Generation: cfg.Storage.Generation,
		Enabled:    true,
		Config: Spec{
			CoordinatedWrites:   cfg.Storage.CoordinatedWrites,
			MaxSinglePutBytes:   cfg.Storage.MaxSinglePutBytes,
			ObjectAdmissionFile: cfg.Storage.ObjectAdmissionFile,
			ObjectSpoolRoot:     cfg.Storage.ObjectSpoolRoot,
			TransferClass:       cfg.Storage.TransferClass,
			RootAtBucket:        cfg.Storage.RootAtBucket,
			COS:                 COSSpec{Bucket: cfg.COS.Bucket, Region: cfg.COS.Region},
			S3: S3Spec{
				Endpoint:      cfg.Storage.S3.Endpoint,
				Region:        cfg.Storage.S3.Region,
				Bucket:        cfg.Storage.S3.Bucket,
				ContentPrefix: cfg.Storage.S3.ContentPrefix,
				SystemPrefix:  cfg.Storage.S3.SystemPrefix,
				PathStyle:     cfg.Storage.S3.PathStyle,
			},
			Local: LocalSpec{
				ContentRoot:   cfg.Storage.Local.ContentRoot,
				SystemRoot:    cfg.Storage.Local.SystemRoot,
				CaseSensitive: cfg.Storage.Local.CaseSensitive,
			},
		},
		Secret: Secret{
			COS: COSCredential{SecretID: cfg.COS.SecretID, SecretKey: cfg.COS.SecretKey},
			S3: S3Credential{
				AccessKeyID:     cfg.Storage.S3.AccessKeyID,
				SecretAccessKey: cfg.Storage.S3.SecretAccessKey,
				SessionToken:    cfg.Storage.S3.SessionToken,
			},
		},
	}
	if c.Generation == 0 {
		c.Generation = 1
	}
	identity, err := c.ComputeIdentity()
	if err != nil {
		return Connection{}, err
	}
	c.Identity = identity
	return c, c.Validate()
}

// bindingName extracts the connection name from a stored binding value, which is
// written as name:digest. The digest is hex, so the last colon is the separator.
func bindingName(binding string) string {
	if i := strings.LastIndex(binding, ":"); i > 0 {
		return binding[:i]
	}
	return binding
}
