package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/store"
)

// runToken 管理对外 API 的访问令牌。
//
//	eam token create --email you@example.com --name Obsidian [--prefix Apps/obsidian] [--write=false]
//	eam token list   --email you@example.com
//	eam token revoke --email you@example.com --id 7f3a9c2b1d4e
func runToken(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`usage:
  eam token create --email email --name name [--prefix path] [--write=false] [--expires 720h]
  eam token list   --email email
  eam token revoke --email email --id token-id`)
	}
	switch args[0] {
	case "create":
		return runTokenCreate(args[1:])
	case "list":
		return runTokenList(args[1:])
	case "revoke":
		return runTokenRevoke(args[1:])
	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

func runTokenCreate(args []string) error {
	flags := flag.NewFlagSet("token create", flag.ContinueOnError)
	email := flags.String("email", "", "account email")
	name := flags.String("name", "", "token name, e.g. Obsidian")
	prefix := flags.String("prefix", "", "path prefix the token may access; empty means the whole drive")
	canRead := flags.Bool("read", true, "allow reads")
	canWrite := flags.Bool("write", true, "allow writes")
	expires := flags.Duration("expires", 0, "lifetime, e.g. 720h (never expires by default)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *email == "" || *name == "" {
		return fmt.Errorf("--email and --name are required")
	}

	db, svc, err := openAuthService()
	if err != nil {
		return err
	}
	defer db.Close()

	user, err := svc.FindUserByEmail(context.Background(), *email)
	if err != nil {
		return err
	}
	if user == nil {
		return fmt.Errorf("account %s not found", *email)
	}

	var expiry *time.Time
	if *expires > 0 {
		at := time.Now().Add(*expires)
		expiry = &at
	}

	plain, token, err := svc.CreateToken(context.Background(), auth.NewTokenInput{
		UserID:   user.ID,
		Name:     *name,
		Prefix:   *prefix,
		CanRead:  *canRead,
		CanWrite: *canWrite,
		Expiry:   expiry,
	})
	if err != nil {
		return err
	}

	fmt.Printf("\ntoken created (shown only once, save it now):\n\n  %s\n\n", plain)
	fmt.Printf("  name:   %s\n", token.Name)
	if token.Prefix == "" {
		fmt.Printf("  prefix: whole drive\n")
	} else {
		fmt.Printf("  prefix: %s\n", token.Prefix)
	}
	fmt.Printf("  access: %s\n", permissionText(token.CanRead, token.CanWrite))
	if token.ExpiresAt == nil {
		fmt.Printf("  expiry: never\n")
	} else {
		fmt.Printf("  expiry: %s\n", *token.ExpiresAt)
	}
	fmt.Printf("  ID:     %s (for eam token revoke)\n\n", token.ID)
	return nil
}

func runTokenList(args []string) error {
	flags := flag.NewFlagSet("token list", flag.ContinueOnError)
	email := flags.String("email", "", "account email")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return fmt.Errorf("--email is required")
	}

	db, svc, err := openAuthService()
	if err != nil {
		return err
	}
	defer db.Close()

	user, err := svc.FindUserByEmail(context.Background(), *email)
	if err != nil {
		return err
	}
	if user == nil {
		return fmt.Errorf("account %s not found", *email)
	}

	tokens, err := svc.ListTokens(context.Background(), user.ID)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		fmt.Println("No tokens yet.")
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tNAME\tPREFIX\tACCESS\tSTATUS\tLAST USED\tEXPIRES")
	for _, token := range tokens {
		prefix := token.Prefix
		if prefix == "" {
			prefix = "(whole drive)"
		}
		lastUsed := "-"
		if token.LastUsedAt != nil {
			lastUsed = *token.LastUsedAt
		}
		expiresAt := "never"
		if token.ExpiresAt != nil {
			expiresAt = *token.ExpiresAt
		}
		status := "active"
		if token.Revoked {
			status = "revoked"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			token.ID, token.Name, prefix, permissionText(token.CanRead, token.CanWrite),
			status, lastUsed, expiresAt)
	}
	return writer.Flush()
}

func runTokenRevoke(args []string) error {
	flags := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	email := flags.String("email", "", "account email")
	id := flags.String("id", "", "token ID (the column printed by eam token list)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *email == "" || *id == "" {
		return fmt.Errorf("--email and --id are required")
	}

	db, svc, err := openAuthService()
	if err != nil {
		return err
	}
	defer db.Close()

	user, err := svc.FindUserByEmail(context.Background(), *email)
	if err != nil {
		return err
	}
	if user == nil {
		return fmt.Errorf("account %s not found", *email)
	}
	if err := svc.RevokeToken(context.Background(), user.ID, *id); err != nil {
		return err
	}
	fmt.Printf("revoked token %s. It stops working immediately; short-lived credentials already issued live at most 2 more hours.\n", *id)
	return nil
}

func openAuthService() (*store.DB, *auth.Service, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	return db, auth.New(db, cfg), nil
}

func permissionText(read, write bool) string {
	switch {
	case read && write:
		return "read+write"
	case read:
		return "read-only"
	case write:
		return "write-only"
	default:
		return "none"
	}
}
