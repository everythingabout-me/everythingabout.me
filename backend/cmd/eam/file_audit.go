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
	"everythingabout.me/backend/internal/store"
)

// auditCutoff parses the explicit retention boundary. There is deliberately no
// default: file_audit is the only durable record of who read or changed an owner's
// files and whether the request was allowed, so retention must be a typed policy
// decision, not a value that arrives with a binary upgrade. A bare date is accepted
// as UTC midnight because that is the form an operator writes in a runbook; the
// resolved instant is echoed in UTC so the command never guesses a time zone.
func auditCutoff(value string, now time.Time) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("file-audit-prune requires --before as an RFC 3339 timestamp or a YYYY-MM-DD UTC date, for example --before 2025-11-13")
	}
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		if at.After(now) {
			return time.Time{}, fmt.Errorf("--before %s is in the future: retention is bounded by age, and a future cutoff would delete rows written after this command started", value)
		}
		return at.UTC(), nil
	}
	if at, err := time.Parse("2006-01-02", value); err == nil {
		if at.After(now) {
			return time.Time{}, fmt.Errorf("--before %s is in the future: retention is bounded by age, and a future cutoff would delete rows written after this command started", value)
		}
		return at.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("--before must be an RFC 3339 timestamp or a YYYY-MM-DD UTC date, got %q", value)
}

// runFileAuditPrune is the explicit command that lets file_audit stop growing. It
// deletes only this owner's rows observed strictly before --before; the audit reader
// and every recent row are untouched, and no scheduler calls it.
//
// It has no wall-clock deadline for the same reason file-operations-prune does not:
// the work is bounded by the expired backlog, a batch at a time, and an interrupted
// run is safe to repeat.
func runFileAuditPrune(args []string) error {
	const name = "file-audit-prune"
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	owner := flags.String("owner-id", "", "owner UUID")
	before := flags.String("before", "", "delete audit rows observed before this RFC 3339 timestamp or YYYY-MM-DD UTC date")
	dry := flags.Bool("dry-run", false, "report what would be deleted without deleting")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !fc.ValidEntryID(*owner) {
		return fmt.Errorf("%s requires --owner-id canonical UUID and no positional arguments", name)
	}
	cutoff, err := auditCutoff(*before, time.Now().UTC())
	if err != nil {
		return err
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
		return fmt.Errorf("owner %s was not found; file-audit-prune needs an existing account", *owner)
	}
	deleted, err := filechanges.PruneAuditBefore(ctx, db.Pool(), *owner, cutoff, *dry)
	if err != nil {
		return err
	}
	label, verb := "", "pruned"
	if *dry {
		label, verb = "[dry-run] ", "would prune"
	}
	fmt.Printf("%s%s %d audit rows for owner %s observed before %s; rows at or after the cutoff still serve GET /api/audit\n",
		label, verb, deleted, *owner, cutoff.Format(time.RFC3339))
	return nil
}
