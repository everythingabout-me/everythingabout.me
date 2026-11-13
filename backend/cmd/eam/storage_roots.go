package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/filechanges"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

// rootBinding is one owner's durable root identity: the fileops.Root the index is
// bound to, plus the provider coordinates that identity was computed from.
//
// The coordinates live beside the Root fields in the same JSON document. fileops
// ignores unknown keys, so this is still a derived record and does not change what
// a Root means or how it is compared. They exist for one reason: a refused start
// has to name the field with both values, and a connection digest cannot be turned
// back into the bucket and region it was computed from.
type rootBinding struct {
	fileops.Root
	Bucket string `json:",omitempty"`
	Region string `json:",omitempty"`
}

// hasCoordinates reports whether this binding recorded where its digest came from.
// A row written before coordinates were stored has neither, and ConnectionID still
// covers both; a local root legitimately has neither.
func (b rootBinding) hasCoordinates() bool { return b.Bucket != "" || b.Region != "" }

func (b rootBinding) namespace() string { return b.Root.Namespace() }

// connectionName is the human-readable half of a binding digest. Connection names
// cannot contain ':' (see connections.ValidateName), so the last colon splits the
// name from the digest.
func connectionName(connectionID string) string {
	if i := strings.LastIndex(connectionID, ":"); i > 0 {
		return connectionID[:i]
	}
	return connectionID
}

// rootDifference is one disagreement between the index and the configuration. Both
// values are provider-free text safe to show an operator.
type rootDifference struct {
	Field      string
	Bound      string
	Configured string
}

func (d rootDifference) String() string {
	return fmt.Sprintf("    %s: bound %q, configured %q", d.Field, d.Bound, d.Configured)
}

// compareRootBinding is the value comparison behind the startup refusal. It reports
// every identity field whose bound and configured values disagree, so an operator
// sees all of them at once instead of fixing one and meeting the next on the next
// attempt.
//
// The connection digest already covers bucket, region, backend, endpoint and path
// style. Bucket and region are compared separately only when the binding recorded
// them: an older binding has no coordinates, and saying "bound \"\", configured
// \"bucket\"" for a binding that simply predates the record would be a false
// mismatch. The digest still catches that case.
func compareRootBinding(bound, configured rootBinding) []rootDifference {
	var diffs []rootDifference
	if bound.hasCoordinates() || configured.Bucket == "" {
		if bound.Bucket != configured.Bucket {
			diffs = append(diffs, rootDifference{Field: "bucket", Bound: bound.Bucket, Configured: configured.Bucket})
		}
	}
	if bound.hasCoordinates() || configured.Region == "" {
		if bound.Region != configured.Region {
			diffs = append(diffs, rootDifference{Field: "region", Bound: bound.Region, Configured: configured.Region})
		}
	}
	if bound.ContentRoot != configured.ContentRoot {
		diffs = append(diffs, rootDifference{Field: "content prefix", Bound: bound.ContentRoot, Configured: configured.ContentRoot})
	}
	if bound.SystemRoot != configured.SystemRoot {
		diffs = append(diffs, rootDifference{Field: "system prefix", Bound: bound.SystemRoot, Configured: configured.SystemRoot})
	}
	if bound.Generation != configured.Generation {
		diffs = append(diffs, rootDifference{Field: "generation", Bound: fmt.Sprint(bound.Generation), Configured: fmt.Sprint(configured.Generation)})
	}
	if bound.ConnectionID != configured.ConnectionID {
		diffs = append(diffs, rootDifference{Field: "connection identity", Bound: bound.ConnectionID, Configured: configured.ConnectionID})
	}
	return diffs
}

// rootMismatch is one owner whose index or change stream belongs to another tree.
type rootMismatch struct {
	Owner      string
	Bound      rootBinding
	Configured rootBinding
	Diffs      []rootDifference
	// Resolve explains why the current configuration could not even bind the owner.
	Resolve error
}

