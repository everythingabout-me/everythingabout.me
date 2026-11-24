package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storageproduct"
)

// Storage configures one connection. A shared connection derives a deterministic root
// per account; an owner-scoped one uses its prefix as that account's root. Connection
// identity and generation must be backed up independently from credential rotation.
type Storage struct {
	Backend      string
	ConnectionID string
	Generation   int64
	// OwnerScoped says this connection serves one account, so that account's tree is
	// the configured prefix itself. A shared connection namespaces each owner's tree
	// under the prefix, because several accounts use it and a prefix a client could
	// name is a prefix a client could point at someone else.
	OwnerScoped bool
	// RootAtBucket says an owner-scoped connection's tree is the bucket itself rather
	// than a prefix inside it. Only an owner-scoped row may ask for it: a shared row
	// would put every account's tree in the same place.
	RootAtBucket      bool
	CoordinatedWrites bool
	MaxSinglePutBytes int64
	ObjectSpoolRoot   string
	// Product is the store's product name (aws-s3, cos, minio, rustfs, local). It selects
	// the driver implementation and the capabilities that go with it; a connection that
	// names only a protocol family has no answer to "will this store replace my bytes".
	Product string
	// TransferClass is the one class this connection serves: presigned-parts,
	// resumable-session or relay. It is derived from Product, never configured: a class is
	// a consequence of how the product answers requests, not a preference.
	TransferClass string
	Local         Local
	S3            S3
}
type Local struct {
	ContentRoot   string
	SystemRoot    string
	CaseSensitive bool
}
type S3 struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	PathStyle       bool
	ContentPrefix   string
	SystemPrefix    string
}

func loadStorage(getenv func(string) string) (Storage, error) {
	value := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	s := Storage{Backend: value("STORAGE_BACKEND", "cos"), ConnectionID: value("STORAGE_CONNECTION_ID", "default"), Product: getenv("STORAGE_PRODUCT"), Generation: 1,
		Local: Local{ContentRoot: getenv("LOCAL_CONTENT_ROOT"), SystemRoot: getenv("LOCAL_SYSTEM_ROOT")},
		S3:    S3{Endpoint: getenv("S3_ENDPOINT"), Region: getenv("S3_REGION"), Bucket: getenv("S3_BUCKET"), AccessKeyID: getenv("S3_ACCESS_KEY_ID"), SecretAccessKey: getenv("S3_SECRET_ACCESS_KEY"), SessionToken: getenv("S3_SESSION_TOKEN"), ContentPrefix: value("S3_CONTENT_PREFIX", "drive/"), SystemPrefix: value("S3_SYSTEM_PREFIX", "__eam_system__/")}}
	s.MaxSinglePutBytes = 32 << 20
	if raw := getenv("FILE_MAX_SINGLE_PUT_BYTES"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 || n > 9007199254740991 {
			return s, fmt.Errorf("FILE_MAX_SINGLE_PUT_BYTES must be a positive safe integer")
		}
		s.MaxSinglePutBytes = n
	}
	s.ObjectSpoolRoot = getenv("FILE_OBJECT_SPOOL_ROOT")
	if raw := getenv("STORAGE_GENERATION"); raw != "" {
		var err error
		s.Generation, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || s.Generation < 1 {
			return s, fmt.Errorf("STORAGE_GENERATION must be a positive integer")
		}
	}
	switch s.Backend {
	case "cos", "local", "s3":
	default:
		return s, fmt.Errorf("STORAGE_BACKEND must be cos, local or s3")
	}
	// An object store has exactly one publication path: the coordinated protocol, where
	// the client's parts stay unpublished until this service completes them. A local root
	// is a filesystem this service writes directly. There is no third mode worth a switch.
	s.CoordinatedWrites = s.Backend != "local"
	// The product resolves before the path-style override: a product carries that
	// addressing default, and an explicit S3_PATH_STYLE still wins over it.
	if s.Product == "" {
		if id, ok := storageproduct.ProductForBackend(s.Backend); ok {
			s.Product = id
		}
	}
	if s.Product == "" {
		return s, fmt.Errorf("STORAGE_PRODUCT is required for STORAGE_BACKEND=%s: %v speak that protocol and differ in what they promise about writes", s.Backend, storageproduct.ForBackend(s.Backend))
	}
	profile, err := storageproduct.Lookup(s.Product)
	if err != nil {
		return s, err
	}
	if profile.Backend != s.Backend {
		return s, fmt.Errorf("STORAGE_PRODUCT %q is served by the %q driver, but STORAGE_BACKEND is %q", s.Product, profile.Backend, s.Backend)
	}
	s.S3.PathStyle = profile.PathStyle
	s.TransferClass = profile.TransferClass
	for key, dest := range map[string]*bool{"LOCAL_CASE_SENSITIVE": &s.Local.CaseSensitive, "S3_PATH_STYLE": &s.S3.PathStyle} {
		if raw := getenv(key); raw != "" {
			v, err := strconv.ParseBool(raw)
			if err != nil {
				return s, fmt.Errorf("%s must be a boolean", key)
			}
			*dest = v
		}
	}
	if strings.TrimSpace(s.ConnectionID) == "" {
		return s, fmt.Errorf("STORAGE_CONNECTION_ID cannot be blank")
	}
	return s, nil
}

