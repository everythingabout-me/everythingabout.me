// Package nodes 维护文件索引表。它是索引，不是副本：
// 对象字节永远只在 COS 里。
package nodes

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

// Node 是返回给前端的行形状（和改造前一致）。
type Node struct {
	ID               string          `json:"id"`
	Path             string          `json:"path"`
	Parent           string          `json:"parent"`
	Name             string          `json:"name"`
	IsDir            bool            `json:"is_dir"`
	Size             int64           `json:"size"`
	Mime             *string         `json:"mime"`
	SourceModifiedAt *string         `json:"source_modified_at"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
	Meta             json.RawMessage `json:"meta"`
}

// Row 额外带上对象 key，只给服务端用。
type Row struct {
	Node
	Key          string  `json:"-"`
	ETag         *string `json:"-"`
	StorageClass *string `json:"-"`
}

const nodeColumns = `id::text, key, path, parent, name, is_dir, size, mime, etag, storage_class,
	to_char(created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
	to_char(updated_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
	case when source_modified_at is null then null
	     else to_char(source_modified_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') end,
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
		&node.Mime, &node.ETag, &node.StorageClass, &node.CreatedAt, &node.UpdatedAt,
		&node.SourceModifiedAt, &meta,
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
		  where owner_id = $1::uuid and path = $2
		  limit 1`, ownerID, path))
}

// EnsureParents 保证祖先目录都有记录（直接写进桶的文件也能自动补全）。
func (s *Service) EnsureParents(ctx context.Context, path, ownerID string) error {
	for _, dir := range paths.AncestorsOf(path) {
		if _, err := s.db.Pool().Exec(ctx,
			`insert into nodes (owner_id, key, path, parent, name, is_dir)
			 values ($1::uuid, $2, $3, $4, $5, true)
			 on conflict (owner_id, path) do nothing`,
			ownerID, paths.DirKeyFor(dir, ownerID), dir, paths.ParentOf(dir), paths.NameOf(dir),
		); err != nil {
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
	// SourceModifiedAt 是对象存储侧的最后修改时间。它和 updated_at 不是一回事：
	// updated_at 是"写进索引"的时间，reconcile 灌历史数据时全是今天。
	SourceModifiedAt *time.Time
	Meta             map[string]any
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
		`insert into nodes (owner_id, key, path, parent, name, is_dir, size, mime, etag, storage_class,
		                    source_modified_at, meta)
		 values ($1::uuid, $2, $3, $4, $5, false, $6, $7, $8, $9, $10, $11::jsonb)
		 on conflict (owner_id, path)
		 do update set
		   key = excluded.key,
		   size = excluded.size,
		   mime = excluded.mime,
		   etag = excluded.etag,
		   storage_class = excluded.storage_class,
		   source_modified_at = coalesce(excluded.source_modified_at, nodes.source_modified_at),
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
		input.SourceModifiedAt,
		string(metaJSON),
	))
}

// Purge 彻底删除一棵子树：对象和索引都删掉，没有回收站。
func (s *Service) Purge(ctx context.Context, path, ownerID string) (int64, error) {
	rows, err := s.Subtree(ctx, ownerID, path)
	if err != nil {
		return 0, err
	}
	for i := range rows {
		info, err := s.store.Head(ctx, rows[i].Key)
		if err != nil {
			return 0, err
		}
		if info == nil {
			continue
		}
		if err := s.store.Remove(ctx, rows[i].Key); err != nil {
			return 0, err
		}
	}

	tag, err := s.db.Pool().Exec(ctx,
		`delete from nodes
		  where owner_id = $1::uuid
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
		 on conflict (owner_id, path) do nothing
		 returning `+nodeColumns,
		ownerID, paths.DirKeyFor(path, ownerID), path, paths.ParentOf(path), paths.NameOf(path),
	))
}

// Move 改单条记录的路径（对象复制由调用方完成）。
func (s *Service) Move(ctx context.Context, id, targetPath, ownerID string) (*Row, error) {
	return s.MoveNode(ctx, id, targetPath, ownerID, false)
}

