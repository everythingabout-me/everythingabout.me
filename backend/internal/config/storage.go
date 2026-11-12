package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
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
	RootAtBucket        bool
	CoordinatedWrites   bool
	MaxSinglePutBytes   int64
	ObjectAdmissionFile string
	ObjectSpoolRoot     string
	// TransferClass is the one class this connection serves: presigned-parts,
	// resumable-session or relay. It is fixed for the connection, not per transfer.
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
	s := Storage{Backend: value("STORAGE_BACKEND", "cos"), ConnectionID: value("STORAGE_CONNECTION_ID", "default"), Generation: 1,
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
	if raw := getenv("FILE_COORDINATED_WRITES"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return s, fmt.Errorf("FILE_COORDINATED_WRITES must be a boolean")
		}
		s.CoordinatedWrites = v
	}
	s.ObjectAdmissionFile = getenv("FILE_OBJECT_ADMISSION_FILE")
	s.ObjectSpoolRoot = getenv("FILE_OBJECT_SPOOL_ROOT")
	s.TransferClass = getenv("FILE_TRANSFER_CLASS")
	if s.TransferClass == "" {
		// A connection that does not name a class keeps the previous byte path:
		// Local and admitted object connections relay through the application.
		s.TransferClass = storage.TransferRelay
		if s.Backend != "local" && getenv("FILE_UPLOAD_INGRESS") == "edgeone" {
			s.TransferClass = storage.TransferPresignedParts
		}
	}
	switch s.TransferClass {
	case storage.TransferPresignedParts, storage.TransferResumableSession, storage.TransferRelay:
	default:
		return s, fmt.Errorf("FILE_TRANSFER_CLASS must be presigned-parts, resumable-session or relay")
	}
	if s.CoordinatedWrites && s.Backend != "local" && (!filepath.IsAbs(s.ObjectAdmissionFile) || !filepath.IsAbs(s.ObjectSpoolRoot)) {
		return s, fmt.Errorf("object writes require explicit absolute FILE_OBJECT_ADMISSION_FILE and FILE_OBJECT_SPOOL_ROOT")
	}
	if raw := getenv("STORAGE_GENERATION"); raw != "" {
		var err error
		s.Generation, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || s.Generation < 1 {
			return s, fmt.Errorf("STORAGE_GENERATION must be a positive integer")
		}
	}
	for key, dest := range map[string]*bool{"LOCAL_CASE_SENSITIVE": &s.Local.CaseSensitive, "S3_PATH_STYLE": &s.S3.PathStyle} {
		if raw := getenv(key); raw != "" {
			v, err := strconv.ParseBool(raw)
			if err != nil {
				return s, fmt.Errorf("%s must be a boolean", key)
			}
			*dest = v
		}
	}
	switch s.Backend {
	case "cos", "local", "s3":
	default:
		return s, fmt.Errorf("STORAGE_BACKEND must be cos, local or s3")
	}
	if strings.TrimSpace(s.ConnectionID) == "" {
		return s, fmt.Errorf("STORAGE_CONNECTION_ID cannot be blank")
	}
	return s, nil
}

// validateTransferClass rejects a class the selected backend cannot serve with
// the credentials actually configured. A driver's Descriptor must still advertise
// the class; this is the deployment-level half of the same rule.
func (c *Config) validateTransferClass() error {
	class := c.Storage.TransferClass
	if class == "" {
		class = storage.TransferRelay
		c.Storage.TransferClass = class
	}
	switch class {
	case storage.TransferRelay:
		if c.EdgeOneUploadIngress() {
			return fmt.Errorf("class C relay cannot use the EdgeOne class A transfer route")
		}
		return nil
	case storage.TransferPresignedParts:
		if c.BackendName() != "cos" {
			return fmt.Errorf("class A presigned-parts currently requires the COS backend")
		}
		if !c.Storage.CoordinatedWrites {
			return fmt.Errorf("class A presigned-parts requires FILE_COORDINATED_WRITES=true")
		}
		return nil
	case storage.TransferResumableSession:
		return fmt.Errorf("no storage driver advertises resumable-session yet")
	default:
		return fmt.Errorf("FILE_TRANSFER_CLASS must be presigned-parts, resumable-session or relay")
	}
}

func (c *Config) BackendName() string {
	if c.Storage.Backend == "" {
		return "cos"
	}
	return c.Storage.Backend
}

func (c *Config) ValidateStorage() error {
	if c.Storage.CoordinatedWrites && c.BackendName() != "local" && (!filepath.IsAbs(c.Storage.ObjectAdmissionFile) || !filepath.IsAbs(c.Storage.ObjectSpoolRoot)) {
		return fmt.Errorf("object writes require explicit absolute FILE_OBJECT_ADMISSION_FILE and FILE_OBJECT_SPOOL_ROOT")
	}
	if err := c.validateTransferClass(); err != nil {
		return err
	}
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
	if err := c.ValidateUploadIngress(); err != nil {
		return err
	}
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
