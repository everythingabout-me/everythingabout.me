package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/store"
)

// runFileOperationsPrune is the explicit maintenance command that lets the operation journal
// stop growing: without it, RetainUntil was written and never read, so every terminal
// operation stayed forever. It deletes only terminal operations past their retention window
// and outside every reference that still needs them (see fileops.PruneExpired for the rule).
//
// It deliberately has no wall-clock deadline. The work is bounded by the expired backlog, a
// batch at a time, and an interrupted run is safe to repeat: each batch is one transaction, so
// Ctrl-C loses at most the batch in flight and the next run continues where it stopped.
func runFileOperationsPrune(args []string) error {
	name := "file-operations-prune"
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	owner := flags.String("owner-id", "", "owner UUID")
	dry := flags.Bool("dry-run", false, "report what would be deleted without deleting")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !fc.ValidEntryID(*owner) {
		return fmt.Errorf("%s requires --owner-id canonical UUID and no positional arguments", name)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	var exists bool
	if err = db.Pool().QueryRow(ctx, `select exists(select 1 from users where id=$1::uuid)`, *owner).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("owner %s was not found; file-operations-prune needs an existing account", *owner)
	}
	result, err := fileops.NewPostgres(db.Pool()).PruneExpired(ctx, *owner, time.Now().UTC(), *dry)
	if err != nil {
		return err
	}
	label := ""
	if *dry {
		label = "[dry-run] "
	}
	verb := "pruned"
	if *dry {
		verb = "would prune"
	}
	fmt.Printf("%s%s %d terminal operations for owner %s; repaired %d disagreeing rows; kept %d candidates still referenced or still live\n",
		label, verb, result.Deleted, *owner, result.Repaired, result.Skipped)
	return nil
}
