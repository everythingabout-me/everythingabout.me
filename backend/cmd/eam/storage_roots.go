package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
)

// rootIdentity is the part of a fileops.Root that names where an owner's bytes
// live. The driver-derived flags (CaseSensitive, RetainsVersions) are excluded on
// purpose: they describe the backend implementation, not the location of the tree,
// and they are not what an operator changes when moving storage.
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

// describe keeps credential-free identity visible to the operator: the connection
// digest already covers endpoint, bucket, region, prefixes and generation.
func (r rootIdentity) describe() string {
	return fmt.Sprintf("connection %q generation %d, content %q, system %q", r.ConnectionID, r.Generation, r.ContentRoot, r.SystemRoot)
}

// validateStorageRoots refuses a configuration that points at a different native
// tree than the one the database index describes.
//
// This is a startup gate, not a convenience warning. Every mismatch it reports
// would otherwise surface much later and in unrelated form: the index would keep
// listing files that no longer resolve to bytes, downloads would 404, and every
// write would be rejected with root_conflict from inside an operation transaction.
// A bucket, region, endpoint, prefix or generation change moves the bytes; that is
// a root migration, and the migration tools are the way through it.
func validateStorageRoots(ctx context.Context, db *store.DB, backend *storagebackend.Backend) error {
	if db == nil || backend == nil {
		return nil
	}
	pool := db.Pool()
	var present string
	if err := pool.QueryRow(ctx, `select coalesce(to_regclass('public.file_budgets')::text, '')`).Scan(&present); err != nil {
		return fmt.Errorf("validate storage roots: %w", err)
	}
	if present == "" {
		// The schema has not been migrated yet, so nothing is bound to a tree.
		return nil
	}
	budgets := map[string]rootIdentity{}
	rows, err := pool.Query(ctx, `select owner_id::text, root_binding from file_budgets order by owner_id`)
	if err != nil {
		return fmt.Errorf("validate storage roots: read file_budgets: %w", err)
	}
	for rows.Next() {
		var owner string
		var raw []byte
		if err := rows.Scan(&owner, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("validate storage roots: read file_budgets: %w", err)
		}
		var binding fileops.Root
		if err := json.Unmarshal(raw, &binding); err != nil {
			rows.Close()
			return fmt.Errorf("validate storage roots: decode root binding for owner %s: %w", owner, err)
		}
		budgets[owner] = identityOf(binding)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("validate storage roots: read file_budgets: %w", err)
	}
	heads := map[string]string{}
	if err := pool.QueryRow(ctx, `select coalesce(to_regclass('public.file_change_heads')::text, '')`).Scan(&present); err != nil {
		return fmt.Errorf("validate storage roots: %w", err)
	}
	if present != "" {
		headRows, err := pool.Query(ctx, `select owner_id::text, root_namespace from file_change_heads where root_namespace <> '' order by owner_id`)
		if err != nil {
			return fmt.Errorf("validate storage roots: read file_change_heads: %w", err)
		}
		for headRows.Next() {
			var owner, namespace string
			if err := headRows.Scan(&owner, &namespace); err != nil {
				headRows.Close()
				return fmt.Errorf("validate storage roots: read file_change_heads: %w", err)
			}
			heads[owner] = namespace
		}
		headRows.Close()
		if err := headRows.Err(); err != nil {
			return fmt.Errorf("validate storage roots: read file_change_heads: %w", err)
		}
	}
	return compareRootBindings(budgets, heads, func(owner string) (rootIdentity, error) {
		binding, err := backend.Binding(owner)
		if err != nil {
			return rootIdentity{}, err
		}
		return identityOf(fileops.Root{
			OwnerID:      binding.OwnerID,
			ConnectionID: binding.ConnectionID,
			Generation:   binding.Generation,
			ContentRoot:  binding.ContentRoot,
			SystemRoot:   binding.SystemRoot,
		}), nil
	})
}

// compareRootBindings is the pure half of the gate. expected derives the identity
// the current configuration produces for one owner.
func compareRootBindings(budgets map[string]rootIdentity, heads map[string]string, expected func(string) (rootIdentity, error)) error {
	owners := make([]string, 0, len(budgets)+len(heads))
	seen := make(map[string]bool, len(budgets)+len(heads))
	for owner := range budgets {
		seen[owner] = true
		owners = append(owners, owner)
	}
	for owner := range heads {
		if !seen[owner] {
			seen[owner] = true
			owners = append(owners, owner)
		}
	}
	sort.Strings(owners)
	var problems []string
	for _, owner := range owners {
		want, err := expected(owner)
		if err != nil {
			problems = append(problems, fmt.Sprintf("  owner %s\n    current configuration cannot bind this owner: %v", owner, err))
			continue
		}
		var findings []string
		if got, ok := budgets[owner]; ok && got != want {
			findings = append(findings, fmt.Sprintf("    file_budgets\n      index:      %s\n      configured: %s", got.describe(), want.describe()))
		}
		if got, ok := heads[owner]; ok && got != want.namespace() {
			findings = append(findings, fmt.Sprintf("    file_change_heads\n      index:      %q\n      configured: %q", got, want.namespace()))
		}
		if len(findings) > 0 {
			problems = append(problems, fmt.Sprintf("  owner %s\n%s", owner, strings.Join(findings, "\n")))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf(`the database index describes a different native tree than this configuration:

%s

COS_BUCKET, COS_REGION, the object prefix, the local roots and STORAGE_GENERATION all select bytes. Changing one moves the tree, so it is a root migration rather than a configuration change: the index would keep listing files that no longer resolve, and every write would be rejected with root_conflict.
Refusing to start against storage that the index does not describe.`, strings.Join(problems, "\n"))
}
