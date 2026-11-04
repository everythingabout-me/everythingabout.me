package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
)

var resolveOperationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// runStorageResolveUpload finishes cleanup for a terminal upload whose discarded
// allocation still keeps an unconfirmed part receipt. A receiver that dies between its
// part request and the reply leaves that receipt permanently uncertain, so ordinary
// cleanup retains the physical liability and retries forever. This command proves with
// the provider that the allocation no longer exists and then lets the ordinary cleanup
// release the reservation. It runs the same discard/cleanup path as the server worker;
// publication, completion, upload and index commits are unreachable from here.
func runStorageResolveUpload(args []string) error {
	flags := flag.NewFlagSet("storage-resolve-upload", flag.ContinueOnError)
	operation := flags.String("operation-id", "", "required operation UUID of a terminal upload_commit")
	dryRun := flags.Bool("dry-run", false, "inspect journal and provider state without writes")
	timeout := flags.Duration("timeout", 2*time.Minute, "total command timeout (positive and at most 1h)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !resolveOperationID.MatchString(*operation) || *timeout <= 0 || *timeout > time.Hour {
		return fmt.Errorf("storage-resolve-upload requires --operation-id lowercase-UUID and a positive timeout of at most 1h")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.Storage.CoordinatedWrites {
		return fmt.Errorf("storage-resolve-upload requires coordinated writes; this deployment has no durable upload journal")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to index database: %w", err)
	}
	defer db.Close()
	var ownerID string
	var snapshot []byte
	err = db.Pool().QueryRow(ctx, `select owner_id::text, snapshot from file_operations where id=$1::uuid`, *operation).Scan(&ownerID, &snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("operation %s does not exist", *operation)
	}
	if err != nil {
		return fmt.Errorf("read operation %s: %w", *operation, err)
	}
	// The snapshot is the journal's own operation document; the exact root comes from it
	// and every later journal call re-verifies that root and the owner actor.
	var recorded struct {
		Root  fileops.Root
		State fileops.State
		Phase fileops.Phase
	}
	if err = json.Unmarshal(snapshot, &recorded); err != nil {
		return fmt.Errorf("decode operation %s: %w", *operation, err)
	}
	journal := fileops.NewPostgres(db.Pool())
	actor := fileops.Actor{ID: "owner:" + ownerID, CanRead: true, CanWrite: true, ManageOwner: true}
	op, err := journal.Get(ctx, recorded.Root, actor, *operation)
	if err != nil {
		return fmt.Errorf("load operation %s: %w", *operation, err)
	}
	if op.Kind != "upload_commit" {
		return fmt.Errorf("operation %s is %q; only upload_commit has a numbered allocation to reconcile", *operation, op.Kind)
	}
	if !op.Terminal() {
		return fmt.Errorf("operation %s is %s/%s; only a terminal operation can be resolved", *operation, op.State, op.Phase)
	}
	var uploadState string
	err = db.Pool().QueryRow(ctx, `select state from file_uploads where operation_id=$1::uuid`, *operation).Scan(&uploadState)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("operation %s has no durable upload session", *operation)
	}
	if err != nil {
		return fmt.Errorf("read upload session of %s: %w", *operation, err)
	}
	if op.CleanedAt != nil {
		fmt.Printf("operation %s is already resolved\n", *operation)
		return nil
	}
	var retained int64
	if err = db.Pool().QueryRow(ctx, `select physical_reserved from file_reservations where operation_id=$1::uuid`, *operation).Scan(&retained); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read physical reservation of %s: %w", *operation, err)
	}
	target := ""
	var intent struct{ Target string }
	if json.Unmarshal(op.Intent, &intent) == nil {
		target = intent.Target
	}
	fmt.Printf("operation %s upload_commit %s/%s\n", *operation, op.State, op.Phase)
	fmt.Printf("  target      %s\n", target)
	fmt.Printf("  upload      %s\n", uploadState)
	fmt.Printf("  retained    %d physical bytes\n", retained)
	backend, err := storagebackend.New(cfg)
	if err != nil {
		return err
	}
	defer backend.Close()
	driver, err := backend.Open(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("open owner storage: %w", err)
	}
	defer driver.Close()
	native, ok := driver.(fileops.NativeActions)
	if !ok {
		return fmt.Errorf("storage driver does not expose native file actions")
	}
	numbered, ok := native.(storage.NumberedUploadWriter)
	if !ok {
		return fmt.Errorf("storage driver has no numbered upload allocation to reconcile")
	}
	binding := native.StorageBinding()
	if binding.OwnerID != op.Root.OwnerID || binding.ConnectionID != op.Root.ConnectionID || binding.Generation != op.Root.Generation || binding.ContentRoot != op.Root.ContentRoot || binding.SystemRoot != op.Root.SystemRoot || native.StorageNameRules().CaseSensitive != op.Root.CaseSensitive || native.Describe().RetainsVersions != op.Root.RetainsVersions {
		return fmt.Errorf("current storage binding does not match operation %s; refusing to reconcile another root", *operation)
	}
	writer := fileops.NewNativeUploadWriter(native, numbered, journal)
	// Resolution reaches only DiscardParts and MarkCleaned, which never authorize or
	// commit. Any future path that does fails closed instead of bypassing owner policy.
	service, err := fileops.NewService(journal, fileops.ServiceOptions{
		Authorize: func(context.Context, fileops.Operation) error { return fileops.E("forbidden", nil) },
		Commit: func(context.Context, pgx.Tx, fileops.Operation, *storage.MutationResult) (json.RawMessage, error) {
			return nil, fileops.E("forbidden", nil)
		},
	})
	if err != nil {
		return err
	}
	evidence, err := service.ReconcileDiscard(ctx, recorded.Root, actor, *operation, writer, !*dryRun)
	if err != nil {
		return err
	}
	report := func(evidence storage.DiscardEvidence) {
		if evidence.UploadAbsent {
			fmt.Printf("  provider    no such multipart upload session\n")
			return
		}
		fmt.Printf("  provider    still holds the upload session (%d parts reported)\n", evidence.Parts)
	}
	if *dryRun {
		report(evidence)
		if !evidence.UploadAbsent {
			return fmt.Errorf("provider still holds the allocation; nothing was resolved")
		}
		fmt.Printf("dry-run: would record the discard proof and release %d physical bytes; rerun without --dry-run to apply\n", retained)
		return nil
	}
	if evidence.UploadAbsent {
		report(evidence)
	}
	resolved, err := journal.Get(ctx, recorded.Root, actor, *operation)
	if err != nil {
		return err
	}
	if resolved.CleanedAt == nil {
		return fmt.Errorf("operation %s is still unresolved", *operation)
	}
	fmt.Printf("resolved: %d physical bytes released and operation cleaned\n", retained)
	return nil
}
