package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/snapshot"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"github.com/jackc/pgx/v5/pgtype"
)

// runBackup takes, lists, prunes and restores whole-tree snapshots.
//
//	eam backup snapshot --owner email-or-UUID [--connection NAME] [--date 2026-11-10]
//	eam backup list     --owner email-or-UUID [--connection NAME]
//	eam backup prune    --owner email-or-UUID --keep 14 [--connection NAME]
//	eam backup restore  --owner email-or-UUID --date 2026-11-10 [--connection NAME] [--to restore-2026-11-10]
func runBackup(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`usage:
  eam backup snapshot --owner email-or-UUID [--connection NAME] [--date YYYY-MM-DD]
  eam backup list     --owner email-or-UUID [--connection NAME]
  eam backup prune    --owner email-or-UUID --keep N [--connection NAME]
  eam backup restore  --owner email-or-UUID --date YYYY-MM-DD [--connection NAME] [--to PATH]`)
	}
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to index database: %w", err)
	}
	defer db.Close()
	connStore := connections.NewPostgresStore(db)
	resolver := connections.NewResolver(*cfg, connStore)
	defer resolver.Close()
	switch args[0] {
	case "snapshot":
		return runBackupSnapshot(ctx, cfg, db, connStore, resolver, args[1:])
	case "list":
		return runBackupList(ctx, db, connStore, resolver, args[1:])
	case "prune":
		return runBackupPrune(ctx, db, connStore, resolver, args[1:])
	case "restore":
		return runBackupRestore(ctx, db, connStore, resolver, args[1:])
	default:
		return fmt.Errorf("unknown backup subcommand %q", args[0])
	}
}

func runBackupSnapshot(ctx context.Context, cfg *config.Config, db *store.DB, connStore connections.Store, resolver *connections.Resolver, args []string) error {
	fs := flag.NewFlagSet("backup snapshot", flag.ContinueOnError)
	owner := fs.String("owner", "", "owner email or UUID")
	name := fs.String("connection", "", "backup connection name; defaults to the owner's only backup connection")
	date := fs.String("date", time.Now().Format("2006-01-02"), "snapshot date")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ownerID, err := resolveOwnerID(ctx, db, *owner)
	if err != nil {
		return err
	}
	target, err := backupConnection(ctx, connStore, ownerID, *name)
	if err != nil {
		return err
	}
	source, err := resolver.Open(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("open the owner's tree: %w", err)
	}
	defer source.Close()
	// Measure the destination at the moment it is used, so recording one does not depend
	// on it being reachable right then.
	effective, err := target.Apply(*cfg)
	if err != nil {
		return err
	}
	if err := storagebackend.EnsureAdmission(ctx, &effective); err != nil {
		return fmt.Errorf("measure backup destination %q: %w", target.Name, err)
	}
	dst, err := resolver.OpenConnection(ctx, ownerID, target.Name)
	if err != nil {
		return fmt.Errorf("open backup connection %q: %w", target.Name, err)
	}
	defer dst.Close()
	writer, ok := dst.(snapshot.Target)
	if !ok {
		return fmt.Errorf("storage connection %q cannot be a backup target: its driver does not expose staged publication and folder creation", target.Name)
	}
	runID, err := startBackupRun(ctx, db, ownerID, target.ID, *date)
	if err != nil {
		return err
	}
	result, copyErr := snapshot.Copy(ctx, source, "", writer, storage.Path(*date))
	if err := finishBackupRun(ctx, db, runID, result, copyErr); err != nil && copyErr == nil {
		return err
	}
	if copyErr != nil {
		return fmt.Errorf("snapshot %s to %q: %w", *date, target.Name, copyErr)
	}
	fmt.Printf("snapshot %s: %s\n", *date, result)
	return nil
}

func runBackupList(ctx context.Context, db *store.DB, connStore connections.Store, resolver *connections.Resolver, args []string) error {
	fs := flag.NewFlagSet("backup list", flag.ContinueOnError)
	owner := fs.String("owner", "", "owner email or UUID")
	name := fs.String("connection", "", "backup connection name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ownerID, err := resolveOwnerID(ctx, db, *owner)
	if err != nil {
		return err
	}
	target, err := backupConnection(ctx, connStore, ownerID, *name)
	if err != nil {
		return err
	}
	tree, err := resolver.OpenConnection(ctx, ownerID, target.Name)
	if err != nil {
		return fmt.Errorf("open backup connection %q: %w", target.Name, err)
	}
	defer tree.Close()
	dates, err := snapshot.Dates(ctx, tree, "")
	if err != nil {
		return err
	}
	if len(dates) == 0 {
		fmt.Printf("no snapshots on %q\n", target.Name)
		return nil
	}
	for _, date := range dates {
		result, err := snapshot.Verify(ctx, tree, storage.Path(date))
		if err != nil {
			fmt.Printf("%s  unreadable: %v\n", date, err)
			continue
		}
		fmt.Printf("%s  %s\n", date, result)
	}
	return nil
}

func runBackupPrune(ctx context.Context, db *store.DB, connStore connections.Store, resolver *connections.Resolver, args []string) error {
	fs := flag.NewFlagSet("backup prune", flag.ContinueOnError)
	owner := fs.String("owner", "", "owner email or UUID")
	name := fs.String("connection", "", "backup connection name")
	keep := fs.Int("keep", 14, "how many snapshots to keep")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ownerID, err := resolveOwnerID(ctx, db, *owner)
	if err != nil {
		return err
	}
	target, err := backupConnection(ctx, connStore, ownerID, *name)
	if err != nil {
		return err
	}
	driver, err := resolver.OpenConnection(ctx, ownerID, target.Name)
	if err != nil {
		return fmt.Errorf("open backup connection %q: %w", target.Name, err)
	}
	defer driver.Close()
	remover, ok := driver.(storage.Remover)
	if !ok {
		return fmt.Errorf("storage connection %q cannot prune: its driver does not expose removal", target.Name)
	}
	removed, err := snapshot.Prune(ctx, driver, remover, "", *keep)
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		fmt.Printf("nothing to prune on %q\n", target.Name)
		return nil
	}
	fmt.Printf("removed %s\n", strings.Join(removed, ", "))
	return nil
}

