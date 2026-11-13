package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/store"
)

// rebindOptions is the whole operator contract of `eam storage-rebind`: the owner,
// the connection the binder believes it is leaving, the connection the current
// configuration selects, and the two confirmations that no request can supply.
type rebindOptions struct {
	Owner           string
	From            string
	To              string
	WritersQuiesced bool
	DryRun          bool
}

// rebindRoot re-points one owner's derived view at the configured root. It is the
// only supported way to accept a bucket, region, prefix or generation change, and
// it is deliberately an operator action: the service cannot tell a planned
// migration from an accidental configuration edit, and the two need opposite
// behaviour.
//
// It reads no provider state and writes none. The operator has already copied the
// bytes; this command only records that the index now describes that copy.
func rebindRoot(ctx context.Context, roots rootStore, source bindingSource, opts rebindOptions) (string, error) {
	if roots == nil || source == nil {
		return "", fmt.Errorf("storage-rebind needs a database and the configured storage connections")
	}
	if opts.Owner == "" || opts.From == "" || opts.To == "" {
		return "", fmt.Errorf("storage-rebind requires --owner-id, --from and --to: name the owner, the connection the index is bound to and the connection the configuration now selects")
	}
	if !opts.WritersQuiesced {
		return "", fmt.Errorf("storage-rebind requires --writers-quiesced after stopping all application and external writers and settling accepted operations; a rebind must not race a publication")
	}
	bindings, err := roots.Bindings(ctx)
	if err != nil {
		return "", fmt.Errorf("read root bindings: %w", err)
	}
	bound, ok := bindings[opts.Owner]
	if !ok {
		return "", fmt.Errorf("owner %s has no persisted root binding, so there is nothing to re-point; starting the server binds a fresh owner on first use, and an owner that already holds content is admitted with `eam storage-admit`", opts.Owner)
	}
	// --to names the connection the owner is moving to; it is resolved directly,
	// not through the owner's persisted binding, because the whole point of the
	// command is to change that binding.
	configured, err := configuredBinding(ctx, source, opts.Owner, opts.To)
	if err != nil {
		return "", fmt.Errorf("the connection named by --to %q cannot bind owner %s: %w", opts.To, opts.Owner, err)
	}
	if got := connectionName(bound.ConnectionID); opts.From != got {
		return "", fmt.Errorf("--from %q does not match the connection this owner is bound to (%q); the index binding is %q", opts.From, got, bound.ConnectionID)
	}
	if got := connectionName(configured.ConnectionID); opts.To != got {
		return "", fmt.Errorf("--to %q selected connection %q instead; refusing to write a binding the operator did not name", opts.To, got)
	}
	heads, err := roots.ChangeNamespaces(ctx)
	if err != nil {
		return "", fmt.Errorf("read change-stream namespaces: %w", err)
	}
	currentNamespace := heads[opts.Owner]
	bindingChanged := bound.namespace() != configured.namespace()
	namespaceChanged := currentNamespace != configured.namespace()
	coordinatesMissing := !bound.hasCoordinates() && configured.hasCoordinates()

	if !bindingChanged && !namespaceChanged && !coordinatesMissing {
		return fmt.Sprintf("owner %s is already bound to connection %q generation %d and its change stream already uses that namespace; nothing changed\n",
			opts.Owner, connectionName(configured.ConnectionID), configured.Generation), nil
	}
	if !bindingChanged && !namespaceChanged {
		// Only the coordinates a pre-upgrade binding never recorded are missing.
		// The identity is already correct, so rotating the namespace here would
		// expire cursors for no reason.
		if opts.DryRun {
			return fmt.Sprintf("[dry-run] would record bucket %q and region %q for owner %s; the binding identity and change stream already match\n",
				configured.Bucket, configured.Region, opts.Owner), nil
		}
		if err := roots.BackfillCoordinates(ctx, opts.Owner, configured.Bucket, configured.Region); err != nil {
			return "", fmt.Errorf("record root coordinates for owner %s: %w", opts.Owner, err)
		}
		return fmt.Sprintf("recorded bucket %q and region %q for owner %s; the binding identity and change stream already matched, so nothing was rebound\n",
			configured.Bucket, configured.Region, opts.Owner), nil
	}

	work, err := roots.PendingWork(ctx, opts.Owner)
	if err != nil {
		return "", fmt.Errorf("read pending work for owner %s: %w", opts.Owner, err)
	}
	if work.busy() {
		return "", fmt.Errorf("refusing to rebind owner %s: %d non-terminal operation(s) and %d pending guard(s) remain; stop writers, settle accepted work and retry", opts.Owner, work.Operations, work.Guards)
	}

	label := "rebound "
	if opts.DryRun {
		label = "[dry-run] would "
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%sowner %s from connection %q to connection %q\n", label, opts.Owner, connectionName(bound.ConnectionID), connectionName(configured.ConnectionID)))
	for _, diff := range compareRootBinding(bound, configured) {
		b.WriteString(fmt.Sprintf("  %s: %q -> %q\n", diff.Field, diff.Bound, diff.Configured))
	}
	boundTo := fmt.Sprintf("  now bound to connection %q generation %d", connectionName(configured.ConnectionID), configured.Generation)
	if configured.Bucket != "" {
		boundTo += fmt.Sprintf(", bucket %q", configured.Bucket)
	}
	if configured.Region != "" {
		boundTo += fmt.Sprintf(", region %q", configured.Region)
	}
	boundTo += fmt.Sprintf(", content %q, system %q\n", configured.ContentRoot, configured.SystemRoot)
	b.WriteString(boundTo)
	if currentNamespace == "" {
		b.WriteString("  change-stream namespace: (unbound) -> " + configured.namespace() + "\n")
	} else {
		b.WriteString(fmt.Sprintf("  change-stream namespace: %q -> %q\n", currentNamespace, configured.namespace()))
	}
	if opts.DryRun {
		b.WriteString("  the transaction would roll back; nothing was written\n")
		return b.String(), nil
	}
	head, err := roots.Rebind(ctx, opts.Owner, configured, configured.namespace())
	if errors.Is(err, filechanges.ErrRootBusy) {
		return "", fmt.Errorf("refusing to rebind owner %s: a pending guard or non-terminal operation appeared while the rebind was taking the exclusive root lock; settle it and retry", opts.Owner)
	}
	if err != nil {
		return "", fmt.Errorf("rebind owner %s: %w", opts.Owner, err)
	}
	b.WriteString(fmt.Sprintf("  new root epoch %s; old cursors, snapshots and subscriptions are expired\n", head.RootEpoch))
	b.WriteString("no bucket was listed, read, copied or deleted; file bytes, file IDs, quota counters and audit are unchanged\n")
	b.WriteString(fmt.Sprintf("the index and quota counters were not re-observed; verify the copied tree with `eam reconcile --owner %s`\n", opts.Owner))
	return b.String(), nil
}

// runStorageRebind is the CLI wrapper. It derives the configured root from the
// connection rows and never opens a provider: the only durable records it changes
// are file_budgets.root_binding and file_change_heads.
func runStorageRebind(args []string) error {
	flags := flag.NewFlagSet("storage-rebind", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	owner := flags.String("owner-id", "", "owner UUID whose binding is re-pointed")
	from := flags.String("from", "", "connection name the index is bound to")
	to := flags.String("to", "", "connection name the owner is moving to")
	quiesced := flags.Bool("writers-quiesced", false, "confirm all application/external writers are stopped and accepted work is settled")
	dry := flags.Bool("dry-run", false, "report the change and roll back")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !fc.ValidEntryID(*owner) {
		return fmt.Errorf("storage-rebind requires --owner-id canonical UUID and no positional arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	resolver := connections.NewResolver(*cfg, connections.NewPostgresStore(db))
	defer resolver.Close()
	out, err := rebindRoot(ctx, newRootStore(db), resolver, rebindOptions{
		Owner: *owner, From: *from, To: *to, WritersQuiesced: *quiesced, DryRun: *dry,
	})
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
