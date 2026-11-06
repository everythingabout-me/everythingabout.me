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
	if _, err = Load(); err == nil {
		t.Fatal("edgeone ingress accepted without isolated part signing credentials")
	}
	t.Setenv("COS_SIGN_SECRET_ID", "sign-only-id")
	t.Setenv("COS_SIGN_SECRET_KEY", "sign-only-secret")
	cfg, err = Load()
	if err != nil || !cfg.EdgeOneUploadIngress() || !cfg.ExternalUploadIngress() {
		t.Fatalf("explicit edgeone ingress: %v", err)
	}
	t.Setenv("COS_SECRET_ID", "sign-only-id")
	if _, err = Load(); err == nil {
		t.Fatal("edgeone signer reused general storage credentials")
	}
	t.Setenv("COS_SECRET_ID", "control-id")
	for _, backend := range []string{"local", "s3"} {
		t.Setenv("STORAGE_BACKEND", backend)
		if _, err = Load(); err == nil {
			t.Fatalf("edgeone ingress accepted for %s", backend)
		}
	}
}