// runBackupRestore materializes a snapshot into a new path on the owner's own
// tree. It never writes into the snapshot and never overwrites a live path, so a
// restore that goes wrong leaves both the old tree and the snapshot intact.
func runBackupRestore(ctx context.Context, db *store.DB, connStore connections.Store, resolver *connections.Resolver, args []string) error {
	fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	owner := fs.String("owner", "", "owner email or UUID")
	name := fs.String("connection", "", "backup connection name")
	date := fs.String("date", "", "snapshot date to restore")
	to := fs.String("to", "", "destination path on the owner's tree; defaults to restore-<date>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *date == "" {
		return fmt.Errorf("--date is required: a restore picks one snapshot")
	}
	ownerID, err := resolveOwnerID(ctx, db, *owner)
	if err != nil {
		return err
	}
	target, err := backupConnection(ctx, connStore, ownerID, *name)
	if err != nil {
		return err
	}
	dest := *to
	if dest == "" {
		dest = "restore-" + *date
	}
	src, err := resolver.OpenConnection(ctx, ownerID, target.Name)
	if err != nil {
		return fmt.Errorf("open backup connection %q: %w", target.Name, err)
	}
	defer src.Close()
	live, err := resolver.Open(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("open the owner's tree: %w", err)
	}
	defer live.Close()
	writer, ok := live.(snapshot.Target)
	if !ok {
		return fmt.Errorf("the owner's tree cannot be restored into: its driver does not expose staged publication")
	}
	result, err := snapshot.Copy(ctx, src, storage.Path(*date), writer, storage.Path(dest))
	if err != nil {
		return fmt.Errorf("restore %s from %q: %w", *date, target.Name, err)
	}
	fmt.Printf("restored %s into %q: %s\n", *date, dest, result)
	fmt.Printf("nothing is indexed yet: run `eam reconcile --owner %s` after moving the restored tree to where it belongs\n", ownerID)
	return nil
}

// backupConnection picks the destination: the named row, or the owner's only
// backup-role row. Zero rows is an error with the command that creates one.
func backupConnection(ctx context.Context, connStore connections.Store, ownerID, name string) (connections.Connection, error) {
	if name != "" {
		c, ok, err := connStore.ByName(ctx, ownerID, name)
		if err != nil {
			return connections.Connection{}, err
		}
		if !ok {
			return connections.Connection{}, fmt.Errorf("storage connection %q does not exist for owner %s", name, ownerID)
		}
		return c, nil
	}
	visible, err := connStore.Visible(ctx, ownerID)
	if err != nil {
		return connections.Connection{}, err
	}
	var backups []connections.Connection
	for _, c := range visible {
		if c.Role == connections.RoleBackup && c.Enabled {
			backups = append(backups, c)
		}
	}
	switch len(backups) {
	case 0:
		return connections.Connection{}, fmt.Errorf("owner %s has no backup connection: add one with `eam storage-connection add --owner %s --backend cos --role backup ...`", ownerID, ownerID)
	case 1:
		return backups[0], nil
	default:
		return connections.Connection{}, fmt.Errorf("owner %s has %d backup connections: name one with --connection", ownerID, len(backups))
	}
}

func resolveOwnerID(ctx context.Context, db *store.DB, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", fmt.Errorf("--owner is required (email or UUID)")
	}
	var id pgtype.UUID
	query := `select id::text from users where lower(email)=lower($1)`
	if err := id.Scan(selector); err == nil && id.Valid {
		query = `select id::text from users where id=$1::uuid`
	}
	var ownerID string
	if err := db.Pool().QueryRow(ctx, query, selector).Scan(&ownerID); err != nil {
		return "", fmt.Errorf("resolve owner %q: %w", selector, err)
	}
	return ownerID, nil
}

func startBackupRun(ctx context.Context, db *store.DB, ownerID, connectionID, date string) (string, error) {
	var id string
	err := db.Pool().QueryRow(ctx, `
		insert into backup_runs(owner_id, connection_id, snapshot, state)
		values($1::uuid,$2::uuid,$3::date,'running')
		on conflict (owner_id, connection_id, snapshot) do update set started_at=now(), state='running', error=null
		returning id::text`, ownerID, connectionID, date).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("record backup run: %w", err)
	}
	return id, nil
}

func finishBackupRun(ctx context.Context, db *store.DB, runID string, result snapshot.Result, runErr error) error {
	state := "complete"
	var detail any
	if runErr != nil {
		state = "failed"
		detail = runErr.Error()
	}
	_, err := db.Pool().Exec(ctx, `update backup_runs set state=$2, files=$3, folders=$4, bytes=$5, error=$6, finished_at=now() where id=$1::uuid`,
		runID, state, result.Files, result.Folders, result.Bytes, detail)
	if err != nil {
		return fmt.Errorf("record backup result: %w", err)
	}
	return nil
}