// resolveCommand is the exact rebind the operator should run for this owner. It is
// included in the refusal so the error ends with an action, not an explanation.
func (m rootMismatch) resolveCommand() string {
	if m.Bound.ConnectionID == "" || m.Configured.ConnectionID == "" {
		return ""
	}
	from := connectionName(m.Bound.ConnectionID)
	to := connectionName(m.Configured.ConnectionID)
	return fmt.Sprintf("eam storage-rebind --owner-id %s --from %s --to %s --writers-quiesced", m.Owner, from, to)
}

func (m rootMismatch) String() string {
	if m.Resolve != nil {
		return fmt.Sprintf("  owner %s\n    the current configuration cannot bind this owner: %v", m.Owner, m.Resolve)
	}
	var lines []string
	for _, d := range m.Diffs {
		lines = append(lines, d.String())
	}
	return fmt.Sprintf("  owner %s\n%s", m.Owner, strings.Join(lines, "\n"))
}

// rootMismatchError refuses a start whose index describes a different tree than the
// configured root. The native tree is authoritative, but the service does not decide
// that a newly configured root is the intended one: adopting it would leave the old
// tree unreachable and unreported, so the operator has to move the bytes and say so.
type rootMismatchError struct {
	Mismatches []rootMismatch
}

func (e *rootMismatchError) Error() string {
	var b strings.Builder
	b.WriteString("refusing to start: the configured storage root is not the one this database is bound to\n\n")
	for _, m := range e.Mismatches {
		b.WriteString(m.String())
		b.WriteString("\n")
		switch {
		case m.Resolve != nil:
			b.WriteString("    fix the configuration cause above before any rebind\n")
		case m.resolveCommand() != "":
			b.WriteString("    re-point the index after the bytes are in place:\n      ")
			b.WriteString(m.resolveCommand())
			b.WriteString("\n")
		default:
			b.WriteString("    no file_budgets row exists, so there is no binding to re-point\n")
		}
	}
	b.WriteString(`
The service never adopts a changed root, and it never reads, writes or deletes the
previous tree. Adopting one would leave that tree unreachable and unreported while
the index described the new root: the files a person sees would stop resolving to
bytes, and nothing would say which copy was real.

Two actions are acceptable:
  1. Bring the data across: copy the tree from the previous root to the configured
     root, verify it, stop all application and external writers, settle accepted
     operations, then run the eam storage-rebind command listed for that owner
     above (fix a configuration cause first, if one was reported). It updates the
     binding and rotates the change-stream namespace; it does not touch a bucket.
  2. Discard this derived index: recreate an empty database, run eam migrate, then
     import the configured tree again with
       eam reconcile --owner UUID
     for each owner.`)
	return b.String()
}

// rootStore is the durable half of the root binding: the per-owner binding, the
// change-stream namespace bound to the same tree, and the explicit write paths an
// operator command may use. Nothing here lists, reads or deletes native content.
type rootStore interface {
	Bindings(context.Context) (map[string]rootBinding, error)
	ChangeNamespaces(context.Context) (map[string]string, error)
	BackfillCoordinates(context.Context, string, string, string) error
	PendingWork(context.Context, string) (pendingWork, error)
	Rebind(context.Context, string, rootBinding, string) (filechanges.Head, error)
}

// pendingWork is the work that makes a rebind unsafe: an accepted operation may
// still publish, and a guard fences a path it is about to change.
type pendingWork struct {
	Operations int
	Guards     int
}

func (p pendingWork) busy() bool { return p.Operations > 0 || p.Guards > 0 }

type postgresRootStore struct{ db *store.DB }

func newRootStore(db *store.DB) rootStore { return postgresRootStore{db: db} }

