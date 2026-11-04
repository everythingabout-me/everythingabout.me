package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// These explicit administrative actions never touch file bytes or provider
// state. Reset requires quiescence and refuses outstanding mutation guards.
func runFileChangesMaintenance(args []string, reset bool) error {
	name := "file-changes-prune"
	if reset {
		name = "file-changes-reset"
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	owner := flags.String("owner-id", "", "owner UUID")
	dry := flags.Bool("dry-run", false, "roll back the maintenance transaction")
	quiesced := flags.Bool("writers-quiesced", false, "confirm all application/external writers are stopped")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !fc.ValidEntryID(*owner) {
		return fmt.Errorf("%s requires --owner-id canonical UUID and no positional arguments", name)
	}
	if reset && !*quiesced {
		return fmt.Errorf("file-changes-reset requires --writers-quiesced after stopping writers and settling accepted operations")
	}
	if !reset && *quiesced {
		return fmt.Errorf("file-changes-prune does not require --writers-quiesced")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
		return fmt.Errorf("owner was not found")
	}
	namespace := ""
	if reset {
		backend, err := storagebackend.New(cfg)
		if err != nil {
			return err
		}
		defer backend.Close()
		binding, err := backend.Binding(*owner)
		if err != nil {
			return err
		}
		namespace = fmt.Sprintf("%s:%s:%d:%s:%s", binding.OwnerID, binding.ConnectionID, binding.Generation, binding.ContentRoot, binding.SystemRoot)
	}
	tx, err := db.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	label := ""
	if *dry {
		label = "[dry-run] "
	}
	if reset {
		head, err := filechanges.ResetRoot(ctx, tx, *owner, namespace)
		if err != nil {
			return err
		}
		if !*dry {
			if err = tx.Commit(ctx); err != nil {
				return err
			}
		}
		fmt.Printf("%sreset change tracking for owner %s; new epoch %s; clients must create a new baseline\n", label, *owner, head.RootEpoch)
	} else {
		count, err := filechanges.PruneExpired(ctx, tx, *owner, time.Now().UTC())
		if err != nil {
			return err
		}
		if !*dry {
			if err = tx.Commit(ctx); err != nil {
				return err
			}
		}
		fmt.Printf("%spruned %d expired change rows for owner %s; effect receipts and file bytes retained\n", label, count, *owner)
	}
	return nil
}
