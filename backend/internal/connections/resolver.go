package connections

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
)

// Resolver answers the only two questions the rest of the service asks about
// storage: which driver serves this owner, and where the bytes actually live. It
// implements the API's OwnerStorageFactory, so no HTTP handler learns that
// connections exist.
//
// An owner's binding is read once and cached: changing it is a rebinding, which
// already requires quiescence and a change-stream reset, so it takes effect on
// restart rather than mid-flight.
type Resolver struct {
	base  config.Config
	store Store

	mu      sync.Mutex
	entries map[string]*entry
	owners  map[string]string
	closed  bool
}

type entry struct {
	conn    Connection
	backend *storagebackend.Backend
}

func NewResolver(base config.Config, store Store) *Resolver {
	return &Resolver{base: base, store: store, entries: map[string]*entry{}, owners: map[string]string{}}
}

// Seed gives a fresh database the one authority connection the environment still
// describes, so an existing deployment's bindings keep resolving. The row stores a
// reference to the environment's variable names, not their values. It is a no-op
// once an enabled authority row exists, which is why a repeated start is harmless.
func (r *Resolver) Seed(ctx context.Context) error {
	if _, ok, err := r.store.Authority(ctx); err != nil {
		return err
	} else if ok {
		return nil
	}
	c, err := FromConfig(r.base)
	if err != nil {
		return fmt.Errorf("seed the first storage connection from the environment: %w", err)
	}
	if err := r.store.Insert(ctx, c); err != nil {
		return fmt.Errorf("seed storage connection %q: %w", c.Name, err)
	}
	slog.Info("seeded the first storage connection from configuration", "name", c.Name, "backend", c.Backend, "identity", c.Identity)
	return nil
}

// AdoptEnvironmentSecret converts the environment-seeded row from an inline
// credential copy to a reference, once per database. It runs before owners are
// bound so a disagreement stops the start instead of serving a row whose
// credentials nobody can explain.
func (r *Resolver) AdoptEnvironmentSecret(ctx context.Context) error {
	return AdoptEnvironmentSecret(ctx, r.store, r.base, os.LookupEnv)
}

// Open implements api.OwnerStorageFactory.
func (r *Resolver) Open(ctx context.Context, ownerID string) (storage.Driver, error) {
	e, err := r.entryFor(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	return e.backend.Open(ctx, ownerID)
}

// Binding implements the optional factory capability the file API uses to derive
// its root identity and content namespace. The value comes from the driver binding
// itself, so it is the same value the mutation layer persists.
func (r *Resolver) Binding(ownerID string) (storage.Config, error) {
	e, err := r.entryFor(context.Background(), ownerID)
	if err != nil {
		return storage.Config{}, err
	}
	return e.backend.Binding(ownerID)
}

// OpenConnection opens a named connection for an owner without going through the
// owner's binding. Backup runs use it to reach a destination the owner does not
// live on.
func (r *Resolver) OpenConnection(ctx context.Context, ownerID, name string) (storage.Driver, error) {
	e, err := r.entryNamed(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	return e.backend.Open(ctx, ownerID)
}

// Connection reports the row an owner resolves to. Maintenance commands use it to
// explain a decision instead of repeating the resolution rules.
func (r *Resolver) Connection(ctx context.Context, ownerID string) (Connection, error) {
	e, err := r.entryFor(ctx, ownerID)
	if err != nil {
		return Connection{}, err
	}
	return e.conn, nil
}

func (r *Resolver) entryFor(ctx context.Context, ownerID string) (*entry, error) {
	r.mu.Lock()
	closed := r.closed
	name, known := r.owners[ownerID]
	r.mu.Unlock()
	if closed {
		return nil, storage.NewError("closed", "", nil)
	}
	if known {
		return r.entryNamed(ctx, ownerID, name)
	}
	name, err := r.ownerConnection(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	e, err := r.entryNamed(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	if e.conn.Role != RoleAuthority {
		return nil, fmt.Errorf("owner %s is bound to storage connection %q, which has role %q: only an authority connection can back an owner's tree",
			ownerID, e.conn.Name, e.conn.Role)
	}
	r.mu.Lock()
	r.owners[ownerID] = name
	r.mu.Unlock()
	return e, nil
}

// ownerConnection resolves the connection name for one owner: the binding it
// already has, otherwise the authority row.
func (r *Resolver) ownerConnection(ctx context.Context, ownerID string) (string, error) {
	name, ok, err := r.store.OwnerConnection(ctx, ownerID)
	if err != nil {
		return "", err
	}
	if ok {
		return name, nil
	}
	authority, found, err := r.store.Authority(ctx)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("owner %s has no storage binding and no authority connection exists: add one with `eam storage-connection add` or restart with storage configuration set", ownerID)
	}
	return authority.Name, nil
}

func (r *Resolver) entryNamed(ctx context.Context, ownerID, name string) (*entry, error) {
	cacheKey := ownerID + "\x00" + name
	r.mu.Lock()
	if e, ok := r.entries[cacheKey]; ok {
		r.mu.Unlock()
		return e, nil
	}
	r.mu.Unlock()

	c, ok, err := r.store.ByName(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("storage connection %q: %w (an owner binding still names it)", name, ErrNotFound)
	}
	if !c.Enabled {
		return nil, fmt.Errorf("storage connection %q is disabled; enable it before serving owners bound to it", name)
	}
	// A row with an environment reference is resolved here, when the connection
	// starts serving, not when it was read: the environment is the owner of the
	// value, so a missing variable names itself and never falls back to an inline
	// copy.
	effective, err := c.ApplyEnvironment(r.base)
	if err != nil {
		return nil, err
	}
	backend, err := storagebackend.NewForServing(&effective)
	if err != nil {
		return nil, fmt.Errorf("storage connection %q: %w", name, err)
	}
	if problem := backend.ObjectWritesProblem(); problem != nil {
		slog.Warn("coordinated object writes are disabled for a storage connection", "connection", name, "reason", problem)
	}
	e := &entry{conn: c, backend: backend}
	r.mu.Lock()
	if existing, ok := r.entries[cacheKey]; ok {
		r.mu.Unlock()
		backend.Close()
		return existing, nil
	}
	r.entries[cacheKey] = e
	r.mu.Unlock()
	return e, nil
}

func (r *Resolver) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	entries := r.entries
	r.entries = map[string]*entry{}
	r.mu.Unlock()
	var first error
	for _, e := range entries {
		if err := e.backend.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
