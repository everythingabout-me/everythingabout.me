package connections

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
)

// fakeStore is the durable half without a database: rows plus the per-owner pointer.
type fakeStore struct {
	rows  []Connection
	owner map[string]string
	// writes counts SetObjectPaths calls, so an idempotent second adoption can assert
	// that it did not rewrite the row.
	writes int
}

func newFakeStore(rows ...Connection) *fakeStore {
	return &fakeStore{rows: rows, owner: map[string]string{}}
}

func (f *fakeStore) Connections(context.Context) ([]Connection, error) { return f.rows, nil }

func (f *fakeStore) Authority(context.Context) (Connection, bool, error) {
	for _, c := range f.rows {
		if c.Role == RoleAuthority && c.Enabled {
			return c, true, nil
		}
	}
	return Connection{}, false, nil
}

func (f *fakeStore) ByName(_ context.Context, ownerID, name string) (Connection, bool, error) {
	for _, c := range f.rows {
		if c.Name == name && (c.OwnerID == ownerID || c.OwnerID == "") {
			return c, true, nil
		}
	}
	return Connection{}, false, nil
}

func (f *fakeStore) Visible(_ context.Context, ownerID string) ([]Connection, error) {
	var out []Connection
	for _, c := range f.rows {
		if c.OwnerID == ownerID || c.OwnerID == "" {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) OwnerConnection(_ context.Context, owner string) (string, bool, error) {
	name, ok := f.owner[owner]
	return name, ok, nil
}

func (f *fakeStore) Insert(_ context.Context, c Connection) error {
	f.rows = append(f.rows, c)
	return nil
}

func (f *fakeStore) SetObjectPaths(_ context.Context, id, admissionFile, spoolRoot string) error {
	for i := range f.rows {
		if f.rows[i].ID != id {
			continue
		}
		f.rows[i].Config.ObjectAdmissionFile = admissionFile
		f.rows[i].Config.ObjectSpoolRoot = spoolRoot
		f.writes++
		return nil
	}
	return fmt.Errorf("fake store: no connection row %q", id)
}

func (f *fakeStore) SetProbe(_ context.Context, id string, probe []byte) error {
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].LastProbe = probe
			return nil
		}
	}
	return fmt.Errorf("fake store: no connection row %q", id)
}

func (f *fakeStore) SetBackupFolder(_ context.Context, id, ownerID, folder string) error {
	for i := range f.rows {
		if f.rows[i].ID != id {
			continue
		}
		if f.rows[i].OwnerID != ownerID {
			return fmt.Errorf("%w: %s", ErrConnectionNotFound, id)
		}
		f.rows[i].Config.BackupFolder = folder
		return nil
	}
	return fmt.Errorf("%w: %s", ErrConnectionNotFound, id)
}

func (f *fakeStore) SetSecretReference(_ context.Context, id string, ref SecretReference) error {
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].Secret = Secret{}
			f.rows[i].SecretReference = ref
			return nil
		}
	}
	return fmt.Errorf("fake store: no connection row %q", id)
}

func localConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	content := filepath.Join(dir, "content")
	system := filepath.Join(dir, "system")
	for _, p := range []string{content, system} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return config.Config{Storage: config.Storage{
		Backend:             "local",
		ConnectionID:        "default",
		Generation:          1,
		MaxSinglePutBytes:   1024,
		ObjectAdmissionFile: filepath.Join(dir, "admission.json"),
		ObjectSpoolRoot:     filepath.Join(dir, "spool"),
		Local:               config.Local{ContentRoot: content, SystemRoot: system, CaseSensitive: true},
	}}
}

// A fresh database must bind owners exactly the way the environment-only backend
// did, or every deployed owner would be rebound on upgrade.
func TestSeededConnectionMatchesEnvironmentBinding(t *testing.T) {
	cfg := localConfig(t)
	envBackend, err := storagebackend.New(&cfg)
	if err != nil {
		t.Fatalf("environment backend: %v", err)
	}
	defer envBackend.Close()

	row, err := FromConfig(cfg)
	if err != nil {
		t.Fatalf("connection from config: %v", err)
	}
	resolver := NewResolver(cfg, newFakeStore(row))
	defer resolver.Close()

	for _, owner := range []string{"17d2b1f0-6e28-4d7e-8f1e-1e0f2a5b9c31", "b58d2b40-1f6c-4d3b-9a5e-2f7c8d9e0a12"} {
		want, err := envBackend.Binding(owner)
		if err != nil {
			t.Fatalf("environment binding: %v", err)
		}
		got, err := resolver.Binding(owner)
		if err != nil {
			t.Fatalf("resolved binding: %v", err)
		}
		if got.ConnectionID != want.ConnectionID || got.ContentRoot != want.ContentRoot || got.SystemRoot != want.SystemRoot || got.Generation != want.Generation {
			t.Fatalf("owner %s: resolved binding %+v differs from environment binding %+v", owner, got, want)
		}
	}
}

