package config

import (
	"path/filepath"
	"testing"
)

func isolatedConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("EAM_ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	for _, key := range []string{"APP_SECRET", "STORAGE_BACKEND", "STORAGE_CONNECTION_ID", "STORAGE_GENERATION", "LOCAL_CONTENT_ROOT", "LOCAL_SYSTEM_ROOT", "LOCAL_CASE_SENSITIVE", "S3_ENDPOINT", "S3_REGION", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY", "S3_SESSION_TOKEN", "S3_PATH_STYLE", "S3_CONTENT_PREFIX", "S3_SYSTEM_PREFIX", "COS_BUCKET", "COS_REGION", "COS_SECRET_ID", "COS_SECRET_KEY", "SES_SECRET_ID", "SES_SECRET_KEY", "SES_FROM_EMAIL_ADDRESS", "SES_TEMPLATE_ID", "APP_ENV"} {
		t.Setenv(key, "")
	}
	t.Setenv("DATABASE_URL", "postgres://unused/test")
}
func TestLocalServerConfigWithoutCOS(t *testing.T) {
	isolatedConfigEnv(t)
	t.Setenv("STORAGE_BACKEND", "local")
	t.Setenv("APP_SECRET", "stable-deployment-secret")
	t.Setenv("LOCAL_CONTENT_ROOT", t.TempDir())
	t.Setenv("LOCAL_SYSTEM_ROOT", t.TempDir())
	// Invalid unrelated COS config must not stop the selected backend.
	t.Setenv("COS_BUCKET", "unrelated-invalid-bucket")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.ValidateServer(); err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Generation != 1 || cfg.Storage.ConnectionID != "default" || cfg.BackendName() != "local" || cfg.Storage.Local.CaseSensitive {
		t.Fatalf("unexpected defaults: %+v", cfg.Storage)
	}
	if cfg.SES.SecretID != "" || cfg.SES.SecretKey != "" {
		t.Fatal("SES inherited storage credentials")
	}
}
func TestAppSecretAndMailAreIndependent(t *testing.T) {
	isolatedConfigEnv(t)
	t.Setenv("COS_SECRET_ID", "storage-id")
	t.Setenv("COS_SECRET_KEY", "storage-secret")
	t.Setenv("SES_FROM_EMAIL_ADDRESS", "sender@example.com")
	t.Setenv("SES_TEMPLATE_ID", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SESConfigured() {
		t.Fatal("storage keys configured SES implicitly")
	}
	if cfg.ValidateServer() == nil {
		t.Fatal("missing application identity accepted")
	}
	t.Setenv("APP_SECRET", "persisted-seed")
	t.Setenv("APP_ENV", "production")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ValidateServer() == nil {
		t.Fatal("production without independent SES accepted")
	}
	t.Setenv("SES_SECRET_ID", "mail-id")
	t.Setenv("SES_SECRET_KEY", "mail-secret")
	t.Setenv("COS_BUCKET", "bucket-123456")
	t.Setenv("COS_REGION", "ap-guangzhou")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.ValidateServer(); err != nil {
		t.Fatal(err)
	}
	if cfg.AppSecret != "persisted-seed" || cfg.SES.SecretKey != "mail-secret" {
		t.Fatal("configuration identities changed")
	}
}
func TestStrictBackendConfiguration(t *testing.T) {
	for _, tc := range []struct{ key, value string }{{"STORAGE_BACKEND", "unknown"}, {"STORAGE_GENERATION", "0"}, {"STORAGE_GENERATION", "-2"}, {"STORAGE_GENERATION", "bad"}, {"LOCAL_CASE_SENSITIVE", "maybe"}, {"S3_PATH_STYLE", "yes"}, {"STORAGE_CONNECTION_ID", " "}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			isolatedConfigEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	isolatedConfigEnv(t)
	t.Setenv("STORAGE_BACKEND", "s3")
	t.Setenv("S3_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("S3_REGION", "test-region")
	t.Setenv("S3_BUCKET", "bucket")
	t.Setenv("S3_ACCESS_KEY_ID", "access")
	t.Setenv("S3_SECRET_ACCESS_KEY", "secret")
	t.Setenv("S3_PATH_STYLE", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.ValidateStorage(); err != nil {
		t.Fatal(err)
	}
	if !cfg.Storage.S3.PathStyle || cfg.Storage.S3.ContentPrefix != "drive/" || cfg.Storage.S3.SystemPrefix != "__eam_system__/" {
		t.Fatalf("unexpected S3 settings %+v", cfg.Storage.S3)
	}
}
