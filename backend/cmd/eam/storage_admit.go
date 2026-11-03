package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/treescan"
	"github.com/jackc/pgx/v5"
)

const maxAdmissionEvidenceBytes = 1 << 20

type admitIndexEntry struct {
	Path   string
	Key    string
	Parent string
	Name   string
	Folder bool
	Size   int64
}

// runStorageAdmit establishes an initial budget only after an explicit operator
// assertion and complete read-only native/index comparison. It is not a writer
// lock, native capability experiment, or automatic startup backfill.
func runStorageAdmit(args []string) error {
	flags := flag.NewFlagSet("storage-admit", flag.ContinueOnError)
	owner := flags.String("owner-id", "", "existing owner UUID")
	quiesced := flags.Bool("writers-quiesced", false, "assert native/application/legacy writers are drained for the entire scan")
	proofFile := flags.String("evidence", "", "nonempty JSON operator proof document, at most 1 MiB")
	physicalLimit := flags.Int64("physical-limit", 0, "physical byte budget; zero means unlimited")
	physicalBytes := flags.Int64("physical-bytes", -1, "required for COS/S3: verified total current and retained-version bytes")
	dryRun := flags.Bool("dry-run", false, "verify baseline without database writes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	physicalProvided := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "physical-bytes" {
			physicalProvided = true
		}
	})
	if flags.NArg() != 0 || !baselineOwner.MatchString(*owner) || *proofFile == "" || *physicalLimit < 0 || (physicalProvided && (*physicalBytes < 0 || *physicalBytes > 9007199254740991)) {
		return fmt.Errorf("storage-admit requires --owner-id lowercase-UUID, --evidence and a nonnegative physical limit")
	}
	proof, err := readAdmissionEvidence(*proofFile)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	objects, err := validateAdmissionAccounting(cfg, proof, physicalProvided, *physicalBytes)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	readConfig := *cfg
	readConfig.Storage.CoordinatedWrites = false
	backend, err := storagebackend.New(&readConfig)
	if err != nil {
		return err
	}
	defer backend.Close()
	var objectValidation *storagebackend.Backend
	if objects {
		objectValidation, err = storagebackend.New(cfg)
		if err != nil {
			return fmt.Errorf("storage-admit object capability admission: %w", err)
		}
		defer objectValidation.Close()
	}
	createOnly := objectValidation != nil && objectValidation.CreateOnly()
	if err = validateAdmissionQuiescence(*quiesced, createOnly); err != nil {
		return err
	}
	binding, err := backend.Binding(*owner)
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	// No Usage() call here: all pre-initialization/dry-run queries stay in a
	// repeatable-read read-only transaction, with no advisory/fence state writes.
	tx, err := db.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	var exists bool
	if err = tx.QueryRow(ctx, `select exists(select 1 from users where id=$1::uuid)`, *owner).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("storage-admit owner does not exist")
	}
	driver, err := backend.Open(ctx, *owner)
	if err != nil {
		return err
	}
	defer driver.Close()
	root := fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: driver.Describe().Names.CaseSensitive, RetainsVersions: objects && !createOnly, CreateOnly: createOnly}
	var storedBinding []byte
	var usage fileops.Usage
	err = tx.QueryRow(ctx, `select root_binding,logical_bytes,logical_reserved,physical_bytes,physical_reserved,logical_limit,physical_limit from file_budgets where owner_id=$1::uuid`, *owner).Scan(&storedBinding, &usage.LogicalBytes, &usage.LogicalReserved, &usage.PhysicalBytes, &usage.PhysicalReserved, &usage.LogicalLimit, &usage.PhysicalLimit)
	if err == nil {
		var prior fileops.Root
		if json.Unmarshal(storedBinding, &prior) != nil || prior != root {
			return fmt.Errorf("storage-admit existing budget has a different root binding; explicit migration is required")
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Printf("budget already initialized; retained logical=%d reserved=%d physical=%d reserved=%d; no reset performed\n", usage.LogicalBytes, usage.LogicalReserved, usage.PhysicalBytes, usage.PhysicalReserved)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	started := time.Now().UTC()
	if !objects {
		if err = emptyAdmissionSystemRoot(ctx, cfg, binding); err != nil {
			return err
		}
	}
	scanned, err := treescan.Scan(ctx, driver, treescan.Options{Names: driver.Describe().Names})
	if err != nil {
		return fmt.Errorf("storage-admit native scan failed: %w", err)
	}
	records, err := scanned.Records()
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `select path,key,parent,name,is_dir,size from nodes where owner_id=$1::uuid order by path`, *owner)
	if err != nil {
		return err
	}
	index := make([]admitIndexEntry, 0)
	for rows.Next() {
		var item admitIndexEntry
		if err = rows.Scan(&item.Path, &item.Key, &item.Parent, &item.Name, &item.Folder, &item.Size); err != nil {
			rows.Close()
			return err
		}
		index = append(index, item)
		if len(index) > 1000000 {
			rows.Close()
			return fmt.Errorf("storage-admit index exceeds the bounded comparison limit")
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	logical, err := compareAdmissionEntries(*owner, records, index)
	if err != nil {
		return err
	}
	physical, err := admissionPhysicalBytes(objects, logical, *physicalBytes)
	if err != nil {
		return err
	}
	if objects {
		entries := make([]storage.Entry, len(records))
		for i := range records {
			entries[i] = records[i].Entry
		}
		if err = objectValidation.ValidateObjectBaseline(ctx, *owner, entries); err != nil {
			return fmt.Errorf("storage-admit object baseline: %w", err)
		}
	}
	// The operator assertion covers external/native writers; the second check only
	// catches an obvious staging change. It is never claimed to establish quiescence.
	if !objects {
		if err = emptyAdmissionSystemRoot(ctx, cfg, binding); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	evidence, err := json.Marshal(struct {
		Version         int             `json:"version"`
		Root            fileops.Root    `json:"root"`
		StartedAt       time.Time       `json:"startedAt"`
		FinishedAt      time.Time       `json:"finishedAt"`
		Entries         int             `json:"entries"`
		LogicalBytes    int64           `json:"logicalBytes"`
		PhysicalBytes   int64           `json:"physicalBytes"`
		RetainsVersions bool            `json:"retainsVersions"`
		OperatorProof   json.RawMessage `json:"operatorProof"`
	}{1, root, started, time.Now().UTC(), len(records), logical, physical, root.RetainsVersions, proof})
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("[dry-run] baseline verified: entries=%d logical=%d physical=%d; no database writes\n", len(records), logical, physical)
		return nil
	}
	if err = fileops.NewPostgres(db.Pool()).Initialize(ctx, root, fileops.Baseline{LogicalBytes: logical, PhysicalBytes: physical, LogicalLimit: cfg.QuotaBytes, PhysicalLimit: *physicalLimit, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: *quiesced, Evidence: evidence}); err != nil {
		return err
	}
	fmt.Printf("budget initialized: entries=%d logical=%d physical=%d; native files and index unchanged\n", len(records), logical, physical)
	return nil
}

func readAdmissionEvidence(path string) (json.RawMessage, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read admission evidence: %w", err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("admission evidence must be a regular JSON file")
	}
	if before.Size() > maxAdmissionEvidenceBytes {
		return nil, fmt.Errorf("admission evidence exceeds 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read admission evidence: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, fmt.Errorf("admission evidence must remain the same regular JSON file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxAdmissionEvidenceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxAdmissionEvidenceBytes {
		return nil, fmt.Errorf("admission evidence exceeds 1 MiB")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("admission evidence must be valid JSON")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, fmt.Errorf("admission evidence must contain exactly one JSON document")
	}
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			return nil, fmt.Errorf("admission evidence cannot be empty")
		}
	case []any:
		if len(v) == 0 {
			return nil, fmt.Errorf("admission evidence cannot be empty")
		}
	default:
		return nil, fmt.Errorf("admission evidence must be a nonempty JSON object or array")
	}
	return json.Marshal(value)
}

func emptyAdmissionSystemRoot(ctx context.Context, cfg *config.Config, binding storage.Config) error {
	factory, err := localstorage.NewFactory(localstorage.Options{CaseSensitive: cfg.Storage.Local.CaseSensitive})
	if err != nil {
		return err
	}
	reversed := binding
	reversed.ContentRoot, reversed.SystemRoot = binding.SystemRoot, binding.ContentRoot
	tree, err := factory.OpenOwner(ctx, reversed, cfg.Storage.Local.SystemRoot, cfg.Storage.Local.ContentRoot)
	if err != nil {
		return fmt.Errorf("storage-admit system root: %w", err)
	}
	defer tree.Close()
	page, err := tree.List(ctx, "", storage.PageRequest{Limit: 1})
	if err != nil {
		return fmt.Errorf("storage-admit system root: %w", err)
	}
	if len(page.Entries) != 0 || page.NextCursor != "" {
		return fmt.Errorf("storage-admit requires an empty system root; reconcile existing staging/intent records before initial admission (nothing was deleted)")
	}
	return nil
}

func compareAdmissionEntries(owner string, records []treescan.Record, index []admitIndexEntry) (int64, error) {
	native := make(map[string]storage.Entry, len(records))
	var logical int64
	for _, record := range records {
		item := record.Entry
		p := string(item.Path)
		if p == "" || storage.ValidatePath(item.Path) != nil || (item.Kind != storage.File && item.Kind != storage.Folder) || item.Size < 0 || (item.Kind == storage.Folder && item.Size != 0) {
			return 0, fmt.Errorf("storage-admit encountered an invalid native entry")
		}
		if _, ok := native[p]; ok {
			return 0, fmt.Errorf("storage-admit native enumeration repeated a path")
		}
		native[p] = item
		if item.Kind == storage.File {
			if item.Size > 9007199254740991-logical {
				return 0, fmt.Errorf("storage-admit native usage exceeds the supported integer limit")
			}
			logical += item.Size
		}
	}
	for p := range native {
		if parent := paths.ParentOf(p); parent != "" {
			ancestor, ok := native[parent]
			if !ok || ancestor.Kind != storage.Folder {
				return 0, fmt.Errorf("storage-admit native enumeration omits a folder ancestor at %q", parent)
			}
		}
	}
	seen := make(map[string]bool, len(index))
	for _, item := range index {
		if seen[item.Path] {
			return 0, fmt.Errorf("storage-admit index contains a duplicate path")
		}
		seen[item.Path] = true
		key := paths.KeyFor(item.Path, owner)
		if item.Folder {
			key = paths.DirKeyFor(item.Path, owner)
		}
		if item.Key != key || item.Parent != paths.ParentOf(item.Path) || item.Name != paths.NameOf(item.Path) {
			return 0, fmt.Errorf("storage-admit index mapping differs from exact owner/path spelling at %q", item.Path)
		}
		if item.Path == "" && item.Folder {
			if item.Size != 0 {
				return 0, fmt.Errorf("storage-admit root index entry has nonzero size")
			}
			continue
		}
		itemNative, ok := native[item.Path]
		if !ok {
			return 0, fmt.Errorf("storage-admit index-only path %q; repair the index before admission", item.Path)
		}
		if item.Folder != (itemNative.Kind == storage.Folder) || item.Size != itemNative.Size {
			return 0, fmt.Errorf("storage-admit native/index kind or size mismatch at %q", item.Path)
		}
		delete(native, item.Path)
	}
	if len(native) != 0 {
		// Report one exact spelling without printing metadata or the operator proof.
		smallest := ""
		for p := range native {
			if smallest == "" || strings.Compare(p, smallest) < 0 {
				smallest = p
			}
		}
		return 0, fmt.Errorf("storage-admit native path %q is not indexed; complete a safe reconcile before admission", smallest)
	}
	return logical, nil
}
