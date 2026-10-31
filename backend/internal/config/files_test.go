package config

import (
	"testing"
)

func TestFileCookieOriginsRequireTrustedExplicitOrigins(t *testing.T) {
	for _, raw := range []string{"*", "https://files.example/path", "https://user:pass@files.example", "https://files.example:99999", "https://files.example,"} {
		t.Run(raw, func(t *testing.T) {
			isolatedConfigEnv(t)
			t.Setenv("STORAGE_BACKEND", "local")
			t.Setenv("APP_SECRET", "stable")
			t.Setenv("LOCAL_CONTENT_ROOT", t.TempDir())
			t.Setenv("LOCAL_SYSTEM_ROOT", t.TempDir())
			t.Setenv("FILE_COOKIE_ORIGINS", raw)
			cfg, err := Load()
			if err == nil && cfg.ValidateServer() == nil {
				t.Fatal("untrusted origin configuration accepted")
			}
		})
	}
	isolatedConfigEnv(t)
	t.Setenv("FILE_COOKIE_ORIGINS", "https://files.example, http://127.0.0.1:5173")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.FileCookieOrigins) != 2 || cfg.FileCookieOrigins[1] != "http://127.0.0.1:5173" {
		t.Fatal("explicit origins not loaded")
	}
}
