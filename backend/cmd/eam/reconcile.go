package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/treescan"
)

// pendingDeleteBlocker refuses a reconcile while the owner has a permanent
// deletion whose native purge has not settled. A delete that `/api/files`
// accepted has already lost its index rows, so a scan started after the
// acceptance would import the native objects the purge has not reached yet and
// resurrect a path the user deleted. The operation journal is the only durable
// record of that state — the legacy fs_jobs table this command used to read never
// saw a `/api/files` delete, so it answered "no pending deletion" and let the scan
// through.
//
// The scan namespace lock is already held by the caller, so no new delete can be
// accepted while this reads; the journal answers for deletions that are already
// purging.
func pendingDeleteBlocker(ctx context.Context, q fileops.Queryer, ownerID, namespace string) error {
	roots, err := fileops.PendingDeleteRoots(ctx, q, ownerID, namespace)
	if err != nil {
		return fmt.Errorf("read pending deletions for reconcile owner %s: %w", ownerID, err)
	}
	if len(roots) == 0 {
		return nil
	}
	message := fmt.Sprintf("permanent deletion of %q has not finished purging; wait for it to settle before reconciling this owner", roots[0])
	if len(roots) > 1 {
		message = fmt.Sprintf("permanent deletions of %d paths (including %q) have not finished purging; wait for them to settle before reconciling this owner", len(roots), roots[0])
	}
	return errors.New(message)
}

// runReconcile imports existing native names and metadata without changing native
// content. P1 deliberately never prunes: complete enumeration and CSV claims do
// not establish writer quiescence or safe deletion of index/application metadata.
func runReconcile(args []string) error {
	flags := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	owner := flags.String("owner", "", "required owner email or UUID")
	inventory := flags.String("inventory", "", "scoped CSV with key,size headers and exact unescaped native-root keys (local: drive/<owner>/ keys)")
	dryRun := flags.Bool("dry-run", false, "validate and report index changes without database writes")
	sync := flags.Bool("sync", false, "remove index entries the native tree no longer has (the index is a derived view)")
	maxEntries := flags.Int("max-entries", 1000000, "maximum scanned entries including implicit folders (up to 5000000)")
	maxPages := flags.Int("max-pages", 100000, "maximum native listing pages (up to 1000000)")
	maxDepth := flags.Int("max-depth", 128, "maximum path depth (up to 1024)")
	pageSize := flags.Int("page-size", 1000, "requested immediate-child listing page size (up to 10000)")
	maxInventoryBytes := flags.Int64("max-inventory-bytes", 256<<20, "maximum CSV input bytes (up to 8 GiB)")
	timeout := flags.Duration("timeout", 30*time.Minute, "total scan/import timeout (positive and at most 24h)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("reconcile does not accept positional arguments")
	}
	if strings.TrimSpace(*owner) == "" {
		return fmt.Errorf("reconcile requires --owner email-or-UUID; legacy data is handled separately by claim")
	}
	if *timeout <= 0 || *timeout > 24*time.Hour {
		return fmt.Errorf("reconcile timeout must be positive and at most 24h")
	}
	if *maxEntries <= 0 || *maxEntries > 5000000 || *maxPages <= 0 || *maxPages > 1000000 || *maxDepth <= 0 || *maxDepth > 1024 || *pageSize <= 0 || *pageSize > 10000 || *maxInventoryBytes <= 0 || *maxInventoryBytes > 8<<30 {
		return fmt.Errorf("reconcile limits must be positive and within the documented bounds")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to index database: %w", err)
	}
	defer db.Close()
	selector := strings.TrimSpace(*owner)
	var id pgtype.UUID
	query := `select id::text from users where lower(email)=lower($1)`
	if err := id.Scan(selector); err == nil && id.Valid {
		query = `select id::text from users where id=$1::uuid`
	}
	var ownerID string
	if err := db.Pool().QueryRow(ctx, query, selector).Scan(&ownerID); err != nil {
		return fmt.Errorf("resolve reconcile owner %q: %w", selector, err)
	}
	readConfig := *cfg
	readConfig.Storage.CoordinatedWrites = false
	backend, err := storagebackend.New(&readConfig)
	if err != nil {
		return err
	}
	defer backend.Close()
	driver, err := backend.Open(ctx, ownerID)
	if err != nil {
		return err
	}
	defer driver.Close()
	binding, err := backend.Binding(ownerID)
	if err != nil {
		return err
	}
	descriptor := driver.Describe()
	changeRoot := fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: descriptor.Names.CaseSensitive, RetainsVersions: descriptor.RetainsVersions}
	// Keep deletion admission excluded through enumeration and import. A result
	// captured before an accepted deletion cannot be applied after it completes.
	scanConn, unlock, err := nodes.LockScanNamespace(ctx, db.Pool(), ownerID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := pendingDeleteBlocker(ctx, scanConn, ownerID, changeRoot.Namespace()); err != nil {
		return err
	}
	opts := treescan.Options{Names: driver.Describe().Names, MaxEntries: *maxEntries, MaxPages: *maxPages, MaxDepth: *maxDepth, PageSize: *pageSize, MaxInventoryBytes: *maxInventoryBytes}
	fmt.Printf("scan owner: %s; index pruning: disabled; dry-run: %t\n", ownerID, *dryRun)
	var result *treescan.Result
	if *inventory != "" {
		fmt.Println("inventory: validating supplied records; current native tree completeness is unverified")
		// Object-store inventories use the configured native prefix. Local CSVs
		// use logical drive keys; neither becomes a raw private database key.
		prefix := paths.OwnerPrefix(ownerID)
		if cfg.BackendName() != "local" {
			prefix = binding.ContentRoot
		}
		input, err := os.Open(*inventory)
		if err != nil {
			return err
		}
		result, err = treescan.Inventory(ctx, input, prefix, opts)
		closeErr := input.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else {
		result, err = treescan.Scan(ctx, driver, opts)
		if err != nil {
			return err
		}
	}
	report, err := nodes.ApplyScan(ctx, scanConn, ownerID, result, nodes.ScanApplyOptions{DryRun: *dryRun, ChangeRoot: &changeRoot, Sync: *sync})
	if err != nil {
		return err
	}
	label := ""
	if *dryRun {
		label = "[dry-run] "
	}
	if !*sync {
		fmt.Printf("%sscanned %d paths, inserted %d, updated %d, unchanged %d; pruned 0 (add --sync to remove entries the native tree no longer has)\n", label, report.Scanned, report.Inserted, report.Updated, report.Unchanged)
		return nil
	}
	if *dryRun {
		for _, path := range report.RemovedPaths {
			fmt.Printf("  would remove %s\n", path)
		}
		if report.Removed > len(report.RemovedPaths) {
			fmt.Printf("  ... and %d more\n", report.Removed-len(report.RemovedPaths))
		}
	}
	fmt.Printf("%sscanned %d paths, inserted %d, updated %d, unchanged %d; removed %d\n", label, report.Scanned, report.Inserted, report.Updated, report.Unchanged, report.Removed)
	return nil
}
