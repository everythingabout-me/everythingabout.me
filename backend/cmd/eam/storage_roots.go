package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"everythingabout.me/backend/internal/filechanges"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
)

// rootIdentity is the part of a fileops.Root that names where an owner's bytes
// live. The driver-derived flags (CaseSensitive, RetainsVersions) are excluded on
// purpose: they describe the backend implementation, not the location of the tree.
type rootIdentity struct {
	OwnerID      string
	ConnectionID string
	Generation   int64
	ContentRoot  string
	SystemRoot   string
}

func identityOf(root fileops.Root) rootIdentity {
	return rootIdentity{
		OwnerID:      root.OwnerID,
		ConnectionID: root.ConnectionID,
		Generation:   root.Generation,
		ContentRoot:  root.ContentRoot,
		SystemRoot:   root.SystemRoot,
	}
}

func (r rootIdentity) asRoot() fileops.Root {
	return fileops.Root{OwnerID: r.OwnerID, ConnectionID: r.ConnectionID, Generation: r.Generation, ContentRoot: r.ContentRoot, SystemRoot: r.SystemRoot}
}

// namespace is the change-stream identity the same tree is bound to.
func (r rootIdentity) namespace() string { return r.asRoot().Namespace() }

func (r rootIdentity) describe() string {
	return fmt.Sprintf("connection %q generation %d, content %q, system %q", r.ConnectionID, r.Generation, r.ContentRoot, r.SystemRoot)
}

// rootStore is the durable half of a derived view: the tree each owner was last
// bound to, and the change stream bound to that same tree.
type rootStore interface {
	Budgets(context.Context) (map[string]fileops.Root, error)
	ChangeNamespaces(context.Context) (map[string]string, error)
	AdoptBinding(context.Context, string, fileops.Root) error
	ResetChangeRoot(context.Context, string, string) (bool, error)
}

type postgresRootStore struct{ db *store.DB }

func newRootStore(db *store.DB) rootStore { return postgresRootStore{db: db} }

func (p postgresRootStore) Budgets(ctx context.Context) (map[string]fileops.Root, error) {
	budgets := map[string]fileops.Root{}
	rows, err := p.db.Pool().Query(ctx, `select owner_id::text, root_binding from file_budgets order by owner_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner string
		var raw []byte
		if err := rows.Scan(&owner, &raw); err != nil {
			return nil, err
		}
		var binding fileops.Root
		if err := json.Unmarshal(raw, &binding); err != nil {
			return nil, fmt.Errorf("decode root binding for owner %s: %w", owner, err)
		}
		budgets[owner] = binding
	}
	return budgets, rows.Err()
}

func (p postgresRootStore) ChangeNamespaces(ctx context.Context) (map[string]string, error) {
	heads := map[string]string{}
	var present string
	if err := p.db.Pool().QueryRow(ctx, `select coalesce(to_regclass('public.file_change_heads')::text, '')`).Scan(&present); err != nil {
		return nil, err
	}
	if present == "" {
		return heads, nil
	}
	rows, err := p.db.Pool().Query(ctx, `select owner_id::text, root_namespace from file_change_heads where root_namespace <> '' order by owner_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, namespace string
		if err := rows.Scan(&owner, &namespace); err != nil {
			return nil, err
		}
		heads[owner] = namespace
	}
	return heads, rows.Err()
}

// AdoptBinding rewrites only the identity fields, keeping the driver-derived flags
// the row already carries so a later comparison still sees the same tree shape.
func (p postgresRootStore) AdoptBinding(ctx context.Context, owner string, binding fileops.Root) error {
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	_, err = p.db.Pool().Exec(ctx, `update file_budgets set root_binding=$2 where owner_id=$1::uuid`, owner, data)
	return err
}