// MoveNode 改一条记录的路径。目录的 key 要带尾斜杠（占位对象约定），
// 所以不能无脑用 KeyFor。
func (s *Service) MoveNode(ctx context.Context, id, targetPath, ownerID string, isDir bool) (*Row, error) {
	key := paths.KeyFor(targetPath, ownerID)
	if isDir {
		key = paths.DirKeyFor(targetPath, ownerID)
	}
	return scanNode(s.db.Pool().QueryRow(ctx,
		`update nodes
		    set key = $1, path = $2, parent = $3, name = $4, updated_at = now()
		  where id = $5::uuid and owner_id = $6::uuid
		  returning `+nodeColumns,
		key, targetPath, paths.ParentOf(targetPath),
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
		return httpx.BadRequest("the root directory cannot be modified")
	}
	return nil
}

// Cursor 是列目录的 keyset 分页位置：排序键是 (is_dir desc, lower(name), id)。
type Cursor struct {
	IsDir bool
	Name  string // 已经 lower 过的名字
	ID    string
}

// ListPage 按 keyset 分页列目录，limit 由调用方给（一般取 pageSize+1 判断还有没有下一页）。
func (s *Service) ListPage(
	ctx context.Context,
	ownerID, path string,
	limit int,
	cursor *Cursor,
) ([]Row, error) {
	var curDir, curName, curID any
	if cursor != nil {
		curDir, curName, curID = cursor.IsDir, cursor.Name, cursor.ID
	}

	rows, err := s.db.Pool().Query(ctx,
		`select `+nodeColumns+`
		   from nodes
		  where owner_id = $1::uuid
		    and parent = $2
		    and ($3::bool is null
		         or (is_dir = false and $3 = true)
		         or (is_dir = $3 and (lower(name), id::text) > ($4, $5)))
		  order by is_dir desc, lower(name) asc, id asc
		  limit $6`,
		ownerID, path, curDir, curName, curID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Row, 0, limit)
	for rows.Next() {
		item, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// Usage 统计一个用户占用的字节数和文件数（不含目录）。
func (s *Service) Usage(ctx context.Context, ownerID string) (bytes, files int64, err error) {
	err = s.db.Pool().QueryRow(ctx,
		`select coalesce(sum(size), 0), count(*)
		   from nodes
		  where owner_id = $1::uuid
		    and is_dir = false`, ownerID).Scan(&bytes, &files)
	return bytes, files, err
}

// SearchOptions 是搜索的过滤条件。
type SearchOptions struct {
	Keyword string // 匹配文件名和完整路径
	Prefix  string // 只在这个目录下找
	Mime    string // 前缀匹配，如 "video/"
	Kind    string // "" / "file" / "dir"
}

// SearchPage 按 keyset 分页搜索，排序和 ListPage 一致，游标格式也一致。
func (s *Service) SearchPage(
	ctx context.Context,
	ownerID string,
	opts SearchOptions,
	limit int,
	cursor *Cursor,
) ([]Row, error) {
	pattern := "%" + EscapeLike(opts.Keyword) + "%"

	var prefix, prefixLike any
	if opts.Prefix != "" {
		prefix = opts.Prefix
		prefixLike = EscapeLike(opts.Prefix) + "/%"
	}

	var curDir, curName, curID any
	if cursor != nil {
		curDir, curName, curID = cursor.IsDir, cursor.Name, cursor.ID
	}

	rows, err := s.db.Pool().Query(ctx,
		`select `+nodeColumns+`
		   from nodes
		  where owner_id = $1::uuid
		    and (name ilike $2 or path ilike $2)
		    and ($3::text is null or path = $3 or path like $4)
		    and ($5 = '' or coalesce(mime, '') ilike $5 || '%')
		    and ($6 = '' or ($6 = 'dir' and is_dir) or ($6 = 'file' and not is_dir))
		    and ($7::bool is null
		         or (is_dir = false and $7 = true)
		         or (is_dir = $7 and (lower(name), id::text) > ($8, $9)))
		  order by is_dir desc, lower(name) asc, id asc
		  limit $10`,
		ownerID, pattern, prefix, prefixLike, opts.Mime, opts.Kind, curDir, curName, curID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Row, 0, limit)
	for rows.Next() {
		item, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// Subtree 返回一个目录下的全部记录（含它自己），父目录在前。
// 目录移动用它来逐个搬对象。
func (s *Service) Subtree(ctx context.Context, ownerID, path string) ([]Row, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select `+nodeColumns+`
		   from nodes
		  where owner_id = $1::uuid
		    and (path = $2 or path like $3)
		  order by length(path) asc, path asc`,
		ownerID, path, EscapeLike(path)+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []Row
	for rows.Next() {
		item, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}