// TransferClass is derived from the product, so this only rejects an impossible
// combination: a class A connection needs a provider to hand presigned URLs to, and a
// publisher able to complete an upload.
func (c *Config) validateTransferClass() error {
	class := c.Storage.TransferClass
	if class == "" {
		// A configuration assembled directly by maintenance and test callers carries no
		// resolved class; ask the product rather than assuming a byte path.
		profile, err := c.StorageProfile()
		if err != nil {
			return err
		}
		class = profile.TransferClass
	}
	switch class {
	case storage.TransferRelay:
		return nil
	case storage.TransferPresignedParts:
		if c.BackendName() == "local" {
			return fmt.Errorf("storage product %q is a local root: it has no provider for a client to reach, so it cannot serve class A", c.Storage.Product)
		}
		if !c.Storage.CoordinatedWrites {
			return fmt.Errorf("class A presigned-parts requires coordinated writes on storage product %q, because the server completes every multi-part upload", c.Storage.Product)
		}
		return nil
	case storage.TransferResumableSession:
		return fmt.Errorf("no storage product serves resumable-session: it needs a cloud-drive driver, and none exists")
	default:
		return fmt.Errorf("storage product %q has an unknown transfer class %q", c.Storage.Product, c.Storage.TransferClass)
	}
}

// StorageProfile is the capability table for this connection's product.
func (c *Config) StorageProfile() (storageproduct.Profile, error) {
	id := c.Storage.Product
	if id == "" {
		resolved, ok := storageproduct.ProductForBackend(c.BackendName())
		if !ok {
			return storageproduct.Profile{}, fmt.Errorf("storage connection %q names no product: set STORAGE_PRODUCT to one of %v", c.Storage.ConnectionID, storageproduct.ForBackend(c.BackendName()))
		}
		id = resolved
	}
	profile, err := storageproduct.Lookup(id)
	if err != nil {
		return storageproduct.Profile{}, err
	}
	if profile.Backend != c.BackendName() {
		return storageproduct.Profile{}, fmt.Errorf("storage connection %q is product %q, which the %q driver serves, but its backend is %q", c.Storage.ConnectionID, profile.ID, profile.Backend, c.BackendName())
	}
	return profile, nil
}

func (c *Config) BackendName() string {
	if c.Storage.Backend == "" {
		return "cos"
	}
	return c.Storage.Backend
}

func (c *Config) ValidateStorage() error {
	if err := c.validateTransferClass(); err != nil {
		return err
	}
	return c.ValidateReadOnlyStorage()
}

// ValidateReadOnlyStorage is the half of ValidateStorage a process needs when it only
// reads the native tree. A transfer class describes how this process would publish an
// upload, so it says nothing about a driver opened for reads — and validating it there
// made a class A deployment unable to run `eam reconcile` or `eam storage-admit` at all:
// class A requires coordinated writes, which is exactly what those read-only drivers turn
// off. The full check still runs on the configuration as loaded, before any of this.
func (c *Config) ValidateReadOnlyStorage() error {
	switch c.BackendName() {
	case "cos":
		return c.ValidateCOS()
	case "local":
		if !filepath.IsAbs(c.Storage.Local.ContentRoot) || !filepath.IsAbs(c.Storage.Local.SystemRoot) {
			return fmt.Errorf("LOCAL_CONTENT_ROOT and LOCAL_SYSTEM_ROOT must be absolute existing directories")
		}
		if filepath.Clean(c.Storage.Local.ContentRoot) == filepath.Clean(c.Storage.Local.SystemRoot) {
			return fmt.Errorf("local content and system roots must be separate")
		}
	case "s3":
		for key, value := range map[string]string{"S3_ENDPOINT": c.Storage.S3.Endpoint, "S3_REGION": c.Storage.S3.Region, "S3_BUCKET": c.Storage.S3.Bucket, "S3_ACCESS_KEY_ID": c.Storage.S3.AccessKeyID, "S3_SECRET_ACCESS_KEY": c.Storage.S3.SecretAccessKey} {
			if value == "" {
				return fmt.Errorf("missing required environment variable %s", key)
			}
		}
	default:
		return fmt.Errorf("unsupported storage backend")
	}
	return nil
}

// ValidateServer refuses an accidental identity reset: migrating deployments must
// explicitly set APP_SECRET to the old save-link derivation seed before switching.
func (c *Config) ValidateServer() error {
	for _, origin := range c.FileCookieOrigins {
		if _, err := filecontract.NewCookieMutationPolicy(origin); err != nil {
			return fmt.Errorf("FILE_COOKIE_ORIGINS requires explicit HTTP(S) origins without paths")
		}
	}
	if c.AppSecret == "" {
		return fmt.Errorf("APP_SECRET is required: preserve existing save links by setting it to the previous signing seed before changing storage or database credentials")
	}
	if c.IsProduction() && !c.SESConfigured() {
		return fmt.Errorf("production requires independent SES credentials, sender and template configuration")
	}
	return c.ValidateStorage()
}
