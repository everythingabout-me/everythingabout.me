package config

import "testing"

func TestUploadIngressDefaultsAndValidation(t *testing.T) {
	isolatedConfigEnv(t)
	cfg, err := Load()
	if err != nil || cfg.FileUploadIngress != "embedded" || cfg.ExternalUploadIngress() || cfg.EdgeOneUploadIngress() {
		t.Fatalf("default ingress: %+v %v", cfg, err)
	}
	for _, bad := range []string{"external", "gateway", "EdgeOne", "edgeone ", "false"} {
		t.Setenv("FILE_UPLOAD_INGRESS", bad)
		if _, err = Load(); err == nil {
			t.Fatalf("invalid ingress %q accepted", bad)
		}
	}
	t.Setenv("FILE_UPLOAD_INGRESS", "edgeone")
	if _, err = Load(); err == nil {
		t.Fatal("edgeone ingress accepted without coordinated storage")
	}
	t.Setenv("FILE_COORDINATED_WRITES", "true")
	t.Setenv("FILE_OBJECT_ADMISSION_FILE", "/synthetic/admission.json")
	t.Setenv("FILE_OBJECT_SPOOL_ROOT", "/synthetic/spool")
	t.Setenv("COS_SECRET_ID", "control-id")
	t.Setenv("COS_SECRET_KEY", "control-secret")
	t.Setenv("COS_BUCKET", "bucket-123456")
	t.Setenv("COS_REGION", "ap-guangzhou")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.ValidateStorage(); err != nil {
		t.Fatal(err)
	}
	if !cfg.EdgeOneUploadIngress() || !cfg.ExternalUploadIngress() {
		t.Fatal("explicit edgeone ingress was not selected")
	}
	for _, backend := range []string{"local", "s3"} {
		t.Setenv("STORAGE_BACKEND", backend)
		if _, err = Load(); err == nil {
			t.Fatalf("edgeone ingress accepted for %s", backend)
		}
	}
}
