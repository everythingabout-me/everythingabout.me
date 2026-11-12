package nodes

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/treescan"
)

// ScanDB allows an owner-scoped index import on a pool or a dedicated connection.
type ScanDB interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}
type ScanApplyOptions struct {
	DryRun bool
	// ChangeRoot binds metadata revisions and external change records to the same
	// namespace as the file API. Production reconcile always supplies it; nil is
	// reserved for index-only utilities/tests with no configured storage binding.
	ChangeRoot *fileops.Root
	// Sync makes the index equal the scan instead of only adding to it: entries the
	// native tree no longer has are removed. The native tree is authoritative and the
	// index is a derived view, so a refresh that cannot remove is not a refresh.
	Sync bool
}

// maxReportedRemovals bounds the dry-run list handed to a CLI or API caller.
const maxReportedRemovals = 1000

type ScanApplyReport struct {
	Scanned, Inserted, Updated, Unchanged, Removed int
	// RemovedPaths carries the dry-run preview; it is empty once rows are deleted.
	RemovedPaths []string
}

// ApplyScan imports a successful scan in one serializable transaction. It never
// changes node IDs/application metadata or touches native storage; with Sync it
// removes the index rows the tree no longer has.
// NativeRef is deliberately not a database key: keys retain the private logical
// drive/<owner>/<path> mapping even when the driver uses local paths/native IDs.
// Dry-run uses a read-only transaction and issues no database writes.
//
// The serializable transaction is not isolation from live writers by itself: a
// write that commits after the native walk is invisible to this snapshot yet
// absent from the scanned records. Callers that share the owner with writers must
// hold LockScanNamespace across the walk and this import; it takes the same owner
// lock every fileops transaction takes.
func ApplyScan(ctx context.Context, db ScanDB, ownerID string, result *treescan.Result, opts ScanApplyOptions) (ScanApplyReport, error) {
	var report ScanApplyReport
	var owner pgtype.UUID
	if err := owner.Scan(ownerID); err != nil || !owner.Valid {
		return report, fmt.Errorf("scan import requires a UUID owner")
	}
	// Use canonical UUID spelling for the private key namespace.
	value, err := owner.Value()
	if err != nil {
		return report, err
	}
	ownerID = value.(string)
	if root := opts.ChangeRoot; root != nil {
		if root.OwnerID != ownerID || root.ConnectionID == "" || root.Generation < 1 || root.ContentRoot == "" || root.SystemRoot == "" || root.ContentRoot == root.SystemRoot {
			return report, fmt.Errorf("scan changes require the owner's configured storage root")
		}
	}
	records, err := result.Records()
	if err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if db == nil {
		return report, fmt.Errorf("scan import database is required")
	}
	conn, unlock, err := LockScanNamespace(ctx, db, ownerID)
	if err != nil {
		return report, err
	}
	defer unlock()
	txOpts := pgx.TxOptions{IsoLevel: pgx.Serializable}
	if opts.DryRun {
		txOpts.AccessMode = pgx.ReadOnly
	}
	tx, err := conn.BeginTx(ctx, txOpts)
	if err != nil {
		return report, err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if opts.ChangeRoot != nil && !opts.DryRun {
		if _, err := filechanges.BindRoot(ctx, tx, ownerID, fileRootNamespace(*opts.ChangeRoot)); err != nil {
			return report, fmt.Errorf("bind scan change root: %w", err)
		}
		if _, err := tx.Exec(ctx, configureNameComparisonSQL, ownerID, nameComparisonVersion(opts.ChangeRoot.CaseSensitive)); err != nil {
			return report, fmt.Errorf("configure scan name comparison: %w", err)
		}
	}
	// The session namespace lock precedes the serializable snapshot. Taking a
	// transaction advisory lock as the first SELECT would establish a snapshot
	// before the lock and could miss a deletion that just committed.
	//
	// A delete that is still purging is exactly an accepted operation whose index
	// rows are already gone but whose native objects may still exist, so a scan
	// that imported them would resurrect paths the user deleted. The operation
	// journal is the source of truth for that state; the retired fs_jobs table was
	// consulted here and could not see a /api/files delete at all.
	namespace := ""
	if opts.ChangeRoot != nil {
		namespace = opts.ChangeRoot.Namespace()
	}
	pending, err := fileops.PendingDeleteRoots(ctx, tx, ownerID, namespace)
	if err != nil {
		return report, fmt.Errorf("read pending deletions for scan import: %w", err)
	}
	for _, record := range records {
		p := norm.NFC.String(string(record.Entry.Path))
		for _, root := range pending {
			root = norm.NFC.String(root)
			if p == root || strings.HasPrefix(p, root+"/") {
				return report, fmt.Errorf("scan path %q is pending permanent deletion; finish deletion before reconciling this subtree", record.Entry.Path)
			}
		}
	}
	// Existing paths participate in preflight because no-prune imports retain them.
	rows, err := tx.Query(ctx, `select path, is_dir from file_entries where owner_id = $1::uuid`, ownerID)
	if err != nil {
		return report, err
	}
	existing := make([]storage.Entry, 0)
	for rows.Next() {
		var path string
		var folder bool
		if err := rows.Scan(&path, &folder); err != nil {
			rows.Close()
			return report, err
		}
		kind := storage.File
		if folder {
			kind = storage.Folder
		}
		existing = append(existing, storage.Entry{Path: storage.Path(path), Kind: kind})
		if len(existing) > 5000000 {
			rows.Close()
			return report, fmt.Errorf("retained index exceeds the bounded import limit")
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	if err := result.CheckIndex(existing); err != nil {
		return report, err
	}

	var changes []scanChangeCapture
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return ScanApplyReport{}, err
		}
		entry := record.Entry
		p := string(entry.Path)
		folder := entry.Kind == storage.Folder
		key := paths.KeyFor(p, ownerID)
		if folder {
			key = paths.DirKeyFor(p, ownerID)
		}
		var modified *time.Time
		if !entry.ModifiedAt.IsZero() {
			v := entry.ModifiedAt
			modified = &v
		}
		var mime *string
		if entry.MediaType != "" {
			v := entry.MediaType
			mime = &v
		}
		// Unknown source attributes preserve prior values. Repeated unchanged imports
		// do not UPDATE, preserving updated_at as well as event/annotation references.
		args := []any{ownerID, key, p, paths.ParentOf(p), paths.NameOf(p), folder, entry.Size, mime, record.ETag, record.StorageClass, modified}
		report.Scanned++
		if opts.DryRun {
			var exists, changed bool
			err := tx.QueryRow(ctx, `select exists(select 1 from file_entries where owner_id=$1::uuid and path=$3),
     exists(select 1 from file_entries where owner_id=$1::uuid and path=$3 and (
       key is distinct from $2::text or parent is distinct from $4::text or name is distinct from $5::text
       or is_dir is distinct from $6::boolean or size is distinct from $7::bigint
       or mime is distinct from coalesce($8::text,mime)
       or etag is distinct from coalesce($9::text,etag)
       or storage_class is distinct from coalesce($10::text,storage_class)
       or source_modified_at is distinct from coalesce($11::timestamptz,source_modified_at)))`, args...).Scan(&exists, &changed)
			if err != nil {
				return ScanApplyReport{}, err
			}
			if !exists {
				report.Inserted++
			} else if changed {
				report.Updated++
			} else {
				report.Unchanged++
			}
			continue
		}
		var before capturedFileState
		if opts.ChangeRoot != nil {
			before, err = captureFileState(ctx, tx, ownerID, fileRootNamespace(*opts.ChangeRoot), p)
			if err != nil {
				return ScanApplyReport{}, fmt.Errorf("capture scanned entry %q: %w", p, err)
			}
		}
		var inserted bool
		err := tx.QueryRow(ctx, `insert into nodes
    (owner_id,key,path,parent,name,is_dir,size,mime,etag,storage_class,source_modified_at)
    values ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
    on conflict (owner_id,parent_id,comparison_name) do update set
      path=excluded.path, key=excluded.key, parent=excluded.parent, name=excluded.name,
      size=excluded.size, mime=coalesce(excluded.mime,nodes.mime),
      etag=coalesce(excluded.etag,nodes.etag),
      storage_class=coalesce(excluded.storage_class,nodes.storage_class),
      source_modified_at=coalesce(excluded.source_modified_at,nodes.source_modified_at),
      updated_at=now()
    where nodes.is_dir=excluded.is_dir and (
      nodes.name is distinct from excluded.name or nodes.size is distinct from excluded.size
      or nodes.mime is distinct from coalesce(excluded.mime,nodes.mime)
      or nodes.etag is distinct from coalesce(excluded.etag,nodes.etag)
      or nodes.storage_class is distinct from coalesce(excluded.storage_class,nodes.storage_class)
      or nodes.source_modified_at is distinct from coalesce(excluded.source_modified_at,nodes.source_modified_at))
    returning (xmax=0)`, args...).Scan(&inserted)
		if err == pgx.ErrNoRows {
			report.Unchanged++
			continue
		}
		if err != nil {
			return ScanApplyReport{}, fmt.Errorf("import %q: %w", p, err)
		}
		// A changed imported observation invalidates any saved native generation.
		// Browsing must not attach the old content revision to external replacements.
		if _, err := tx.Exec(ctx, `delete from file_content_generations where owner_id=$1::uuid and (node_id=file_tree_resolve($1::uuid,$2) or (node_id is null and path=$2))`, ownerID, p); err != nil {
			return ScanApplyReport{}, fmt.Errorf("invalidate scanned content revision %q: %w", p, err)
		}
		if opts.ChangeRoot != nil {
			changes = append(changes, scanChangeCapture{path: p, before: before})
		}
		if inserted {
			report.Inserted++
		} else {
			report.Updated++
		}
	}
	if opts.Sync {
		deletions, err := removeAbsentEntries(ctx, tx, ownerID, absentEntries(existing, records), opts, &report)
		if err != nil {
			return report, err
		}
		changes = append(changes, deletions...)
	}
	if opts.ChangeRoot != nil && len(changes) > 0 {
		if err := settleScanChanges(ctx, tx, *opts.ChangeRoot, changes); err != nil {
			return ScanApplyReport{}, fmt.Errorf("record scan changes: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return ScanApplyReport{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ScanApplyReport{}, fmt.Errorf("commit scan index (retry after coordinating writers): %w", err)
	}
	return report, nil
}

// absentEntries lists the indexed entries the complete scan did not observe,
// deepest first so a removed folder's children leave before the folder itself.
func absentEntries(existing []storage.Entry, records []treescan.Record) []storage.Entry {
	present := make(map[string]struct{}, len(records))
	for _, record := range records {
		present[norm.NFC.String(string(record.Entry.Path))] = struct{}{}
	}
	absent := make([]storage.Entry, 0, len(existing))
	for _, entry := range existing {
		if entry.Path == "" {
			// The owner root is identity, not a scanned child.
			continue
		}
		if _, ok := present[norm.NFC.String(string(entry.Path))]; !ok {
			absent = append(absent, entry)
		}
	}
	sort.Slice(absent, func(i, j int) bool {
		if len(absent[i].Path) != len(absent[j].Path) {
			return len(absent[i].Path) > len(absent[j].Path)
		}
		return absent[i].Path > absent[j].Path
	})
	return absent
}

// removeAbsentEntries deletes index rows for entries the native tree no longer
// has. The audit record is written before the row disappears: the audit trail is
// the only durable account of what a sync removed.
func removeAbsentEntries(ctx context.Context, tx pgx.Tx, ownerID string, absent []storage.Entry, opts ScanApplyOptions, report *ScanApplyReport) ([]scanChangeCapture, error) {
	deletions := make([]scanChangeCapture, 0, len(absent))
	for _, entry := range absent {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := string(entry.Path)
		if opts.DryRun {
			report.Removed++
			if len(report.RemovedPaths) < maxReportedRemovals {
				report.RemovedPaths = append(report.RemovedPaths, p)
			}
			continue
		}
		var before capturedFileState
		if opts.ChangeRoot != nil {
			var err error
			before, err = captureFileState(ctx, tx, ownerID, fileRootNamespace(*opts.ChangeRoot), p)
			if err != nil {
				return nil, fmt.Errorf("capture removed entry %q: %w", p, err)
			}
			if before.metadata == nil {
				// Already gone from the index: nothing to remove or record.
				continue
			}
		}
		if _, err := tx.Exec(ctx, `delete from nodes where owner_id=$1::uuid and id=file_tree_resolve($1::uuid,$2)`, ownerID, p); err != nil {
			return nil, fmt.Errorf("remove %q: %w", p, err)
		}
		report.Removed++
		if before.metadata != nil {
			entryID := before.metadata.ID
			// Audit paths are API paths (leading slash); the index path is relative.
			relative := fc.Path("/" + p)
			allowed := true
			if err := filechanges.RecordAudit(ctx, tx, &filechanges.Audit{
				OwnerID:    ownerID,
				Action:     "file.deleted",
				Outcome:    "confirmed",
				Allowed:    &allowed,
				Status:     200,
				ReasonCode: "native_absent",
				EntryID:    &entryID,
				Path:       &relative,
				Actor:      filechanges.Actor{Kind: "external"},
				Origin:     "external",
			}); err != nil {
				return nil, fmt.Errorf("record removal audit %q: %w", p, err)
			}
			deletions = append(deletions, scanChangeCapture{path: p, before: before, deleted: true})
		}
	}
	return deletions, nil
}

// LockScanNamespace retains one connection for the admission locks a scan needs
// before it enumerates and applies, then releases them on that session:
//
//   - "browser-write:"+owner, the legacy namespace lock, first;
//   - the owner's write-admission lock (fileops.OwnerLockKey) second.
//
// Every fileops transaction takes the owner lock before its change-root and
// index locks, and no path that holds the owner lock acquires the namespace
// lock, so namespace -> owner matches the rest of the service and cannot
// deadlock.
//
// The owner lock must be held across the native walk AND the import, not only by
// the import transaction. A SERIALIZABLE snapshot cannot exclude a write that
// settles after the walk: an upload committed in that window looks absent to a
// Sync import and its row is pruned, while a delete committed there has already
// removed rows a stale walk still reports, so the import re-inserts them. Taking
// the owner lock as a transaction lock inside ApplyScan would also establish the
// snapshot before the wait, so this session lock is taken before BeginTx.
func LockScanNamespace(ctx context.Context, db ScanDB, ownerID string) (*pgx.Conn, func(), error) {
	var conn *pgx.Conn
	release := func() {}
	switch value := db.(type) {
	case *pgxpool.Pool:
		pooled, err := value.Acquire(ctx)
		if err != nil {
			return nil, nil, err
		}
		conn = pooled.Conn()
		release = pooled.Release
	case *pgxpool.Conn:
		conn = value.Conn()
	case *pgx.Conn:
		conn = value
	default:
		return nil, nil, fmt.Errorf("scan import requires a dedicated Postgres connection or pool for namespace coordination")
	}
	key := "browser-write:" + ownerID
	var acquired bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1,0))`, key).Scan(&acquired); err != nil {
		release()
		return nil, nil, err
	}
	if !acquired {
		release()
		return nil, nil, fmt.Errorf("another file change is in progress; retry reconcile shortly")
	}
	unlockNamespace := func() error {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := conn.Exec(unlockCtx, `select pg_advisory_unlock(hashtextextended($1,0))`, key)
		return err
	}
	var admitted bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock(`+fileops.OwnerLockKey+`)`, ownerID).Scan(&admitted); err != nil {
		if unlockErr := unlockNamespace(); unlockErr != nil {
			_ = conn.Close(context.Background())
		}
		release()
		return nil, nil, err
	}
	if !admitted {
		if unlockErr := unlockNamespace(); unlockErr != nil {
			_ = conn.Close(context.Background())
		}
		release()
		return nil, nil, fmt.Errorf("a file write is in progress; retry reconcile shortly")
	}
	return conn, func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Release in reverse acquisition order: write admission, then namespace.
		if _, err := conn.Exec(unlockCtx, `select pg_advisory_unlock(`+fileops.OwnerLockKey+`)`, ownerID); err != nil {
			// A session lock must never escape back into a shared pool.
			_ = conn.Close(context.Background())
			release()
			return
		}
		if _, err := conn.Exec(unlockCtx, `select pg_advisory_unlock(hashtextextended($1,0))`, key); err != nil {
			_ = conn.Close(context.Background())
		}
		release()
	}, nil
}
