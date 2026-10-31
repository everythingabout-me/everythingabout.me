package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"path/filepath"
	"time"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

type uploadSnapshot struct {
	tx       pgx.Tx
	existing *Row
	key      string
	info     *storage.ObjectInfo
}

func rollbackUpload(ctx context.Context, tx pgx.Tx) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(rollbackCtx)
}

// beginUploadSnapshot locks before HEAD, so a deletion or replacement cannot
// commit a newer index row and then be overwritten with our older observation.
// A serialization error must restart the whole operation, including HEAD.
func (s *Service) beginUploadSnapshot(ctx context.Context, ownerID, path string, isDir bool) (*uploadSnapshot, error) {
	if normalized, err := paths.Normalize(path); err != nil || normalized != path || path == "" {
		return nil, fmt.Errorf("upload indexing requires a normalized non-root path")
	}
	tx, err := s.db.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	ready := false
	defer func() {
		if !ready {
			rollbackUpload(ctx, tx)
		}
	}()
	// Use a consistent order and include every existing ancestor and target.
	lockedPaths := append(paths.AncestorsOf(path), path)
	rows, err := tx.Query(ctx, `select path from nodes where owner_id=$1::uuid and path=any($2::text[]) order by path for update`, ownerID, lockedPaths)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var locked string
		if err := rows.Scan(&locked); err != nil {
			rows.Close()
			return nil, err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	existing, err := scanNode(tx.QueryRow(ctx, `select `+nodeColumns+` from nodes where owner_id=$1::uuid and path=$2`, ownerID, path))
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.IsDir != isDir {
		return nil, httpx.Conflict("upload conflicts with the indexed entry kind")
	}
	key := paths.KeyFor(path, ownerID)
	if isDir {
		key = paths.DirKeyFor(path, ownerID)
	}
	info, err := s.store.Head(ctx, key)
	if err != nil {
		return nil, err
	}
	if info != nil && info.Key != "" && info.Key != key {
		return nil, fmt.Errorf("upload indexing returned a different object key")
	}
	ready = true
	return &uploadSnapshot{tx: tx, existing: existing, key: key, info: info}, nil
}

func ensureUploadParents(ctx context.Context, tx pgx.Tx, ownerID, path string) (bool, error) {
	changed := false
	for _, dir := range paths.AncestorsOf(path) {
		tag, err := tx.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir)
			values($1::uuid,$2,$3,$4,$5,true) on conflict(owner_id,path) do nothing`,
			ownerID, paths.DirKeyFor(dir, ownerID), dir, paths.ParentOf(dir), paths.NameOf(dir))
		if err != nil {
			return false, err
		}
		changed = changed || tag.RowsAffected() > 0
	}
	return changed, nil
}

// ObserveUpload repairs the index from a current native object, never from an
// upload intention. Missing objects stay watched without creating index rows.
func (s *Service) ObserveUpload(ctx context.Context, ownerID, path string, isDir bool) (bool, error) {
	snapshot, err := s.beginUploadSnapshot(ctx, ownerID, path, isDir)
	if err != nil {
		return false, err
	}
	defer rollbackUpload(ctx, snapshot.tx)
	tx, existing, info := snapshot.tx, snapshot.existing, snapshot.info
	if info == nil {
		return false, tx.Commit(ctx)
	}
	// Repair implicit ancestors even when the target representation is unchanged.
	// A concurrent subtree deletion can leave an index child whose native object
	// still exists but whose parent row was removed.
	parentsChanged, err := ensureUploadParents(ctx, tx, ownerID, path)
	if err != nil {
		return false, err
	}
	if existing != nil && (isDir || (existing.Size == info.Size && stringValue(existing.ETag) == info.ETag && stringValue(existing.StorageClass) == info.StorageClass)) {
		// Preserve accepted File.lastModified, MIME and checksum without churning
		// the target's updated_at when the same representation is observed again.
		return parentsChanged, tx.Commit(ctx)
	}
	var sourceModified *time.Time
	if !info.LastModified.IsZero() {
		sourceModified = &info.LastModified
	}
	mediaType := info.ContentType
	if mediaType == "" && !isDir {
		mediaType = mime.TypeByExtension(filepath.Ext(path))
	}
	var media *string
	if mediaType != "" {
		media = &mediaType
	}
	_, err = tx.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime,etag,storage_class,source_modified_at)
		values($1::uuid,$2,$3,$4,$5,$6,$7,$8,nullif($9,''),nullif($10,''),$11)
		on conflict(owner_id,path) do update set
		  key=excluded.key, size=excluded.size, mime=excluded.mime, etag=excluded.etag,
		  storage_class=excluded.storage_class, source_modified_at=excluded.source_modified_at,
		  meta=nodes.meta - 'sha256' - 'mime', updated_at=now()`,
		ownerID, snapshot.key, path, paths.ParentOf(path), paths.NameOf(path), isDir, info.Size, media, info.ETag, info.StorageClass, sourceModified)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ConfirmUpload verifies a complete native object and applies accepted client
// metadata in the same transaction as its parent/index writes. Size, ETag and
// storage class always come from HEAD rather than the client's intention.
func (s *Service) ConfirmUpload(ctx context.Context, input UpsertInput) (*Row, error) {
	snapshot, err := s.beginUploadSnapshot(ctx, input.OwnerID, input.Path, false)
	if err != nil {
		return nil, err
	}
	defer rollbackUpload(ctx, snapshot.tx)
	tx, info := snapshot.tx, snapshot.info
	if info == nil {
		return nil, httpx.BadRequest("object not found; the upload may not be complete")
	}
	if _, err := ensureUploadParents(ctx, tx, input.OwnerID, input.Path); err != nil {
		return nil, err
	}
	meta := input.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	modified := input.SourceModifiedAt
	if modified == nil && !info.LastModified.IsZero() {
		modified = &info.LastModified
	}
	node, err := scanNode(tx.QueryRow(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime,etag,storage_class,source_modified_at,meta)
		values($1::uuid,$2,$3,$4,$5,false,$6,$7,nullif($8,''),nullif($9,''),$10,$11::jsonb)
		on conflict(owner_id,path) do update set
		  key=excluded.key, size=excluded.size, mime=excluded.mime, etag=excluded.etag,
		  storage_class=excluded.storage_class, source_modified_at=excluded.source_modified_at,
		  meta=(case when nodes.size=excluded.size and nodes.etag is not distinct from excluded.etag
		    then nodes.meta else nodes.meta - 'sha256' - 'mime' end) || excluded.meta,
		  updated_at=now()
		returning `+nodeColumns,
		input.OwnerID, snapshot.key, input.Path, paths.ParentOf(input.Path), paths.NameOf(input.Path),
		info.Size, input.Mime, info.ETag, info.StorageClass, modified, string(metaJSON)))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return node, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
