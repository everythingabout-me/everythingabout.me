// Package config reads environment variables and .env.
//
// A Go process does not load .env by itself. This package searches up to three directories
// upward from the current one, and once it finds .env it fills any variables that are not
// set yet into the process environment, so `cd backend && go run ./cmd/eam` behaves the
// same as starting from the repository root.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is all the configuration one process start needs.
type Config struct {
	Addr        string
	AppEnv      string
	WebDir      string
	DatabaseURL string
	// AppSecret is a stable deployment secret, independent of database/storage credentials.
	AppSecret         string
	FileCookieOrigins []string
	FileUploadIngress string
	Storage           Storage
	SessionTTLDays    int
	CookieSecure      bool
	RegistrationOpen  bool
	// QuotaBytes is the storage cap per account; 0 means unlimited.
	QuotaBytes int64
	// MultipartThreshold is the size above which uploads go multipart; defaults to 5 GiB.
	MultipartThreshold int64
	// UploadPartSize is the part size; defaults to 64 MiB.
	UploadPartSize int64
	COS            COS
	SES            SES
	Timeline       Timeline
}

// Timeline is the configuration for the stream's background organizer.
type Timeline struct {
	Enabled bool
	// Workers is how many files/months run in parallel. Pure metadata work is light; 2 is enough.
	Workers int
	// MonthBudget is how many "display units" a month may take up at most (1 unit ≈ 72px).
	MonthBudget int
	// Timezone decides how "which day" is computed. Photos shot late at night or across
	// time zones are judged by this zone.
	Timezone string
}

type COS struct {
	SecretID  string
	SecretKey string
	Bucket    string
	Region    string
	// Signing credentials are one dedicated class A identity whose policy allows
	// only UploadPart and GetObject on the admitted root. They never fall back to
	// the general mutation credentials or leave the server.
	SignSecretID  string
	SignSecretKey string
}

type SES struct {
	SecretID         string
	SecretKey        string
	Region           string
	FromEmailAddress string
	// TemplateID / Subject are the default, English pair. SES stores one template per language, so
	// Chinese is an optional override: when TemplateIDZh is unset, Chinese requests get the default.
	TemplateID   int64
	TemplateVar  string
	Subject      string
	TemplateIDZh int64
	SubjectZh    string
	ReplyTo      string
}

// IsProduction decides whether cookies get Secure and whether a missing SES setup may print
// the verification code to the log (production must have SES configured; staying silent is
// not an option).
func (c *Config) IsProduction() bool { return c.AppEnv == "production" }

func (c *Config) SessionTTL() time.Duration {
	return time.Duration(c.SessionTTLDays) * 24 * time.Hour
}

// SESConfigured is only true once the sender, template, and keys are all in place.
func (c *Config) SESConfigured() bool {
	return c.SES.FromEmailAddress != "" && c.SES.TemplateID > 0 &&
		c.SES.SecretID != "" && c.SES.SecretKey != ""
}

// COSAppID pulls the account ID out of the APPID suffix of a bucket name; STS policies need it.
func COSAppID(bucket string) string {
	index := strings.LastIndex(bucket, "-")
	if index < 0 || index == len(bucket)-1 {
		return ""
	}
	return bucket[index+1:]
}

