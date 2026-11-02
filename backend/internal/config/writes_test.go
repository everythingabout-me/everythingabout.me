package config

import "testing"

func TestNativeWriteAdmissionConfigurationIsExplicit(t *testing.T) {
	isolatedConfigEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.CoordinatedWrites || cfg.Storage.MaxSinglePutBytes != 32<<20 {
		t.Fatal("unsafe write defaults")
	}
	t.Setenv("FILE_COORDINATED_WRITES", "true")
	if _, err = Load(); err == nil {
		t.Fatal("unverified COS write admission accepted")
	}
	t.Setenv("STORAGE_BACKEND", "local")
	cfg, err = Load()
	if err != nil || !cfg.Storage.CoordinatedWrites {
		t.Fatalf("explicit local admission failed %v", err)
	}
	for _, bad := range []string{"0", "-1", "unbounded", "9007199254740992"} {
		t.Setenv("FILE_MAX_SINGLE_PUT_BYTES", bad)
		if _, err = Load(); err == nil {
			t.Fatalf("invalid byte limit %q admitted", bad)
		}
	}
	t.Setenv("FILE_MAX_SINGLE_PUT_BYTES", "1048576")
	cfg, err = Load()
	if err != nil || cfg.Storage.MaxSinglePutBytes != 1048576 {
		t.Fatal("explicit single put policy not loaded")
	}
}