// Rotating a credential is the same tree; moving it is not. Identity must encode
// exactly that, because it is the row's uniqueness rule.
func TestIdentityIgnoresCredentialsAndNames(t *testing.T) {
	cfg := localConfig(t)
	row, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	renamed := row
	renamed.Name = "another-label"
	renamed.Secret.COS.SecretKey = "rotated"
	got, err := renamed.ComputeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if got != row.Identity {
		t.Fatalf("identity changes with name or credential: %s vs %s", got, row.Identity)
	}
	moved := row
	moved.Config.Local.ContentRoot += "-moved"
	got, err = moved.ComputeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if got == row.Identity {
		t.Fatal("identity did not change when the content root did")
	}
}

// The binding an owner already has decides the connection, so the environment and
// the table cannot disagree about an existing owner.
func TestResolverFollowsTheOwnersBinding(t *testing.T) {
	cfg := localConfig(t)
	authority, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backup := Connection{
		Name: "backup", Backend: "local", Role: RoleBackup, Generation: 1, Enabled: true,
		Config: Spec{Local: LocalSpec{ContentRoot: filepath.Join(cfg.Storage.Local.ContentRoot, "b"), SystemRoot: filepath.Join(cfg.Storage.Local.SystemRoot, "b")}},
	}
	backup.Identity = mustIdentity(t, backup)
	store := newFakeStore(authority, backup)
	store.owner["owner-a"] = "default"
	store.owner["owner-b"] = "backup"
	resolver := NewResolver(cfg, store)
	defer resolver.Close()

	if _, err := resolver.Binding("owner-a"); err != nil {
		t.Fatalf("owner-a: %v", err)
	}
	// A backup connection can never back an owner's tree, whatever the binding says.
	if _, err := resolver.Binding("owner-b"); err == nil {
		t.Fatal("a backup connection was accepted as an owner authority")
	}
}

func TestResolverRejectsMissingAndDisabledConnections(t *testing.T) {
	cfg := localConfig(t)
	authority, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	missing := newFakeStore(authority)
	missing.owner["owner-a"] = "gone"
	resolver := NewResolver(cfg, missing)
	if _, err := resolver.Binding("owner-a"); err == nil {
		t.Fatal("a binding naming a deleted connection was accepted")
	}
	resolver.Close()

	disabled := authority
	disabled.Enabled = false
	resolver = NewResolver(cfg, newFakeStore(disabled))
	if _, err := resolver.Binding("owner-c"); err == nil {
		t.Fatal("a disabled connection served an owner")
	}
	resolver.Close()
}

// Seed is what keeps an existing deployment's bindings resolving: it runs once and
// never overwrites a row an operator already has.
func TestSeedRunsOnlyWhenNoAuthorityExists(t *testing.T) {
	cfg := localConfig(t)
	store := newFakeStore()
	resolver := NewResolver(cfg, store)
	defer resolver.Close()
	if err := resolver.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("seed inserted %d rows, want 1", len(store.rows))
	}
	seeded := store.rows[0]
	if err := resolver.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("a second seed inserted another row: %d", len(store.rows))
	}
	if _, err := resolver.Binding("owner-a"); err != nil {
		t.Fatalf("seeded connection does not resolve an owner: %v", err)
	}
	if seeded.Name != cfg.Storage.ConnectionID {
		t.Fatalf("seeded name %q, want the configured %q", seeded.Name, cfg.Storage.ConnectionID)
	}
}