// Load reads .env if it exists, then validates the required values.
func Load() (*Config, error) {
	loadDotEnv()

	storageConfig, err := loadStorage(os.Getenv)
	if err != nil {
		return nil, err
	}

	c := &Config{
		Addr:               addr(),
		AppEnv:             envOr("APP_ENV", "development"),
		WebDir:             webDir(),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		AppSecret:          os.Getenv("APP_SECRET"),
		FileUploadIngress:  envOr("FILE_UPLOAD_INGRESS", "embedded"),
		Storage:            storageConfig,
		SessionTTLDays:     positiveInt("SESSION_TTL_DAYS", 30),
		RegistrationOpen:   os.Getenv("ALLOW_REGISTRATION") != "false",
		QuotaBytes:         int64(positiveInt("USER_QUOTA_BYTES", 0)),
		MultipartThreshold: int64(positiveInt("UPLOAD_MULTIPART_THRESHOLD", 5<<30)),
		UploadPartSize:     int64(positiveInt("UPLOAD_PART_SIZE", 64<<20)),
		COS: COS{
			SecretID:      os.Getenv("COS_SECRET_ID"),
			SecretKey:     os.Getenv("COS_SECRET_KEY"),
			Bucket:        os.Getenv("COS_BUCKET"),
			Region:        os.Getenv("COS_REGION"),
			SignSecretID:  os.Getenv("COS_SIGN_SECRET_ID"),
			SignSecretKey: os.Getenv("COS_SIGN_SECRET_KEY"),
		},
		SES: SES{
			SecretID:         os.Getenv("SES_SECRET_ID"),
			SecretKey:        os.Getenv("SES_SECRET_KEY"),
			Region:           envOr("SES_REGION", "ap-guangzhou"),
			FromEmailAddress: os.Getenv("SES_FROM_EMAIL_ADDRESS"),
			TemplateID:       int64(positiveInt("SES_TEMPLATE_ID", 0)),
			TemplateVar:      envOr("SES_TEMPLATE_VAR", "code"),
			Subject:          envOr("SES_SUBJECT", "Everything About Me verification code"),
			TemplateIDZh:     int64(positiveInt("SES_TEMPLATE_ID_ZH", 0)),
			SubjectZh:        envOr("SES_SUBJECT_ZH", "Everything About Me 验证码"),
			ReplyTo:          os.Getenv("SES_REPLY_TO_ADDRESS"),
		},
		Timeline: Timeline{
			Enabled:     os.Getenv("TIMELINE_ENABLED") != "false",
			Workers:     positiveInt("TIMELINE_WORKERS", 2),
			MonthBudget: positiveInt("TIMELINE_MONTH_BUDGET", 24),
			Timezone:    envOr("TIMELINE_TZ", "Asia/Shanghai"),
		},
	}

	if raw := os.Getenv("FILE_COOKIE_ORIGINS"); raw != "" {
		for _, origin := range strings.Split(raw, ",") {
			c.FileCookieOrigins = append(c.FileCookieOrigins, strings.TrimSpace(origin))
		}
	} else if !c.IsProduction() {
		c.FileCookieOrigins = []string{"http://localhost:5173", "http://127.0.0.1:5173"}
	}

	// An explicit COOKIE_SECURE wins; otherwise production defaults to Secure.
	switch strings.ToLower(os.Getenv("COOKIE_SECURE")) {
	case "true":
		c.CookieSecure = true
	case "false":
		c.CookieSecure = false
	default:
		c.CookieSecure = c.IsProduction()
	}

	if err := c.ValidateUploadIngress(); err != nil {
		return nil, err
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("missing required environment variable DATABASE_URL")
	}

	return c, nil
}

// ValidateCOS is only for commands that really touch the bucket (server / doctor /
// reconcile / claim); pure database work such as migrations needs no COS config.
func (c *Config) ValidateCOS() error {
	if c.COS.Bucket != "" && COSAppID(c.COS.Bucket) == "" {
		return fmt.Errorf("COS_BUCKET must include the APPID, e.g. mydata-1250000000")
	}
	for name, value := range map[string]string{
		"COS_SECRET_ID":  c.COS.SecretID,
		"COS_SECRET_KEY": c.COS.SecretKey,
		"COS_BUCKET":     c.COS.Bucket,
		"COS_REGION":     c.COS.Region,
	} {
		if value == "" {
			return fmt.Errorf("missing required environment variable %s", name)
		}
	}
	return nil
}

// webDir tries web/dist under the repository root and under backend/ in turn, so that both
// `cd backend && go run` and `./bin/eam` from the repository root find the frontend.
func webDir() string {
	if value := os.Getenv("WEB_DIR"); value != "" {
		return value
	}
	for _, candidate := range []string{
		filepath.Join("web", "dist"),
		filepath.Join("..", "web", "dist"),
		filepath.Join("..", "..", "web", "dist"),
	} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return filepath.Join("web", "dist")
}

func addr() string {
	if value := os.Getenv("ADDR"); value != "" {
		return value
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func positiveInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// loadDotEnv looks up to three directories up from the current one for .env and only fills
// in variables that are not set yet.
func loadDotEnv() {
	path := os.Getenv("EAM_ENV_FILE")
	if path == "" {
		dir, err := os.Getwd()
		if err != nil {
			return
		}
		for depth := 0; depth < 4 && dir != ""; depth++ {
			candidate := filepath.Join(dir, ".env")
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				path = candidate
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if path == "" {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || !validEnvKey(key) {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		os.Setenv(key, unquote(strings.TrimSpace(raw)))
	}
}

func validEnvKey(key string) bool {
	for i, r := range key {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
