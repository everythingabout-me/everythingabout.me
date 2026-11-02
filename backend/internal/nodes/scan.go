package nodes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/treescan"
)

// ScanDB allows an owner-scoped index import on a pool or a dedicated connection.
type ScanDB interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}
type ScanApplyOptions struct{ DryRun bool }
type ScanApplyReport struct{ Scanned, Inserted, Updated, Unchanged int }

// ApplyScan imports a successful scan in one serializable transaction. It never
// deletes rows, changes node IDs/application metadata, or touches native storage.
// NativeRef is deliberately not a database key: keys retain the private logical
// drive/<owner>/<path> mapping even when the driver uses local paths/native IDs.
// Dry-run uses a read-only transaction and issues no database writes. A complete
// scan is enumeration evidence only; operators must coordinate external writers
// when they need a stable point-in-time import.
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
	// The session namespace lock precedes the serializable snapshot. Taking a
	// transaction advisory lock as the first SELECT would establish a snapshot
	// before the lock and could miss a deletion that just committed.
	pending, err := tx.Query(ctx, `select from_path from fs_jobs where user_id=$1::uuid and op='delete' and status<>'done'`, ownerID)
	if err != nil {
		return report, err
	}
	var roots []string
	for pending.Next() {
		var root string
		if err := pending.Scan(&root); err != nil {
			pending.Close()
			return report, err
		}
		roots = append(roots, norm.NFC.String(root))
	}
	pending.Close()
	if err := pending.Err(); err != nil {
		return report, err
	}
	for _, record := range records {
		p := norm.NFC.String(string(record.Entry.Path))
		for _, root := range roots {
			if p == root || strings.HasPrefix(p, root+"/") {
				return report, fmt.Errorf("scan path %q is pending permanent deletion; finish deletion before reconciling this subtree", record.Entry.Path)
			}
		}
	}
	// Existing paths participate in preflight because no-prune imports retain them.
	rows, err := tx.Query(ctx, `select path, is_dir from nodes where owner_id = $1::uuid`, ownerID)
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
			err := tx.QueryRow(ctx, `select exists(select 1 from nodes where owner_id=$1::uuid and path=$3),
     exists(select 1 from nodes where owner_id=$1::uuid and path=$3 and (
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
		var inserted bool
		err := tx.QueryRow(ctx, `insert into nodes
    (owner_id,key,path,parent,name,is_dir,size,mime,etag,storage_class,source_modified_at)
    values ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
    on conflict (owner_id,path) do update set
      key=excluded.key, parent=excluded.parent, name=excluded.name,
      size=excluded.size, mime=coalesce(excluded.mime,nodes.mime),
      etag=coalesce(excluded.etag,nodes.etag),
      storage_class=coalesce(excluded.storage_class,nodes.storage_class),
      source_modified_at=coalesce(excluded.source_modified_at,nodes.source_modified_at),
      updated_at=now()
    where nodes.is_dir=excluded.is_dir and (
      nodes.key is distinct from excluded.key or nodes.parent is distinct from excluded.parent
      or nodes.name is distinct from excluded.name or nodes.size is distinct from excluded.size
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
		if inserted {
			report.Inserted++
		} else {
			report.Updated++
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

// LockScanNamespace retains one connection so the admission lock is acquired
// before beginning the serializable transaction, then released on that session.
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
	return conn, func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `select pg_advisory_unlock(hashtextextended($1,0))`, key); err != nil {
			// A session lock must never escape back into a shared pool.
			_ = conn.Close(context.Background())
		}
		release()
	}, nil
}
