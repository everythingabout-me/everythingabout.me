package connections

import (
	"path/filepath"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/config"
)

func deploymentStorage() config.Storage {
	return config.Storage{ObjectSpoolRoot: "/srv/eam/object-spool"}
}

func testConnection(id, name, backend string) Connection {
	return Connection{ID: id, Name: name, Backend: backend}
}

// A second object connection must not inherit the deployment's spool: a shared one would
// let one target's temporary bytes be published to another.
func TestObjectConnectionPathsDeriveAPerConnectionSpool(t *testing.T) {
	c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "nas", "s3")
	spool, err := ObjectPaths(deploymentStorage(), c, "")
	if err != nil {
		t.Fatalf("derive paths: %v", err)
	}
	if want := filepath.Join("/srv/eam/object-spool", c.ID); spool != want {
		t.Errorf("spool root = %q, want %q", spool, want)
	}
	if spool == deploymentStorage().ObjectSpoolRoot {
		t.Errorf("the derived spool overlaps the deployment's: %q", spool)
	}
	// The name is a human label now; it must not appear in the path, or two owners that
	// call a connection "nas" share one directory again.
	if strings.Contains(spool, "nas") {
		t.Errorf("a derived path still carries the connection name: %q", spool)
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

	firstSpool, err := ObjectPaths(deployment, first, "")
	if err != nil {
		t.Fatalf("derive the first owner's path: %v", err)
	}
	secondSpool, err := ObjectPaths(deployment, second, "")
	if err != nil {
		t.Fatalf("derive the second owner's path: %v", err)
	}
	if firstSpool == secondSpool {
		t.Fatalf("two owners sharing a connection name share a spool: %q", firstSpool)
	}
	if filepath.Dir(firstSpool) != filepath.Dir(secondSpool) {
		t.Fatalf("derived spools are not siblings under the deployment root: %q %q", firstSpool, secondSpool)
	}
}

func TestObjectConnectionPathsKeepAnExplicitSpool(t *testing.T) {
	c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "nas", "s3")
	spool, err := ObjectPaths(deploymentStorage(), c, "/var/spool/nas")
	if err != nil {
		t.Fatalf("derive paths: %v", err)
	}
	if spool != "/var/spool/nas" {
		t.Errorf("an explicit spool was not kept: %q", spool)
	}
}

func TestObjectConnectionPathsRejectTheDeploymentSpool(t *testing.T) {
	deployment := deploymentStorage()
	c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "nas", "s3")
	if _, err := ObjectPaths(deployment, c, deployment.ObjectSpoolRoot); err == nil {
		t.Fatal("sharing the deployment's spool was accepted")
	}
}

// Without a deployment root to derive from, guessing would place staged bytes somewhere
// the driver does not read, so the operator has to name it.
func TestObjectConnectionPathsRequireSomethingToDeriveFrom(t *testing.T) {
	c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "nas", "s3")
	if _, err := ObjectPaths(config.Storage{}, c, ""); err == nil {
		t.Fatal("deriving from an empty deployment configuration was accepted")
	}
}

// A row id is a path component now, so it is validated like one.
func TestObjectConnectionPathsRequireAUsableID(t *testing.T) {
	for _, id := range []string{"", "..", "a/b", `a\b`, "a\x00b"} {
		c := testConnection(id, "nas", "s3")
		if _, err := ObjectPaths(deploymentStorage(), c, ""); err == nil {
			t.Errorf("row id %q was accepted as a path component", id)
		}
	}
}

// A local connection stages inside its own system root, so it owns no object spool and
// the derivation must not invent one.
func TestObjectConnectionPathsLeaveLocalConnectionsAlone(t *testing.T) {
	c := testConnection("0f0e9d5a-3f2b-4b1a-9f8e-2d7c6a5b4c3d", "local-nas", "local")
	spool, err := ObjectPaths(deploymentStorage(), c, "")
	if err != nil {
		t.Fatalf("derive paths: %v", err)
	}
	if spool != "" {
		t.Errorf("a local connection got an object spool: %q", spool)
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

// The backup folder is where dates land inside the store, so the normal form has
// to round-trip the spellings a person types and refuse anything that is not a
// native relative path or that names the service's reserved prefix.
func TestNormalizeBackupFolder(t *testing.T) {
	for raw, want := range map[string]string{
		"":          "",
		"backups":   "backups",
		"/backups/": "backups",
		"  /a/b/  ": "a/b",
		"备份/2026":   "备份/2026",
	} {
		got, err := NormalizeBackupFolder(raw)
		if err != nil || got != want {
			t.Errorf("NormalizeBackupFolder(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"__eam_system__", "__eam_system__/x", "../x", "a//b", "/..", "a/./b"} {
		if got, err := NormalizeBackupFolder(bad); err == nil {
			t.Errorf("NormalizeBackupFolder(%q) = %q, want a refusal", bad, got)
		}
	}
}

// A snapshot's path is derived from the row, so the page, the CLI and a restore of
// an older date cannot disagree about where a date lives.
func TestBackupRootFollowsTheConfiguredFolder(t *testing.T) {
	root := Connection{}
	if got := root.BackupRoot("2026-11-14"); got != "2026-11-14" {
		t.Fatalf("root folder gave %q", got)
	}
	placed := Connection{Config: Spec{BackupFolder: "backups"}}
	if got := placed.BackupRoot("2026-11-14"); got != "backups/2026-11-14" {
		t.Fatalf("configured folder gave %q", got)
	}
	if got := placed.BackupFolderPath(); got != "backups" {
		t.Fatalf("folder path gave %q", got)
	}
}
