package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

const reconcileBatch = 500

type scanned struct {
	key          string
	path         string
	isDir        bool
	size         int64
	etag         string
	storageClass string
}

// reconcile 把 COS 桶里的对象同步进数据库索引。
//
//  1. 用 coscli / rclone 把文件灌进某个用户的 drive/<用户ID>/ 前缀后，跑这个命令建索引；
//  2. 也可以读 COS 清单（Inventory）CSV，避免百万对象全量 List。
func runReconcile(args []string) error {
	flags := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	owner := flags.String("owner", "", "只同步这个邮箱对应的用户前缀")
	inventory := flags.String("inventory", "", "COS 清单 CSV；不填则实时 List")
	dryRun := flags.Bool("dry-run", false, "打印结果但不提交")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateCOS(); err != nil {
		return err
	}

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer db.Close()

	ownerID := ""
	if strings.TrimSpace(*owner) != "" {
		if err := db.Pool().QueryRow(ctx,
			`select id::text from users where lower(email) = lower($1) limit 1`,
			strings.TrimSpace(*owner)).Scan(&ownerID); err != nil {
			return fmt.Errorf("找不到用户 %s，请先在网站上注册", *owner)
		}
	}

	prefix := paths.DrivePrefix
	if ownerID != "" {
		prefix = paths.OwnerPrefix(ownerID)
	}

	objectStorage, err := cos.New(cfg.COS)
	if err != nil {
		return err
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `create temp table scan_keys (key text primary key) on commit drop`); err != nil {
		return err
	}

	startedAt := time.Now()
	var seen, inserted int
	var buffer []scanned

	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		owners := make([]string, len(buffer))
		keys := make([]string, len(buffer))
		itemPaths := make([]string, len(buffer))
		parents := make([]string, len(buffer))
		names := make([]string, len(buffer))
		dirs := make([]string, len(buffer))
		sizes := make([]string, len(buffer))
		etags := make([]string, len(buffer))
		classes := make([]string, len(buffer))
		for i, item := range buffer {
			owners[i] = ownerID
			keys[i] = item.key
			itemPaths[i] = item.path
			parents[i] = paths.ParentOf(item.path)
			names[i] = paths.NameOf(item.path)
			dirs[i] = strconv.FormatBool(item.isDir)
			sizes[i] = strconv.FormatInt(item.size, 10)
			etags[i] = item.etag
			classes[i] = item.storageClass
		}

		tag, err := tx.Exec(ctx,
			`insert into nodes (owner_id, key, path, parent, name, is_dir, size, etag, storage_class)
			 select nullif(owner, '')::uuid, key, path, parent, name,
			        is_dir::boolean, size::bigint, nullif(etag, ''), nullif(storage_class, '')
			   from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[],
			               $6::text[], $7::text[], $8::text[], $9::text[])
			     as t(owner, key, path, parent, name, is_dir, size, etag, storage_class)
			 on conflict do nothing`,
			owners, keys, itemPaths, parents, names, dirs, sizes, etags, classes)
		if err != nil {
			return err
		}
		inserted += int(tag.RowsAffected())
		seen += len(buffer)

		if _, err := tx.Exec(ctx,
			`insert into scan_keys (key) select unnest($1::text[]) on conflict do nothing`, keys); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`update nodes n
			    set size = s.size,
			        etag = s.etag,
			        storage_class = s.storage_class,
			        updated_at = now()
			   from (
			     select key,
			            size::bigint as size,
			            nullif(etag, '') as etag,
			            nullif(storage_class, '') as storage_class
			       from unnest($1::text[], $2::text[], $3::text[], $4::text[])
			         as t(key, size, etag, storage_class)
			   ) s
			  where n.key = s.key
			    and n.deleted_at is null
			    and (n.size is distinct from s.size or n.etag is distinct from s.etag)`,
			keys, sizes, etags, classes); err != nil {
			return err
		}

		buffer = buffer[:0]
		return nil
	}

	accept := func(item scanned) error {
		buffer = append(buffer, item)
		if len(buffer) >= reconcileBatch {
			return flush()
		}
		return nil
	}

	acceptKey := func(key string, size int64, etag, storageClass string) error {
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		// 不带 --owner 时只处理旧的无主数据，跳过已有归属的 drive/<用户ID>/
		if ownerID == "" && paths.OwnerSegmentOf(key) != "" {
			return nil
		}
		itemPath, ok := paths.PathFromKey(key, ownerID)
		if !ok {
			return nil
		}
		isDir := strings.HasSuffix(key, "/")
		if isDir && itemPath == "" {
			return nil
		}
		if isDir {
			size = 0
		}
		return accept(scanned{
			key:          key,
			path:         itemPath,
			isDir:        isDir,
			size:         size,
			etag:         etag,
			storageClass: storageClass,
		})
	}

	scope := prefix
	if ownerID == "" {
		scope += "（旧的无主数据）"
	}
	if *dryRun {
		scope += " [dry-run]"
	}
	fmt.Printf("扫描范围：%s\n", scope)

	if *inventory != "" {
		if err := readInventory(*inventory, acceptKey); err != nil {
			return err
		}
	} else {
		err := objectStorage.List(ctx, prefix, func(page storage.Page) (bool, error) {
			for _, object := range page.Objects {
				if err := acceptKey(object.Key, object.Size, object.ETag, object.StorageClass); err != nil {
					return false, err
				}
			}
			return true, nil
		})
		if err != nil {
			return err
		}
	}
	if err := flush(); err != nil {
		return err
	}

	// 补全父目录
	if err := backfillParents(ctx, tx, ownerID); err != nil {
		return err
	}

	// 扫描期间没见过的对象 = 已被外部删除（只影响本次扫描的归属范围）
	var gone int64
	err = tx.QueryRow(ctx,
		`with changed as (
		   update nodes set deleted_at = now(), updated_at = now()
		    where deleted_at is null
		      and updated_at < $1
		      and owner_id is not distinct from nullif($2, '')::uuid
		      and key not in (select key from scan_keys)
		    returning 1
		 )
		 select count(*) from changed`, startedAt, ownerID).Scan(&gone)
	if err != nil {
		return err
	}

	if *dryRun {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
	} else if err := tx.Commit(ctx); err != nil {
		return err
	}

	prefixText := ""
	if *dryRun {
		prefixText = "[dry-run] "
	}
	fmt.Printf("%s扫描 %d 个对象，新增 %d 条，标记删除 %d 条\n", prefixText, seen, inserted, gone)
	return nil
}

