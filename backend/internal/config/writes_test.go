package config

import "testing"

// There is one publication path per backend kind, so it is derived rather than chosen:
// an object store stages every publication, a local root writes its filesystem directly.
// The switch that used to make this optional could only produce a connection that could
// not publish at all.
func TestWriteModeIsDerivedFromTheBackend(t *testing.T) {
	isolatedConfigEnv(t)
	t.Setenv("COS_BUCKET", "bucket-123456")
	t.Setenv("COS_REGION", "ap-guangzhou")
	t.Setenv("COS_SECRET_ID", "id")
	t.Setenv("COS_SECRET_KEY", "key")
	// Loading alone does not need a spool root: a read-only command (migrate, doctor's
	// read half) opens no writer. The driver refuses the missing root when a write is
	// actually opened, which is where its absence matters.
	t.Setenv("FILE_OBJECT_SPOOL_ROOT", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Storage.CoordinatedWrites {
		t.Fatal("an object backend was not set up to stage its publications")
	}
	if cfg.Storage.TransferClass != "presigned-parts" {
		t.Fatalf("COS was not given the direct class: %q", cfg.Storage.TransferClass)
	}

	t.Setenv("STORAGE_BACKEND", "local")
	t.Setenv("LOCAL_CONTENT_ROOT", t.TempDir())
	t.Setenv("LOCAL_SYSTEM_ROOT", t.TempDir())
	cfg, err = Load()
	if err != nil || cfg.Storage.CoordinatedWrites {
		t.Fatalf("a local root was given the object protocol: %v %+v", err, cfg.Storage)
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