func mustIdentity(t *testing.T, c Connection) string {
	t.Helper()
	id, err := c.ComputeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

var _ = storage.TransferRelay

// An owner-scoped row's tree is its prefix, so the account is not part of where the
// tree lives: two rows that differ only by account would collide in one location, and
// the identity has to say so instead of hiding it behind an account-shaped digest.
func TestIdentityIgnoresTheAccountButNotTheLocation(t *testing.T) {
	row := Connection{
		Name: "nas", Backend: "s3", Role: RoleBackup, Generation: 1, Enabled: true,
		Config: Spec{S3: S3Spec{Endpoint: "https://nas.invalid", Region: "us-east-1", Bucket: "backup"}},
	}
	mine := row
	mine.OwnerID = "owner-a"
	theirs := row
	theirs.OwnerID = "owner-b"
	if mustIdentity(t, mine) != mustIdentity(t, theirs) {
		t.Fatal("identity still encodes the account, so two rows could claim one location")
	}
	moved := mine
	moved.Config.S3.Bucket = "other"
	if mustIdentity(t, moved) == mustIdentity(t, mine) {
		t.Fatal("identity did not change when the location did")
	}
}

// The account API may name a COS bucket or an S3 endpoint; a local row would be an
// arbitrary host path, and authority is not an account's to grant. The operator CLI is
// not bound by this, because it already has host access.
func TestValidateAccountAddedStaysStricterThanValidate(t *testing.T) {
	ownerRow := func(backend string, role string) Connection {
		c := Connection{OwnerID: "owner-a", Name: "mine", Backend: backend, Role: role, Generation: 1, Enabled: true}
		switch backend {
		case "cos":
			c.Config.COS = COSSpec{Bucket: "b-125", Region: "ap-hongkong"}
			c.Secret.COS = COSCredential{SecretID: "id", SecretKey: "key"}
		case "s3":
			c.Config.S3 = S3Spec{Endpoint: "https://nas.invalid", Region: "us-east-1", Bucket: "b"}
			c.Secret.S3 = S3Credential{AccessKeyID: "id", SecretAccessKey: "key"}
		}
		return c
	}
	if err := ownerRow("cos", RoleBackup).Validate(); err != nil {
		t.Fatalf("operator-added owner row rejected by Validate: %v", err)
	}
	if err := ownerRow("s3", RoleBackup).Validate(); err != nil {
		t.Fatalf("operator-added owner s3 row rejected by Validate: %v", err)
	}
	if err := ownerRow("cos", RoleBackup).ValidateAccountAdded(); err != nil {
		t.Fatalf("account cos backup row rejected: %v", err)
	}
	// Attaching your own NAS is the feature: naming an endpoint is the account's call,
	// and the probe decides whether that endpoint can hold a tree.
	if err := ownerRow("s3", RoleBackup).ValidateAccountAdded(); err != nil {
		t.Fatalf("account s3 backup row rejected: %v", err)
	}
	broken := ownerRow("s3", RoleBackup)
	broken.Config.S3.Endpoint = "nas.invalid:9000"
	if err := broken.ValidateAccountAdded(); err == nil {
		t.Error("an endpoint without a scheme was accepted")
	}
	for _, test := range []struct {
		what string
		row  Connection
	}{
		{"authority role", ownerRow("cos", RoleAuthority)},
		{"shared row", func() Connection { c := ownerRow("cos", RoleBackup); c.OwnerID = ""; return c }()},
	} {
		if err := test.row.ValidateAccountAdded(); err == nil {
			t.Errorf("%s accepted from the account path", test.what)
		}
	}
}

func TestS3EndpointHostRejectsAnythingButABareServiceURL(t *testing.T) {
	host, err := S3EndpointHost("https://NAS.example.com:9000")
	if err != nil || host != "nas.example.com" {
		t.Fatalf("host %q err %v", host, err)
	}
	if _, err := S3EndpointHost("http://127.0.0.1:9000"); err != nil {
		t.Fatalf("loopback endpoint is the operator's call when listed: %v", err)
	}
	for _, endpoint := range []string{
		"nas.example.com:9000", "ftp://nas.example.com", "https://user:pass@nas.example.com",
		"https://nas.example.com/bucket", "https://nas.example.com?x=1", "https://nas.example.com#f",
	} {
		if _, err := S3EndpointHost(endpoint); err == nil {
			t.Errorf("%q was accepted", endpoint)
		}
	}
}

// rootAtBucket is for a tree only one account may use: a shared connection at the
// bucket root would place every account's tree in the same place.
func TestValidateRejectsRootAtBucketWhereItCannotWork(t *testing.T) {
	cos := func(owner string) Connection {
		c := Connection{OwnerID: owner, Name: "mine", Backend: "cos", Role: RoleBackup, Generation: 1, Enabled: true}
		c.Config.COS = COSSpec{Bucket: "b-125", Region: "ap-hongkong"}
		c.Secret.COS = COSCredential{SecretID: "id", SecretKey: "key"}
		c.Config.RootAtBucket = true
		return c
	}
	if err := cos("owner-a").Validate(); err != nil {
		t.Fatalf("owner-scoped bucket-root row rejected: %v", err)
	}
	if err := cos("").Validate(); err == nil {
		t.Fatal("a shared row was allowed to root at the bucket")
	}
	local := Connection{OwnerID: "owner-a", Name: "mine", Backend: "local", Role: RoleBackup, Generation: 1, Enabled: true,
		Config: Spec{Local: LocalSpec{ContentRoot: "/srv/a", SystemRoot: "/srv/b"}, RootAtBucket: true}}
	if err := local.Validate(); err == nil {
		t.Fatal("a local row was allowed to root at the bucket")
	}
}

// The operator-facing derivation must agree with the binding the serving path
// persists, or a rebind could record a root the file API never serves.
func TestBindingForMatchesTheServingBinding(t *testing.T) {
	cfg := localConfig(t)
	authority, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeStore(authority)
	resolver := NewResolver(cfg, store)
	defer resolver.Close()
	served, err := resolver.Binding("owner-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", authority.Name} {
		pure, err := resolver.BindingFor(context.Background(), "owner-a", name)
		if err != nil {
			t.Fatalf("BindingFor(%q): %v", name, err)
		}
		if pure != served {
			t.Fatalf("BindingFor(%q) = %+v, serving binding = %+v", name, pure, served)
		}
	}
	if _, err := resolver.BindingFor(context.Background(), "owner-a", "missing"); err == nil {
		t.Fatal("a missing connection was derived as a binding")
	}
}