func (p postgresRootStore) Bindings(ctx context.Context) (map[string]rootBinding, error) {
	bindings := map[string]rootBinding{}
	var present string
	if err := p.db.Pool().QueryRow(ctx, `select coalesce(to_regclass('public.file_budgets')::text, '')`).Scan(&present); err != nil {
		return nil, err
	}
	if present == "" {
		// Nothing is bound yet: a fresh, unmigrated database has no derived view.
		return bindings, nil
	}
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
		var binding rootBinding
		if err := json.Unmarshal(raw, &binding); err != nil {
			return nil, fmt.Errorf("decode root binding for owner %s: %w", owner, err)
		}
		bindings[owner] = binding
	}
	return bindings, rows.Err()
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

// BackfillCoordinates records where an existing binding's digest came from. It is a
// derived-record write, never a native one: a binding written before coordinates
// were stored still matches its tree, and recording them now is what lets a later
// change be reported field by field.
func (p postgresRootStore) BackfillCoordinates(ctx context.Context, owner, bucket, region string) error {
	if bucket == "" && region == "" {
		return nil
	}
	_, err := p.db.Pool().Exec(ctx, `update file_budgets set root_binding =
 jsonb_set(jsonb_set(root_binding, '{Bucket}', to_jsonb($2::text), true), '{Region}', to_jsonb($3::text), true)
 where owner_id=$1::uuid`, owner, bucket, region)
	return err
}

func (p postgresRootStore) PendingWork(ctx context.Context, owner string) (pendingWork, error) {
	var work pendingWork
	err := p.db.Pool().QueryRow(ctx, `select
 (select count(*) from file_operations where owner_id=$1::uuid and state not in ('succeeded','failed','cancelled')),
 (select count(*) from file_operation_guards where owner_id=$1::uuid)`, owner).Scan(&work.Operations, &work.Guards)
	return work, err
}

