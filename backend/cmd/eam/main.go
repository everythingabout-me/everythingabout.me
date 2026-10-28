// Command eam is the Everything About Me backend server and maintenance tool.
//
//	eam server                      start the HTTP server (default)
//	eam migrate                     run sql/*.sql in filename order
//	eam doctor [--write]            self-check config / database / COS
//	eam reconcile [--owner email] [--inventory inventory.csv] [--dry-run]
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
  eam migrate                      create tables / run sql/*.sql
  eam doctor [--write]             check config, database, COS
  eam reconcile [--owner email] [--inventory inventory.csv] [--dry-run]
  eam claim --email email [--dry-run] [--keep-source]
  eam token create --email email --name name [--prefix path] [--write=false]
  eam token list   --email email
  eam token revoke --email email --id token-id

Environment variables come from the process; a .env in the current or the two
parent directories is loaded automatically.
Common: DATABASE_URL / COS_* / SES_* / APP_ENV / ADDR / WEB_DIR
`)
}
