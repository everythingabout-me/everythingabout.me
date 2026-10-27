// Package config 负责读取环境变量和 .env。
//
// Go 进程不会自动加载 .env。这里会从当前目录往上找三层，找到 .env 后
// 把里面还没设置的变量补进进程环境，所以 `cd backend && go run ./cmd/eam`
// 和从仓库根目录启动的行为一致。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是一次进程启动所需的全部配置。
type Config struct {
	Addr             string
	AppEnv           string
	WebDir           string
	DatabaseURL      string
	SessionTTLDays   int
	CookieSecure     bool
	RegistrationOpen bool
	// QuotaBytes 是每个账号的存储上限，0 表示不限。
	QuotaBytes int64
	// MultipartThreshold 超过这个大小的上传走分片，默认 5 GiB。
	MultipartThreshold int64
	// UploadPartSize 是分片大小，默认 64 MiB。
	UploadPartSize int64
	COS            COS
	SES            SES
	Timeline       Timeline
}

// Timeline 是内容流后台整理器的配置。
type Timeline struct {
	Enabled bool
	// Workers 是并行处理文件/月份的数量。纯元数据整理很轻，2 个就够。
	Workers int
	// MonthBudget 是一个月最多占多少"展示单位"（1 单位 ≈ 72px）。
	MonthBudget int
	// Timezone 决定"哪一天"怎么算。照片拍到半夜、跨时区时以这个时区为准。
	Timezone string
}

type COS struct {
	SecretID  string
	SecretKey string
	Bucket    string
	Region    string
}

type SES struct {
	SecretID         string
	SecretKey        string
	Region           string
	FromEmailAddress string
	TemplateID       int64
	TemplateVar      string
	Subject          string
	ReplyTo          string
}

// IsProduction 决定 cookie 是否加 Secure、没配 SES 时是否允许把验证码
// 打印到日志（生产环境必须配好 SES，不能静默）。
func (c *Config) IsProduction() bool { return c.AppEnv == "production" }

func (c *Config) SessionTTL() time.Duration {
	return time.Duration(c.SessionTTLDays) * 24 * time.Hour
}

// SESConfigured：发件人、模板、密钥都齐了才算配置好。
func (c *Config) SESConfigured() bool {
	return c.SES.FromEmailAddress != "" && c.SES.TemplateID > 0 &&
		c.SES.SecretID != "" && c.SES.SecretKey != ""
}

// COSAppID 从桶名的 APPID 后缀里取出账号 ID，STS 策略需要。
func COSAppID(bucket string) string {
	index := strings.LastIndex(bucket, "-")
	if index < 0 || index == len(bucket)-1 {
		return ""
	}
	return bucket[index+1:]
}

// Load 读取 .env（如果存在），再校验必需项。
func Load() (*Config, error) {
	loadDotEnv()

	if bucket := os.Getenv("COS_BUCKET"); bucket != "" && COSAppID(bucket) == "" {
		return nil, fmt.Errorf("COS_BUCKET must include the APPID, e.g. mydata-1250000000")
	}

	c := &Config{
		Addr:               addr(),
		AppEnv:             envOr("APP_ENV", "development"),
		WebDir:             webDir(),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		SessionTTLDays:     positiveInt("SESSION_TTL_DAYS", 30),
		RegistrationOpen:   os.Getenv("ALLOW_REGISTRATION") != "false",
		QuotaBytes:         int64(positiveInt("USER_QUOTA_BYTES", 0)),
		MultipartThreshold: int64(positiveInt("UPLOAD_MULTIPART_THRESHOLD", 5<<30)),
		UploadPartSize:     int64(positiveInt("UPLOAD_PART_SIZE", 64<<20)),
		COS: COS{
			SecretID:  os.Getenv("COS_SECRET_ID"),
			SecretKey: os.Getenv("COS_SECRET_KEY"),
			Bucket:    os.Getenv("COS_BUCKET"),
			Region:    os.Getenv("COS_REGION"),
		},
		SES: SES{
			SecretID:         firstNonEmpty(os.Getenv("SES_SECRET_ID"), os.Getenv("COS_SECRET_ID")),
			SecretKey:        firstNonEmpty(os.Getenv("SES_SECRET_KEY"), os.Getenv("COS_SECRET_KEY")),
			Region:           envOr("SES_REGION", "ap-guangzhou"),
			FromEmailAddress: os.Getenv("SES_FROM_EMAIL_ADDRESS"),
			TemplateID:       int64(positiveInt("SES_TEMPLATE_ID", 0)),
			TemplateVar:      envOr("SES_TEMPLATE_VAR", "code"),
			Subject:          envOr("SES_SUBJECT", "Everything About Me 验证码"),
			ReplyTo:          os.Getenv("SES_REPLY_TO_ADDRESS"),
		},
		Timeline: Timeline{
			Enabled:     os.Getenv("TIMELINE_ENABLED") != "false",
			Workers:     positiveInt("TIMELINE_WORKERS", 2),
			MonthBudget: positiveInt("TIMELINE_MONTH_BUDGET", 24),
			Timezone:    envOr("TIMELINE_TZ", "Asia/Shanghai"),
		},
	}

	// CookieSecure 显式设置优先；否则正式环境默认加 Secure。
	switch strings.ToLower(os.Getenv("COOKIE_SECURE")) {
	case "true":
		c.CookieSecure = true
	case "false":
		c.CookieSecure = false
	default:
		c.CookieSecure = c.IsProduction()
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("missing required environment variable DATABASE_URL")
	}

	return c, nil
}

// ValidateCOS 只给真正要访问桶的命令（server / doctor / reconcile / claim）用，
// 建表这类纯数据库操作不需要 COS 配置。
func (c *Config) ValidateCOS() error {
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

// webDir 依次尝试仓库根目录和 backend/ 目录下的 web/dist，
// 所以 `cd backend && go run` 和仓库根目录下的 `./bin/eam` 都能找到前端。
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

// loadDotEnv 从当前目录向上最多三层找 .env，只补充还没设置的变量。
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