// Rebind writes the configured binding and rotates the change stream in one
// transaction. It reads no provider state and writes none: only these two derived
// records change, so the bytes the operator already moved are the only copy this
// command can be looking at.
func (p postgresRootStore) Rebind(ctx context.Context, owner string, binding rootBinding, namespace string) (filechanges.Head, error) {
	data, err := json.Marshal(binding)
	if err != nil {
		return filechanges.Head{}, err
	}
	tx, err := p.db.Pool().Begin(ctx)
	if err != nil {
		return filechanges.Head{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, `update file_budgets set root_binding=$2 where owner_id=$1::uuid`, owner, data)
	if err != nil {
		return filechanges.Head{}, err
	}
	if tag.RowsAffected() != 1 {
		return filechanges.Head{}, fmt.Errorf("owner %s has no file_budgets row, so there is no binding to re-point", owner)
	}
	head, err := filechanges.ResetRoot(ctx, tx, owner, namespace)
	if err != nil {
		return filechanges.Head{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return filechanges.Head{}, err
	}
	return head, nil
}

// bindingSource is the configured side: where the configuration puts one owner's
// tree, and the connection row that answers where its bucket lives. An empty
// connection name means the owner's currently resolved connection; a name selects
// a row directly, which is what lets a rebind name the connection it is moving to
// before the binding names it.
type bindingSource interface {
	BindingFor(context.Context, string, string) (storage.Config, error)
	ConnectionFor(context.Context, string, string) (connections.Connection, error)
}

// configuredBinding derives the root the configuration selects for one owner. It
// reads the connection row only; it never opens the tree.
func configuredBinding(ctx context.Context, source bindingSource, owner, connection string) (rootBinding, error) {
	binding, err := source.BindingFor(ctx, owner, connection)
	if err != nil {
		return rootBinding{}, err
	}
	conn, err := source.ConnectionFor(ctx, owner, connection)
	if err != nil {
		return rootBinding{}, err
	}
	configured := rootBinding{Root: fileops.Root{
		OwnerID:      binding.OwnerID,
		ConnectionID: binding.ConnectionID,
		Generation:   binding.Generation,
		ContentRoot:  binding.ContentRoot,
		SystemRoot:   binding.SystemRoot,
	}}
	switch conn.Backend {
	case "cos":
		configured.Bucket, configured.Region = conn.Config.COS.Bucket, conn.Config.COS.Region
	case "s3":
		configured.Bucket, configured.Region = conn.Config.S3.Bucket, conn.Config.S3.Region
	}
	return configured, nil
}

// rootMismatches compares every owner the index or the change stream knows about
// with the tree the current configuration selects for it.
func rootMismatches(bindings map[string]rootBinding, heads map[string]string, configured func(string) (rootBinding, error)) []rootMismatch {
	var mismatches []rootMismatch
	for _, owner := range rootOwners(bindings, heads) {
		want, err := configured(owner)
		if err != nil {
			mismatches = append(mismatches, rootMismatch{Owner: owner, Resolve: err})
			continue
		}
		bound, boundOK := bindings[owner]
		head, headOK := heads[owner]
		var diffs []rootDifference
		if boundOK {
			diffs = compareRootBinding(bound, want)
		}
		if headOK && head != want.namespace() {
			diffs = append(diffs, rootDifference{Field: "change-stream namespace", Bound: head, Configured: want.namespace()})
		}
		if len(diffs) > 0 {
			mismatches = append(mismatches, rootMismatch{Owner: owner, Bound: bound, Configured: want, Diffs: diffs})
		}
	}
	return mismatches
}

func rootOwners(bindings map[string]rootBinding, heads map[string]string) []string {
	owners := make([]string, 0, len(bindings)+len(heads))
	for owner := range bindings {
		owners = append(owners, owner)
	}
	for owner := range heads {
		if _, seen := bindings[owner]; !seen {
			owners = append(owners, owner)
		}
	}
	sort.Strings(owners)
	return owners
}

// refuseStorageRoots is the startup gate. It compares the persisted binding with
// the configured root before the process serves anything, and records the provider
// coordinates of a binding that still matches so the next change can be reported
// field by field. It performs no native I/O.
func refuseStorageRoots(ctx context.Context, store rootStore, source bindingSource) error {
	if store == nil || source == nil {
		return nil
	}
	bindings, err := store.Bindings(ctx)
	if err != nil {
		return fmt.Errorf("read root bindings: %w", err)
	}
	heads, err := store.ChangeNamespaces(ctx)
	if err != nil {
		return fmt.Errorf("read change-stream namespaces: %w", err)
	}
	mismatches := rootMismatches(bindings, heads, func(owner string) (rootBinding, error) {
		return configuredBinding(ctx, source, owner, "")
	})
	if len(mismatches) > 0 {
		return &rootMismatchError{Mismatches: mismatches}
	}
	// Everything agrees. Record the coordinates a pre-upgrade binding is missing,
	// so a later change names the bucket and region it is leaving instead of only
	// the opaque digest.
	for _, owner := range rootOwners(bindings, heads) {
		bound, ok := bindings[owner]
		if !ok || bound.hasCoordinates() {
			continue
		}
		want, err := configuredBinding(ctx, source, owner, "")
		if err != nil {
			return err
		}
		if !want.hasCoordinates() {
			continue
		}
		if err := store.BackfillCoordinates(ctx, owner, want.Bucket, want.Region); err != nil {
			return fmt.Errorf("record root coordinates for owner %s: %w", owner, err)
		}
	}
	return nil
}

// inspectStorageRoots is the read-only form used by `eam doctor`. It reports the
// same disagreement the server refuses to start with, and changes nothing.
func inspectStorageRoots(ctx context.Context, store rootStore, source bindingSource) error {
	if store == nil || source == nil {
		return nil
	}
	bindings, err := store.Bindings(ctx)
	if err != nil {
		return fmt.Errorf("inspect storage roots: %w", err)
	}
	heads, err := store.ChangeNamespaces(ctx)
	if err != nil {
		return fmt.Errorf("inspect storage roots: %w", err)
	}
	mismatches := rootMismatches(bindings, heads, func(owner string) (rootBinding, error) {
		return configuredBinding(ctx, source, owner, "")
	})
	if len(mismatches) == 0 {
		return nil
	}
	return &rootMismatchError{Mismatches: mismatches}
}
