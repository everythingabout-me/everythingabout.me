package connections

import (
	"path/filepath"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/config"
)

func deploymentStorage() config.Storage {
	return config.Storage{
		ObjectAdmissionFile: "/srv/eam/state/admission.json",
		ObjectSpoolRoot:     "/srv/eam/object-spool",
	}
}

func testConnection(id, name, backend string) Connection {
	return Connection{ID: id, Name: name, Backend: backend}
}

// A second object connection must not inherit the deployment's evidence or spool:
// inheriting the file makes the driver refuse its own evidence, and a probe run
// against it would overwrite the connection that file describes.
func TestObjectConnectionPathsDerivePerConnectionPaths(t *testing.T) {
	c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "nas", "s3")
	admission, spool, err := ObjectPaths(deploymentStorage(), c, "", "")
	if err != nil {
		t.Fatalf("derive paths: %v", err)
	}
	if want := filepath.Join("/srv/eam/state", "admission-"+c.ID+".json"); admission != want {
		t.Errorf("admission evidence = %q, want %q", admission, want)
	}
	if want := filepath.Join("/srv/eam/object-spool", c.ID); spool != want {
		t.Errorf("spool root = %q, want %q", spool, want)
	}
	deployment := deploymentStorage()
	if admission == deployment.ObjectAdmissionFile || spool == deployment.ObjectSpoolRoot {
		t.Errorf("derived paths overlap the deployment's: %q %q", admission, spool)
	}
	// The name is a human label now; it must not appear in either path, or two owners
	// that call a connection "nas" share one directory again.
	if strings.Contains(spool, "nas") || strings.Contains(admission, "nas") {
		t.Errorf("a derived path still carries the connection name: %q %q", admission, spool)
	}
}

// 036 lets two owners each name a connection "nas". Their on-disk state must not
// collide, so the leaf is the row id.
func TestSameNameDifferentOwnersGetDisjointDirectories(t *testing.T) {
	deployment := deploymentStorage()
	first := testConnection("11111111-1111-4111-8111-111111111111", "nas", "cos")
	first.OwnerID = "owner-one"
	second := testConnection("22222222-2222-4222-8222-222222222222", "nas", "cos")
	second.OwnerID = "owner-two"

	firstAdmission, firstSpool, err := ObjectPaths(deployment, first, "", "")
	if err != nil {
		t.Fatalf("derive the first owner's paths: %v", err)
	}
	secondAdmission, secondSpool, err := ObjectPaths(deployment, second, "", "")
	if err != nil {
		t.Fatalf("derive the second owner's paths: %v", err)
	}
	if firstAdmission == secondAdmission || firstSpool == secondSpool {
		t.Fatalf("two owners sharing a connection name share on-disk paths: %q/%q and %q/%q",
			firstAdmission, firstSpool, secondAdmission, secondSpool)
	}
	if filepath.Dir(firstSpool) != filepath.Dir(secondSpool) {
		t.Fatalf("derived spools are not siblings under the deployment root: %q %q", firstSpool, secondSpool)
	}
	if filepath.Dir(firstAdmission) != filepath.Dir(secondAdmission) {
		t.Fatalf("derived evidence files are not siblings beside the deployment evidence: %q %q", firstAdmission, secondAdmission)
	}
}

func TestObjectConnectionPathsKeepExplicitPaths(t *testing.T) {
	c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "nas", "s3")
	admission, spool, err := ObjectPaths(deploymentStorage(), c, "/srv/eam/state/nas.json", "/var/spool/nas")
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
			c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "nas", "s3")
			if _, _, err := ObjectPaths(deployment, c, test.admission, test.spool); err == nil {
				t.Fatal("sharing the deployment's path was accepted")
			}
		})
	}
}

// Without a deployment path to derive from, guessing would place evidence somewhere
// the driver does not read, so the operator has to name it.
func TestObjectConnectionPathsRequireSomethingToDeriveFrom(t *testing.T) {
	c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "nas", "s3")
	if _, _, err := ObjectPaths(config.Storage{}, c, "", ""); err == nil {
		t.Fatal("deriving from an empty deployment configuration was accepted")
	}
}

// A row id is a path component now, so it is validated like one.
func TestObjectConnectionPathsRequireAUsableID(t *testing.T) {
	for _, id := range []string{"", "..", "a/b", `a\b`, "a\x00b"} {
		c := testConnection(id, "nas", "s3")
		if _, _, err := ObjectPaths(deploymentStorage(), c, "", ""); err == nil {
			t.Errorf("row id %q was accepted as a path component", id)
		}
	}
}

// A local connection stages inside its own system root, so it owns no object
// evidence or spool and the derivation must not invent one.
func TestObjectConnectionPathsLeaveLocalConnectionsAlone(t *testing.T) {
	c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "local-nas", "local")
	admission, spool, err := ObjectPaths(deploymentStorage(), c, "", "")
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

// A missing choice and an explicit "the bucket itself" are different requests: one
// keeps the deployment default, the other says the tree is the bucket.
func TestS3ContentPrefixDistinguishesDefaultFromRoot(t *testing.T) {
	empty := ""
	root, atBucket, err := S3ContentPrefix(&empty)
	if err != nil || root != "" || !atBucket {
		t.Fatalf("explicit empty prefix gave %q atBucket=%t err=%v", root, atBucket, err)
	}
	if prefix, atBucket, err := S3ContentPrefix(nil); err != nil || prefix != "drive/" || atBucket {
		t.Fatalf("no choice gave %q atBucket=%t err=%v", prefix, atBucket, err)
	}
	slash := "/"
	if prefix, atBucket, err := S3ContentPrefix(&slash); err != nil || prefix != "" || !atBucket {
		t.Fatalf("a bare slash gave %q atBucket=%t err=%v", prefix, atBucket, err)
	}
	named := " /eam "
	if prefix, atBucket, err := S3ContentPrefix(&named); err != nil || prefix != "eam/" || atBucket {
		t.Fatalf("named prefix gave %q atBucket=%t err=%v", prefix, atBucket, err)
	}
	for _, bad := range []string{"__eam_system__", "__eam_system__/x", "../x", "a//b"} {
		if _, _, err := S3ContentPrefix(&bad); err == nil {
			t.Errorf("prefix %q was accepted", bad)
		}
	}
}
