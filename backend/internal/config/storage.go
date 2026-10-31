package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"everythingabout.me/backend/internal/filecontract"
)

// Storage configures one connection with deterministic per-owner roots. Connection
// identity and generation must be backed up independently from credential rotation.
type Storage struct {
	Backend      string
	ConnectionID string
	Generation   int64
	Local        Local
	S3           S3
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

// BackendName also supports existing manually constructed config values in tests.
func (c *Config) BackendName() string {
	if c.Storage.Backend == "" {
		return "cos"
	}
	return c.Storage.Backend
}

func (c *Config) ValidateStorage() error {
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
