package config

import "testing"

func TestUploadIngressDefaultsAndValidation(t *testing.T) {
	isolatedConfigEnv(t)
	cfg, err := Load()
	if err != nil || cfg.FileUploadIngress != "embedded" || cfg.ExternalUploadIngress() {
		t.Fatalf("default ingress: %+v %v", cfg, err)
	}
	for _, bad := range []string{"gateway", "External", "external ", "false"} {
		t.Setenv("FILE_UPLOAD_INGRESS", bad)
		if _, err = Load(); err == nil {
			t.Fatalf("invalid ingress %q accepted", bad)
		}
	}
	t.Setenv("FILE_UPLOAD_INGRESS", "external")
	if _, err = Load(); err == nil {
		t.Fatal("external ingress accepted without coordinated storage")
	}
	t.Setenv("FILE_COORDINATED_WRITES", "true")
	t.Setenv("FILE_OBJECT_ADMISSION_FILE", "/synthetic/admission.json")
	t.Setenv("FILE_OBJECT_SPOOL_ROOT", "/synthetic/spool")
	cfg, err = Load()
	if err != nil || !cfg.ExternalUploadIngress() {
		t.Fatalf("explicit external ingress: %v", err)
	}
	t.Setenv("STORAGE_BACKEND", "local")
	if _, err = Load(); err == nil {
		t.Fatal("external ingress accepted for local filesystems")
	}
}

func TestUploadGatewayValidatesStorageAndIdentityWithoutMail(t *testing.T) {
	cfg := &Config{AppEnv: "production", AppSecret: "shared-app-identity", FileCookieOrigins: []string{"https://files.example"}, Storage: Storage{Backend: "cos", CoordinatedWrites: true, ObjectAdmissionFile: "/synthetic/admission.json", ObjectSpoolRoot: "/synthetic/gateway-spool"}, COS: COS{SecretID: "synthetic-id", SecretKey: "synthetic-secret", Region: "test-region", Bucket: "synthetic-123456"}}
	if err := cfg.ValidateUploadGateway(); err != nil {
		t.Fatal("gateway should not require SES:", err)
	}
	if err := cfg.ValidateServer(); err == nil {
		t.Fatal("ordinary production server lost its SES requirement")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.AppSecret = "" },
		func(c *Config) { c.FileCookieOrigins = []string{"*"} },
		func(c *Config) { c.Storage.CoordinatedWrites = false },
		func(c *Config) { c.Storage.Backend = "local" },
		func(c *Config) { c.Storage.ObjectSpoolRoot = "relative" },
		func(c *Config) { c.COS.SecretKey = "" },
	} {
		copy := *cfg
		mutate(&copy)
		if err := copy.ValidateUploadGateway(); err == nil {
			t.Fatal("incomplete gateway configuration accepted")
		}
	}
}
