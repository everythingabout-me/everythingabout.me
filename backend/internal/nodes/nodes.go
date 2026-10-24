// Package nodes 维护文件索引表。它是索引，不是副本：
// 对象字节永远只在 COS 里。
package nodes

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

// Node 是返回给前端的行形状（和改造前一致）。
type Node struct {
	ID        string          `json:"id"`
	Path      string          `json:"path"`
	Parent    string          `json:"parent"`
	Name      string          `json:"name"`
	IsDir     bool            `json:"is_dir"`
	Size      int64           `json:"size"`
	Mime      *string         `json:"mime"`
	UpdatedAt string          `json:"updated_at"`
	Meta      json.RawMessage `json:"meta"`
}

// Row 额外带上对象 key，只给服务端用。
type Row struct {
	Node
	Key          string  `json:"-"`
	ETag         *string `json:"-"`
	StorageClass *string `json:"-"`
}

const nodeColumns = `id::text, key, path, parent, name, is_dir, size, mime, etag, storage_class,
	to_char(updated_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
	meta`

type Service struct {
	db    *store.DB
	store storage.Storage
}

func New(db *store.DB, store storage.Storage) *Service {
	return &Service{db: db, store: store}
}

func scanNode(row pgx.Row) (*Row, error) {
	var (
		node Row
		meta []byte
	)
	err := row.Scan(
		&node.ID, &node.Key, &node.Path, &node.Parent, &node.Name, &node.IsDir, &node.Size,
		&node.Mime, &node.ETag, &node.StorageClass, &node.UpdatedAt, &meta,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if len(meta) == 0 {
		meta = []byte("{}")
	}
	node.Meta = meta
	return &node, nil
}

// List 只查索引表，不查桶。
func (s *Service) List(ctx context.Context, ownerID, path string, limit int) ([]Row, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select `+nodeColumns+`
		   from nodes
		  where owner_id = $1::uuid
		    and deleted_at is null
		    and parent = $2
		  order by is_dir desc, lower(name) asc
		  limit $3`, ownerID, path, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Row, 0, 32)
	for rows.Next() {
		item, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// Search 同时匹配文件名和完整路径。
func (s *Service) Search(ctx context.Context, ownerID, keyword string, limit int) ([]Row, error) {
	pattern := "%" + EscapeLike(keyword) + "%"
	rows, err := s.db.Pool().Query(ctx,
		`select `+nodeColumns+`
		   from nodes
		  where owner_id = $1::uuid
		    and deleted_at is null
		    and (name ilike $2 or path ilike $2)
		  order by is_dir desc, lower(name) asc
		  limit $3`, ownerID, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Row, 0, 32)
	for rows.Next() {
		item, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// Live 返回未被删除的单条记录。
func (s *Service) Live(ctx context.Context, path, ownerID string) (*Row, error) {
	return scanNode(s.db.Pool().QueryRow(ctx,
		`select `+nodeColumns+`
		   from nodes
		  where owner_id = $1::uuid and path = $2 and deleted_at is null
		  limit 1`, ownerID, path))
}

// moveToTrash 把一条回收站记录的对象挪到 _trash/，并把 DB 里的 key 改过去。
func (s *Service) moveToTrash(ctx context.Context, id, key string) error {
	trashKey := paths.TrashPrefix + id
	// 对象可能已经被手动清掉；这时只改 DB key，避免同名新建被唯一约束挡住
	exists, err := s.store.Head(ctx, key)
	if err != nil {
		return err
	}
	if exists != nil {
		if err := s.store.Copy(ctx, key, trashKey); err != nil {
			return err
		}
	}
	if _, err := s.db.Pool().Exec(ctx, `update nodes set key = $1 where id = $2::uuid`, trashKey, id); err != nil {
		return err
	}
	// 先改 DB 再删源：删源失败最多留个重复对象，不会把索引指向不存在的 key
	if exists != nil {
		return s.store.Remove(ctx, key)
	}
	return nil
}

// EnsureParents 保证祖先目录都有记录（直传进桶时也能自动补全）。
func (s *Service) EnsureParents(ctx context.Context, path, ownerID string) error {
	dirs := paths.AncestorsOf(path)
	if len(dirs) == 0 {
		return nil
	}

	// 祖先目录可能在回收站里；先把它们的占位对象挪进 _trash/，
	// 否则新插入的目录会撞上 nodes.key 的全局唯一约束。
	rows, err := s.db.Pool().Query(ctx,
		`select id::text, key from nodes
		  where owner_id = $1::uuid
		    and deleted_at is not null
		    and path = any($2::text[])`, ownerID, dirs)
	if err != nil {
		return err
	}
	type trashRow struct{ id, key string }
	var trashed []trashRow
	for rows.Next() {
		var item trashRow
		if err := rows.Scan(&item.id, &item.key); err != nil {
			rows.Close()
			return err
		}
		trashed = append(trashed, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range trashed {
		if err := s.moveToTrash(ctx, item.id, item.key); err != nil {
			return err
		}
	}

	for _, dir := range dirs {
		if _, err := s.db.Pool().Exec(ctx,
			`insert into nodes (owner_id, key, path, parent, name, is_dir)
			 values ($1::uuid, $2, $3, $4, $5, true)
			 on conflict (owner_id, path) where deleted_at is null do nothing`,
			ownerID, paths.DirKeyFor(dir, ownerID), dir, paths.ParentOf(dir), paths.NameOf(dir),
		); err != nil {
			return err
		}
	}
	return nil
}

// EvictTrashedAt 在回收站里的旧对象占着同一个 key 时，先挪到 _trash/ 再允许覆盖。
func (s *Service) EvictTrashedAt(ctx context.Context, path, ownerID string, isDir bool) error {
	key := paths.KeyFor(path, ownerID)
	if isDir {
		key = paths.DirKeyFor(path, ownerID)
	}
	rows, err := s.db.Pool().Query(ctx,
		`select id::text, key from nodes where key = $1 and deleted_at is not null`, key)
	if err != nil {
		return err
	}
	type trashRow struct{ id, key string }
	var trashed []trashRow
	for rows.Next() {
		var item trashRow
		if err := rows.Scan(&item.id, &item.key); err != nil {
			rows.Close()
			return err
		}
		trashed = append(trashed, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range trashed {
		if err := s.moveToTrash(ctx, item.id, item.key); err != nil {
			return err
		}
	}
	return nil
}

type UpsertInput struct {
	OwnerID      string
	Path         string
	Size         int64
	Mime         *string
	ETag         *string
	StorageClass *string
	Meta         map[string]any
}

func (s *Service) UpsertFile(ctx context.Context, input UpsertInput) (*Row, error) {
	meta := input.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}

	return scanNode(s.db.Pool().QueryRow(ctx,
		`insert into nodes (owner_id, key, path, parent, name, is_dir, size, mime, etag, storage_class, meta)
		 values ($1::uuid, $2, $3, $4, $5, false, $6, $7, $8, $9, $10::jsonb)
		 on conflict (owner_id, path) where deleted_at is null
		 do update set
		   key = excluded.key,
		   size = excluded.size,
		   mime = excluded.mime,
		   etag = excluded.etag,
		   storage_class = excluded.storage_class,
		   updated_at = now(),
		   meta = nodes.meta || excluded.meta
		 returning `+nodeColumns,
		input.OwnerID,
		paths.KeyFor(input.Path, input.OwnerID),
		input.Path,
		paths.ParentOf(input.Path),
		paths.NameOf(input.Path),
		input.Size,
		input.Mime,
		input.ETag,
		input.StorageClass,
		string(metaJSON),
	))
}

// SoftDelete 给整棵子树打删除标记，返回影响行数。
func (s *Service) SoftDelete(ctx context.Context, path, ownerID string) (int64, error) {
	tag, err := s.db.Pool().Exec(ctx,
		`update nodes set deleted_at = now()
		  where owner_id = $1::uuid
		    and deleted_at is null
		    and (path = $2 or path like $3)`,
		ownerID, path, EscapeLike(path)+"/%")
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CreateDirectory 插入一条目录记录（占位对象由调用方写入）。
func (s *Service) CreateDirectory(ctx context.Context, path, ownerID string) (*Row, error) {
	return scanNode(s.db.Pool().QueryRow(ctx,
		`insert into nodes (owner_id, key, path, parent, name, is_dir)
		 values ($1::uuid, $2, $3, $4, $5, true)
		 on conflict (owner_id, path) where deleted_at is null do nothing
		 returning `+nodeColumns,
		ownerID, paths.DirKeyFor(path, ownerID), path, paths.ParentOf(path), paths.NameOf(path),
	))
}

// Move 改单条记录的路径（对象复制由调用方完成）。
func (s *Service) Move(ctx context.Context, id, targetPath, ownerID string) (*Row, error) {
	return scanNode(s.db.Pool().QueryRow(ctx,
		`update nodes
		    set key = $1, path = $2, parent = $3, name = $4, updated_at = now()
		  where id = $5::uuid and owner_id = $6::uuid
		  returning `+nodeColumns,
		paths.KeyFor(targetPath, ownerID), targetPath, paths.ParentOf(targetPath),
		paths.NameOf(targetPath), id, ownerID))
}

// EscapeLike 转义 LIKE 通配符，否则目录名里的 % 和 _ 会误伤别的路径。
func EscapeLike(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		switch r {
		case '\\', '%', '_':
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

// AssertNotRoot 检查路径不是根目录。
func AssertNotRoot(path string) error {
	if path == "" {
		return httpx.BadRequest("不能操作根目录")
	}
	return nil
}
