// Command eam is the Everything About Me backend server and maintenance tool.
//
//	eam server                      start the HTTP server (default)
//	eam migrate                     apply pending sql/*.sql migrations in filename order
//	eam doctor [--owner-id UUID] [--write]  self-check configuration / database / selected storage
//	eam reconcile --owner email-or-UUID [--inventory inventory.csv] [--dry-run]
//	eam storage-admit --owner-id UUID --writers-quiesced --evidence proof.json [--dry-run]
//	eam storage-baseline --owner-id UUID --output new-baseline.json
//	eam storage-resolve-upload --operation-id UUID [--dry-run]
//	eam claim --email email [--dry-run] [--keep-source]
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	args := os.Args[1:]
	// eam --help / eam -h is the same as eam help (otherwise it parses as a server flag)
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		usage()
		return
	}
	command := "server"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	var err error
	switch command {
	case "server":
		err = runServer(args)
	case "migrate":
		err = runMigrate(args)
	case "doctor":
		err = runDoctor(args)
	case "reconcile":
		err = runReconcile(args)
	case "backup":
		err = runBackup(args)
	case "storage-connection":
		err = runStorageConnection(args)
	case "storage-admit":
		err = runStorageAdmit(args)
	case "storage-baseline":
		err = runStorageBaseline(args)
	case "storage-resolve-upload":
		err = runStorageResolveUpload(args)
	case "storage-ledger-prune":
		err = runStorageLedgerPrune(args)
	case "file-changes-prune":
		err = runFileChangesMaintenance(args, false)
	case "file-operations-prune":
		err = runFileOperationsPrune(args)
	case "file-changes-reset":
		err = runFileChangesMaintenance(args, true)
	case "claim":
		err = runClaim(args)
	case "token":
		err = runToken(args)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Everything About Me backend

usage:
  eam [server]                     start the HTTP server
  eam migrate                      apply pending migrations / create tables
  eam doctor [--owner-id UUID] [--write]  check config, database, selected storage
  eam reconcile --owner email-or-UUID [--inventory inventory.csv] [--dry-run]
  eam backup snapshot --owner email-or-UUID [--connection NAME] [--date YYYY-MM-DD]
  eam backup list     --owner email-or-UUID [--connection NAME]
  eam backup prune    --owner email-or-UUID --keep N [--connection NAME]
  eam backup restore  --owner email-or-UUID --date YYYY-MM-DD [--to PATH]
  eam storage-connection list
  eam storage-connection add --name NAME --backend cos|s3|local --role authority|backup [flags]
      [--owner EMAIL|UUID] gives the connection to one account; [--root-at-bucket] puts that tree at the bucket root
  eam storage-connection probe --name NAME [--owner email-or-UUID]  measure one connection and record its write evidence
  eam storage-admit --owner-id UUID --writers-quiesced --evidence proof.json [--physical-limit N] [--dry-run]
  eam storage-baseline --owner-id UUID --output new-baseline.json [--writers-quiesced]
  eam storage-baseline --input baseline.json --output recheck.json [--quota-bytes N]
  eam storage-resolve-upload --operation-id UUID [--dry-run]  resolve a terminal upload stuck on an unconfirmed part receipt
  eam storage-ledger-prune --owner-id UUID [--dry-run]  remove expired settled intent records, keep non-terminal evidence
  eam file-changes-prune --owner-id UUID [--dry-run]  remove expired change rows, retain deduplication receipts
  eam file-operations-prune --owner-id UUID [--dry-run]  remove terminal operations past retention, keep referenced ones
  eam file-changes-reset --owner-id UUID --writers-quiesced [--dry-run]  bind current root and expire old cursors
  eam claim --email email [--dry-run] [--keep-source]
  eam token create --email email --name name [--prefix path] [--write=false]
  eam token list   --email email
  eam token revoke --email email --id token-id

Environment variables come from the process; a .env in the current or the two
parent directories is loaded automatically.
Common: DATABASE_URL / APP_SECRET / STORAGE_BACKEND / COS_* / S3_* / LOCAL_* / SES_* / APP_ENV / ADDR / WEB_DIR
storage-baseline --input is offline and does not load application configuration.
`)
}