// backfillParents 保证树上每一级目录都有记录。
func backfillParents(ctx context.Context, tx pgx.Tx, ownerID string) error {
	rows, err := tx.Query(ctx,
		`select distinct parent
		   from nodes
		  where deleted_at is null
		    and parent <> ''
		    and owner_id is not distinct from nullif($1, '')::uuid`, ownerID)
	if err != nil {
		return err
	}
	allDirs := make(map[string]struct{})
	for rows.Next() {
		var parent string
		if err := rows.Scan(&parent); err != nil {
			rows.Close()
			return err
		}
		allDirs[parent] = struct{}{}
		for _, ancestor := range paths.AncestorsOf(parent) {
			allDirs[ancestor] = struct{}{}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(allDirs) == 0 {
		return nil
	}

	dirs := make([]string, 0, len(allDirs))
	for dir := range allDirs {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) < len(dirs[j]) })

	owners := make([]string, len(dirs))
	keys := make([]string, len(dirs))
	parents := make([]string, len(dirs))
	names := make([]string, len(dirs))
	for i, dir := range dirs {
		owners[i] = ownerID
		if ownerID != "" {
			keys[i] = paths.DirKeyFor(dir, ownerID)
		} else {
			keys[i] = paths.DrivePrefix + dir + "/"
		}
		parents[i] = paths.ParentOf(dir)
		names[i] = paths.NameOf(dir)
	}

	_, err = tx.Exec(ctx,
		`insert into nodes (owner_id, key, path, parent, name, is_dir)
		 select nullif(owner, '')::uuid, key, path, parent, name, true
		   from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[])
		     as t(owner, key, path, parent, name)
		 on conflict do nothing`,
		owners, keys, dirs, parents, names)
	return err
}

// readInventory 读 COS 清单 CSV，按表头（或默认列序）取 key / size / etag。
func readInventory(file string, accept func(key string, size int64, etag, storageClass string) error) error {
	handle, err := os.Open(file)
	if err != nil {
		return err
	}
	defer handle.Close()

	reader := csv.NewReader(handle)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	var header []string
	first := true
	for {
		record, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if len(record) == 0 {
			continue
		}
		for i := range record {
			record[i] = strings.TrimSpace(record[i])
		}

		if first {
			first = false
			isHeader := false
			for _, cell := range record {
				if strings.Contains(strings.ToLower(cell), "key") {
					isHeader = true
					break
				}
			}
			if isHeader {
				header = make([]string, len(record))
				for i, cell := range record {
					header[i] = strings.ToLower(cell)
				}
				continue
			}
			// 无表头时按 COS 清单默认列序
			header = []string{"bucket", "key", "size", "lastmodifieddate", "etag", "storageclass"}
		}

		row := make(map[string]string, len(header))
		for i, name := range header {
			if i < len(record) {
				row[name] = record[i]
			}
		}
		key := pick(row, "key", "objectkey", "object_key")
		if key == "" || strings.HasPrefix(key, "_inventory/") {
			continue
		}
		size, _ := strconv.ParseInt(pick(row, "size"), 10, 64)
		if err := accept(key, size,
			strings.Trim(pick(row, "etag"), `"`),
			pick(row, "storageclass", "storage_class")); err != nil {
			return err
		}
	}
}

func pick(row map[string]string, names ...string) string {
	for _, name := range names {
		if value := row[name]; value != "" {
			return value
		}
	}
	return ""
}
