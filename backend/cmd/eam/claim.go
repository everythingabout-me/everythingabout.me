package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/store"
)

// claim 把多用户改造之前留下的旧文件认领给某个账号。
//
// 旧数据在数据库里 owner_id 为空、COS key 是 drive/<路径>；
// 新方案是每个用户一个前缀 drive/<用户ID>/<路径>，所以这里同时
// 复制对象并改写 key。先跑 --dry-run 看清楚再正式执行。
func runClaim(args []string) error {
	flags := flag.NewFlagSet("claim", flag.ContinueOnError)
	email := flags.String("email", "", "assign legacy files to this email")
	dryRun := flags.Bool("dry-run", false, "print only, apply nothing")
	keepSource := flags.Bool("keep-source", false, "keep old objects, do not delete sources")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*email) == "" {
		return fmt.Errorf("usage: eam claim --email you@example.com [--dry-run] [--keep-source]")
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
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	var ownerID, ownerEmail string
	err = db.Pool().QueryRow(ctx,
		`select id::text, email from users where lower(email) = lower($1) limit 1`,
		strings.TrimSpace(*email)).Scan(&ownerID, &ownerEmail)
	if err != nil {
		return fmt.Errorf("user %s not found; sign up on the website first", *email)
	}

	rows, err := db.Pool().Query(ctx,
		`select id::text, path, key, is_dir
		   from nodes
		  where owner_id is null
		  order by is_dir asc, path asc`)
	if err != nil {
		return err
	}
	type legacy struct {
		id    string
		path  string
		key   string
		isDir bool
	}
	var items []legacy
	for rows.Next() {
		var item legacy
		if err := rows.Scan(&item.id, &item.path, &item.key, &item.isDir); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if len(items) == 0 {
		fmt.Println("no legacy files to claim")
		return nil
	}

	fmt.Printf("claiming %d records into %s (%s)\n", len(items), ownerEmail, ownerID)

	objectStorage, err := cos.New(cfg.COS)
	if err != nil {
		return err
	}

	var moved, skipped, failed int
	for _, item := range items {
		targetKey := paths.KeyFor(item.path, ownerID)
		if item.isDir {
			targetKey = paths.DirKeyFor(item.path, ownerID)
		}
		if *dryRun {
			fmt.Printf("  %s  →  %s\n", item.key, targetKey)
			moved++
			continue
		}

		source, err := objectStorage.Head(ctx, item.key)
		if err != nil {
			failed++
			log.Printf("  failed %s: %v", item.key, err)
			continue
		}
		if source == nil {
			if item.isDir {
				// 空目录的占位对象丢了也能补回来
				if err := objectStorage.PutEmpty(ctx, targetKey); err != nil {
					failed++
					log.Printf("  failed %s: %v", item.key, err)
					continue
				}
			} else {
				fmt.Printf("  skipped (source object missing): %s\n", item.key)
				skipped++
				continue
			}
		} else if err := objectStorage.Copy(ctx, item.key, targetKey); err != nil {
			failed++
			log.Printf("  failed %s: %v", item.key, err)
			continue
		}

		tag, err := db.Pool().Exec(ctx,
			`update nodes set owner_id = $1::uuid, key = $2, updated_at = now()
			  where id = $3::uuid and owner_id is null`, ownerID, targetKey, item.id)
		if err != nil {
			failed++
			log.Printf("  failed %s: %v", item.key, err)
			continue
		}
		if tag.RowsAffected() == 0 {
			fmt.Printf("  skipped (already claimed by another process): %s\n", item.key)
			skipped++
			continue
		}
		if source != nil && !*keepSource {
			// 数据库已经指向新 key 之后再删源，失败顶多留个重复对象，不会丢数据
			if err := objectStorage.Remove(ctx, item.key); err != nil {
				log.Printf("  failed to delete source object %s: %v", item.key, err)
			}
		}
		moved++
		if moved%100 == 0 {
			fmt.Printf("  processed %d/%d\n", moved, len(items))
		}
	}

	prefix := ""
	if *dryRun {
		prefix = "[dry-run] "
	}
	fmt.Printf("%sdone: moved %d, skipped %d, failed %d\n", prefix, moved, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d records failed to migrate", failed)
	}
	return nil
}
