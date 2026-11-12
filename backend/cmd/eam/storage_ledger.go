package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storagebackend"
)

// runStorageLedgerPrune is explicit housekeeping for the driver's private intent
// spool. It does not read or write the file index: admission no longer depends on
// this command, because settled records never block a rebuild. It only reclaims
// records whose retention has passed; non-terminal records and unknown entries stay
// exactly where they are.
func runStorageLedgerPrune(args []string) error {
	flags := flag.NewFlagSet("storage-ledger-prune", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	owner := flags.String("owner-id", "", "owner UUID")
	dry := flags.Bool("dry-run", false, "count settled records without removing them")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !fc.ValidEntryID(*owner) {
		return fmt.Errorf("storage-ledger-prune requires --owner-id canonical UUID and no positional arguments")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	backend, err := storagebackend.New(cfg)
	if err != nil {
		return err
	}
	defer backend.Close()
	count, err := backend.PruneSettled(context.Background(), *owner, time.Now().UTC(), !*dry)
	if err != nil {
		return err
	}
	if *dry {
		fmt.Printf("[dry-run] would prune %d settled intent ledger record(s) for owner %s; non-terminal records and unknown spool entries retained\n", count, *owner)
	} else {
		fmt.Printf("pruned %d settled intent ledger record(s) for owner %s; non-terminal records and unknown spool entries retained\n", count, *owner)
	}
	return nil
}
