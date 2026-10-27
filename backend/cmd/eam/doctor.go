package main

import (
	"context"
	"flag"
	"fmt"
	"regexp"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/store"
)

var credentialRe = regexp.MustCompile(`//[^@]*@`)

type doctor struct {
	failed int
}

func (d *doctor) ok(label, detail string) {
	fmt.Printf("  ✓ %s%s\n", label, detail)
}

func (d *doctor) bad(label string, err error, hint string) {
	d.failed++
	fmt.Printf("  ✗ %s  %v\n", label, err)
	if hint != "" {
		fmt.Printf("    → %s\n", hint)
	}
}

func (d *doctor) warn(label string) {
	fmt.Printf("  ! %s\n", label)
}

func runDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	writeTest := flags.Bool("write", false, "also test write + delete")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	checks := &doctor{}

	fmt.Println("\nconfig")
	fmt.Printf("  bucket   %s\n", cfg.COS.Bucket)
	fmt.Printf("  region   %s\n", cfg.COS.Region)
	fmt.Printf("  database %s\n", credentialRe.ReplaceAllString(cfg.DatabaseURL, "//***@"))

	ctx := context.Background()
	db, dbErr := store.Open(ctx, cfg.DatabaseURL)

	fmt.Println("\ndatabase")
	if dbErr != nil {
		checks.bad("connection failed", dbErr, "check DATABASE_URL; use another port if it is taken")
	} else {
		defer db.Close()
		checkDatabase(ctx, db, checks)
	}

	fmt.Println("\nemail")
	if cfg.SESConfigured() {
		checks.ok("Tencent SES configured", " "+cfg.SES.Region+" · "+cfg.SES.FromEmailAddress)
	} else if cfg.IsProduction() {
		checks.bad("Tencent SES not configured",
			fmt.Errorf("cannot send verification codes in production"),
			"set SES_FROM_EMAIL_ADDRESS / SES_TEMPLATE_ID etc.; or run in development")
	} else {
		checks.warn("Tencent SES not configured: in development the code goes to the server log and is autofilled on the login page")
	}

	fmt.Println("\nCOS")
	if err := cfg.ValidateCOS(); err != nil {
		checks.bad("incomplete COS config", err, "set COS_SECRET_ID / COS_SECRET_KEY / COS_BUCKET / COS_REGION")
	} else {
		checkCOS(ctx, cfg, checks, *writeTest)
	}

	if checks.failed == 0 {
		fmt.Print("\nAll checks passed.\n\n")
		return nil
	}
	fmt.Printf("\n%d check(s) failed.\n", checks.failed)
	return fmt.Errorf("%d check(s) failed", checks.failed)
}

func checkDatabase(ctx context.Context, db *store.DB, checks *doctor) {
	pool := db.Pool()

	var nodesTable string
	if err := pool.QueryRow(ctx, `select coalesce(to_regclass('public.nodes')::text, '')`).Scan(&nodesTable); err != nil {
		checks.bad("failed to query nodes table", err, "")
		return
	}
	if nodesTable == "" {
		checks.bad("nodes table is missing", fmt.Errorf("missing"), "run eam migrate first")
	} else {
		var count int
		if err := pool.QueryRow(ctx, `select count(*)::int from nodes`).Scan(&count); err != nil {
			checks.bad("failed to count nodes", err, "")
		} else {
			checks.ok("connected, nodes table ready", fmt.Sprintf(" %d rows", count))
		}
	}

	var usersTable, sessionsTable, codesTable string
	err := pool.QueryRow(ctx, `select
		coalesce(to_regclass('public.users')::text, ''),
		coalesce(to_regclass('public.sessions')::text, ''),
		coalesce(to_regclass('public.email_codes')::text, '')`).Scan(&usersTable, &sessionsTable, &codesTable)
	if err != nil {
		checks.bad("failed to query user tables", err, "")
		return
	}
	if usersTable == "" || sessionsTable == "" || codesTable == "" {
		checks.bad("users / sessions / email_codes tables are missing", fmt.Errorf("missing"), "run eam migrate first")
		return
	}

	var users, unowned int
	if err := pool.QueryRow(ctx, `select count(*)::int from users`).Scan(&users); err != nil {
		checks.bad("failed to count users", err, "")
		return
	}
	if err := pool.QueryRow(ctx,
		`select count(*)::int from nodes where owner_id is null`).Scan(&unowned); err != nil {
		checks.bad("failed to count unowned files", err, "")
		return
	}
	checks.ok("user tables ready", fmt.Sprintf(" %d accounts", users))
	if unowned > 0 {
		checks.warn(fmt.Sprintf("%d legacy files have no owner; run: eam claim --email you@example.com", unowned))
	}
}

func checkCOS(ctx context.Context, cfg *config.Config, checks *doctor, writeTest bool) {
	objectStorage, err := cos.New(cfg.COS)
	if err != nil {
		checks.bad("failed to initialize COS client", err, "")
		return
	}
	probe := fmt.Sprintf("drive/__doctor__/probe-%d.txt", time.Now().UnixNano())

	info, err := objectStorage.Head(ctx, probe)
	if err != nil {
		checks.bad("headObject failed", err, "usually a bucket name without APPID, a wrong region, or a key without permission")
		return
	}
	if info != nil {
		checks.ok("auth OK, bucket reachable", " (unexpected: probe object already exists)")
	} else {
		checks.ok("auth OK, bucket reachable", " (probe object missing, as expected)")
	}

	credentials, err := objectStorage.TemporaryCredentials(ctx, probe, 900)
	if err != nil {
		checks.bad("STS credential issue failed", err, "the sub-account needs sts:GetFederationToken, or the key is not a valid API key")
	} else {
		expires := time.Unix(credentials.ExpiredTime, 0).Format(time.DateTime)
		checks.ok("STS credentials issued", " valid until "+expires)
	}

	if !writeTest {
		return
	}
	if err := objectStorage.PutEmpty(ctx, probe); err != nil {
		checks.bad("write failed", err, "check the sub-account has PutObject / DeleteObject permission")
		return
	}
	written, err := objectStorage.Head(ctx, probe)
	if err != nil || written == nil {
		checks.bad("failed to read back the written object", err, "")
		return
	}
	if err := objectStorage.Remove(ctx, probe); err != nil {
		checks.bad("failed to delete probe object", err, "")
		return
	}
	checks.ok("write / read / delete", fmt.Sprintf(" %d bytes", written.Size))
}
