package main

import (
	"context"
	"flag"
	"fmt"
	"regexp"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
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
	writeTest := flags.Bool("write", false, "also test legacy COS write + delete")
	ownerID := flags.String("owner-id", "", "owner UUID for native tree checks on local/S3 backends")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	checks := &doctor{}

	fmt.Println("\nconfig")
	fmt.Printf("  storage  %s\n", cfg.BackendName())
	if cfg.BackendName() == "cos" {
		fmt.Printf("  bucket   %s\n", cfg.COS.Bucket)
		fmt.Printf("  region   %s\n", cfg.COS.Region)
	}
	if cfg.AppSecret == "" {
		checks.bad("application secret missing", fmt.Errorf("APP_SECRET is required for server startup"), "preserve the previous signing seed explicitly before changing backend")
	}
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

	fmt.Println("\nstorage")
	if cfg.BackendName() == "cos" {
		if err := cfg.ValidateCOS(); err != nil {
			checks.bad("incomplete COS config", err, "set COS_SECRET_ID / COS_SECRET_KEY / COS_BUCKET / COS_REGION")
		} else {
			checkCOS(ctx, cfg, checks, *writeTest)
		}
	} else {
		checkNativeStorage(ctx, cfg, checks, *ownerID, *writeTest)
	}

	fmt.Println("\nstorage roots")
	rootDB := db
	if dbErr != nil {
		rootDB = nil
	}
	checkStorageRoots(ctx, cfg, rootDB, checks)

	if checks.failed == 0 {
		fmt.Print("\nAll checks passed.\n\n")
		return nil
	}
	fmt.Printf("\n%d check(s) failed.\n", checks.failed)
	return fmt.Errorf("%d check(s) failed", checks.failed)
}

func checkNativeStorage(ctx context.Context, cfg *config.Config, checks *doctor, ownerID string, writeTest bool) {
	if writeTest {
		checks.bad("write probe unavailable", fmt.Errorf("P1 native backends are read-only"), "use isolated P0 capability probes; native publication ships after durable mutation coordination")
		return
	}
	backend, err := storagebackend.New(cfg)
	if err != nil {
		checks.bad("storage configuration", err, "check the selected backend configuration and existing root directories")
		return
	}
	defer backend.Close()
	if ownerID == "" {
		checks.warn("backend configuration valid; pass --owner-id UUID to verify a native owner root")
		return
	}
	driver, err := backend.Open(ctx, ownerID)
	if err != nil {
		checks.bad("owner root binding", err, "create and verify the separate content/system owner directories")
		return
	}
	defer driver.Close()
	entry, err := driver.Stat(ctx, "")
	if err != nil || entry.Kind != storage.Folder {
		checks.bad("native root", fmt.Errorf("root check failed: %v", err), "")
		return
	}
	page, err := driver.List(ctx, "", storage.PageRequest{Limit: 1})
	if err != nil {
		checks.bad("native child listing", err, "resolve ambiguous or unsupported native names before import")
		return
	}
	checks.ok("native root and child listing", fmt.Sprintf(" %d sampled children; read-only", len(page.Entries)))
}

// checkStorageRoots surfaces at `eam doctor` time what the server refuses to start
// with: admission evidence that belongs to another connection, and owner roots that
// no longer match the tree the current configuration selects.
func checkStorageRoots(ctx context.Context, cfg *config.Config, db *store.DB, checks *doctor) {
	backend, err := storagebackend.New(cfg)
	if err != nil {
		checks.bad("configuration and admission evidence", err, "probe this exact connection; evidence recorded against another bucket or region does not apply")
		return
	}
	defer backend.Close()
	checks.ok("configuration and admission evidence", "")
	if db == nil {
		checks.warn("owner roots not compared: database unavailable")
		return
	}
	if err := validateStorageRoots(ctx, db, backend); err != nil {
		checks.bad("owner roots match the database index", err, "do not start the server: this connection selects a different native tree")
		return
	}
	checks.ok("owner roots match the database index", "")
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
