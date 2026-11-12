package connections

import (
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/config"
)

func deploymentStorage() config.Storage {
	return config.Storage{
		ObjectAdmissionFile: "/srv/eam/state/admission.json",
		ObjectSpoolRoot:     "/srv/eam/object-spool",
	}
}

// A second object connection must not inherit the deployment's evidence or spool:
// inheriting the file makes the driver refuse its own evidence, and a probe run
// against it would overwrite the connection that file describes.
func TestObjectConnectionPathsDerivePerConnectionPaths(t *testing.T) {
	admission, spool, err := ObjectPaths(deploymentStorage(), "nas", "s3", "", "")
	if err != nil {
		t.Fatalf("derive paths: %v", err)
	}
	if want := filepath.Join("/srv/eam/state", "admission-nas.json"); admission != want {
		t.Errorf("admission evidence = %q, want %q", admission, want)
	}
	if want := filepath.Join("/srv/eam/object-spool", "nas"); spool != want {
		t.Errorf("spool root = %q, want %q", spool, want)
	}
	deployment := deploymentStorage()
	if admission == deployment.ObjectAdmissionFile || spool == deployment.ObjectSpoolRoot {
		t.Errorf("derived paths overlap the deployment's: %q %q", admission, spool)
	}
}

func TestObjectConnectionPathsKeepExplicitPaths(t *testing.T) {
	admission, spool, err := ObjectPaths(deploymentStorage(), "nas", "s3", "/srv/eam/state/nas.json", "/var/spool/nas")
	if err != nil {
		t.Fatalf("derive paths: %v", err)
	}
	if admission != "/srv/eam/state/nas.json" || spool != "/var/spool/nas" {
		t.Errorf("explicit paths were not kept: %q %q", admission, spool)
	}
}

func TestObjectConnectionPathsRejectSharedDeploymentPaths(t *testing.T) {
	deployment := deploymentStorage()
	for _, test := range []struct {
		name      string
		admission string
		spool     string
	}{
		{"admission", deployment.ObjectAdmissionFile, ""},
		{"spool", "", deployment.ObjectSpoolRoot},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := ObjectPaths(deployment, "nas", "s3", test.admission, test.spool); err == nil {
				t.Fatal("sharing the deployment's path was accepted")
			}
		})
	}
}

// Without a deployment path to derive from, guessing would place evidence somewhere
// the driver does not read, so the operator has to name it.
func TestObjectConnectionPathsRequireSomethingToDeriveFrom(t *testing.T) {
	if _, _, err := ObjectPaths(config.Storage{}, "nas", "s3", "", ""); err == nil {
		t.Fatal("deriving from an empty deployment configuration was accepted")
	}
}

// A local connection stages inside its own system root, so it owns no object
// evidence or spool and the derivation must not invent one.
func TestObjectConnectionPathsLeaveLocalConnectionsAlone(t *testing.T) {
	admission, spool, err := ObjectPaths(deploymentStorage(), "local-nas", "local", "", "")
	if err != nil {
		t.Fatalf("derive paths: %v", err)
	}
	if admission != "" || spool != "" {
		t.Errorf("local connection got object paths: %q %q", admission, spool)
	}
}

func TestValidateConnectionName(t *testing.T) {
	for _, name := range []string{"nas", "backup-nas", "nas_2", "NAS"} {
		if err := ValidateName(name); err != nil {
			t.Errorf("validate %q: %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "a\x00b"} {
		if err := ValidateName(name); err == nil {
			t.Errorf("validate %q: accepted", name)
		}
	}
}
