package connections

import (
	"context"
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

func (f *fakeStore) ByName(_ context.Context, name string) (Connection, bool, error) {
	for _, c := range f.rows {
		if c.Name == name {
			return c, true, nil
		}
	}
	return Connection{}, false, nil
}

func (f *fakeStore) OwnerConnection(_ context.Context, owner string) (string, bool, error) {
	name, ok := f.owner[owner]
	return name, ok, nil
}

func (f *fakeStore) Insert(_ context.Context, c Connection) error {
	f.rows = append(f.rows, c)
	return nil
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