// ResetChangeRoot rotates the change stream onto the new tree. A busy root is not
// an error here: the caller keeps serving and the old namespace stays until the
// pending operations finish.
func (p postgresRootStore) ResetChangeRoot(ctx context.Context, owner, namespace string) (bool, error) {
	tx, err := p.db.Pool().Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := filechanges.ResetRoot(ctx, tx, owner, namespace); err != nil {
		if err == filechanges.ErrRootBusy {
			return false, nil
		}
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// adoptStorageRoots makes the derived view follow the configured tree.
//
// The native tree decides what exists; configuration decides which tree this
// process serves; the database index and the change stream are derived views of
// it. When configuration selects a different tree, the stored binding is stale by
// definition, so adopt the configured one instead of refusing to start. This is
// deliberately cheap: a couple of statements per changed owner and no native
// enumeration, because deployments have many owners and scanning them at startup
// is not affordable.
func adoptStorageRoots(ctx context.Context, db *store.DB, backend *storagebackend.Backend) error {
	if db == nil || backend == nil {
		return nil
	}
	var present string
	if err := db.Pool().QueryRow(ctx, `select coalesce(to_regclass('public.file_budgets')::text, '')`).Scan(&present); err != nil {
		return fmt.Errorf("adopt storage roots: %w", err)
	}
	if present == "" {
		// Nothing is bound yet: a fresh, unmigrated database has no derived view.
		return nil
	}
	return adoptRoots(ctx, newRootStore(db), func(owner string) (fileops.Root, error) {
		return bindingRoot(backend, owner)
	})
}

// bindingRoot derives the tree the current configuration selects for one owner.
func bindingRoot(backend *storagebackend.Backend, owner string) (fileops.Root, error) {
	binding, err := backend.Binding(owner)
	if err != nil {
		return fileops.Root{}, err
	}
	return fileops.Root{
		OwnerID:      binding.OwnerID,
		ConnectionID: binding.ConnectionID,
		Generation:   binding.Generation,
		ContentRoot:  binding.ContentRoot,
		SystemRoot:   binding.SystemRoot,
	}, nil
}

func adoptRoots(ctx context.Context, store rootStore, expected func(string) (fileops.Root, error)) error {
	budgets, err := store.Budgets(ctx)
	if err != nil {
		return fmt.Errorf("adopt storage roots: read budgets: %w", err)
	}
	heads, err := store.ChangeNamespaces(ctx)
	if err != nil {
		return fmt.Errorf("adopt storage roots: read change namespaces: %w", err)
	}
	for _, owner := range rootOwners(budgets, heads) {
		want, err := expected(owner)
		if err != nil {
			slog.Warn("cannot derive a storage root for owner; leaving the stored binding", "owner", owner, "error", err)
			continue
		}
		if stored, ok := budgets[owner]; ok && identityOf(stored) != identityOf(want) {
			adopted := stored
			adopted.ConnectionID = want.ConnectionID
			adopted.Generation = want.Generation
			adopted.ContentRoot = want.ContentRoot
			adopted.SystemRoot = want.SystemRoot
			if err := store.AdoptBinding(ctx, owner, adopted); err != nil {
				return fmt.Errorf("adopt storage roots: update budget for %s: %w", owner, err)
			}
			slog.Warn("configuration selects a different storage root; adopted it",
				"owner", owner,
				"previous_connection", stored.ConnectionID,
				"configured_connection", want.ConnectionID,
				"note", "the index and quota counters still describe the previous tree; run `eam reconcile --owner "+owner+"` to refresh the index")
		}
		if stored, ok := heads[owner]; ok && stored != want.Namespace() {
			done, err := store.ResetChangeRoot(ctx, owner, want.Namespace())
			if err != nil {
				return fmt.Errorf("adopt storage roots: reset change stream for %s: %w", owner, err)
			}
			if !done {
				slog.Warn("change stream still belongs to the previous tree; pending work blocks the reset", "owner", owner)
				continue
			}
			slog.Warn("change stream reset onto the configured storage root", "owner", owner)
		}
	}
	return nil
}

func rootOwners(budgets map[string]fileops.Root, heads map[string]string) []string {
	owners := make([]string, 0, len(budgets)+len(heads))
	for owner := range budgets {
		owners = append(owners, owner)
	}
	for owner := range heads {
		if _, seen := budgets[owner]; !seen {
			owners = append(owners, owner)
		}
	}
	sort.Strings(owners)
	return owners
}

// inspectStorageRoots is the read-only form used by `eam doctor`: it reports the
// same disagreement the server would adopt, without changing anything.
func inspectStorageRoots(ctx context.Context, db *store.DB, backend *storagebackend.Backend) error {
	if db == nil || backend == nil {
		return nil
	}
	store := newRootStore(db)
	budgets, err := store.Budgets(ctx)
	if err != nil {
		return fmt.Errorf("inspect storage roots: %w", err)
	}
	heads, err := store.ChangeNamespaces(ctx)
	if err != nil {
		return fmt.Errorf("inspect storage roots: %w", err)
	}
	problems := rootProblems(budgets, heads, func(owner string) (fileops.Root, error) {
		return bindingRoot(backend, owner)
	})
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("configuration selects a different tree than the index records:\n%s\nthe server adopts the configured tree at startup; this check only reports it", strings.Join(problems, "\n"))
}

// rootProblems describes every owner whose derived view belongs to another tree.
func rootProblems(budgets map[string]fileops.Root, heads map[string]string, expected func(string) (fileops.Root, error)) []string {
	var problems []string
	for _, owner := range rootOwners(budgets, heads) {
		want, err := expected(owner)
		if err != nil {
			problems = append(problems, fmt.Sprintf("  owner %s\n    current configuration cannot bind this owner: %v", owner, err))
			continue
		}
		wantIdentity := identityOf(want)
		var findings []string
		if stored, ok := budgets[owner]; ok && identityOf(stored) != wantIdentity {
			findings = append(findings, fmt.Sprintf("    file_budgets\n      index:      %s\n      configured: %s", identityOf(stored).describe(), wantIdentity.describe()))
		}
		if stored, ok := heads[owner]; ok && stored != wantIdentity.namespace() {
			findings = append(findings, fmt.Sprintf("    file_change_heads\n      index:      %q\n      configured: %q", stored, wantIdentity.namespace()))
		}
		if len(findings) > 0 {
			problems = append(problems, fmt.Sprintf("  owner %s\n%s", owner, strings.Join(findings, "\n")))
		}
	}
	return problems
}
