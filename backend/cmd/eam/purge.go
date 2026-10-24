package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/store"
)

func runPurge(args []string) error {
	flags := flag.NewFlagSet("purge", flag.ContinueOnError)
	days := flags.Int("days", 30, "回收站保留天数")
	dryRun := flags.Bool("dry-run", false, "只显示会清理多少条")
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

	rows, err := db.Pool().Query(ctx,
		`select id::text, key from nodes
		  where deleted_at is not null
		    and deleted_at < now() - ($1::int * interval '1 day')
		  order by deleted_at asc
		  limit 5000`, *days)
	if err != nil {
		return err
	}
	type trashed struct{ id, key string }
	var items []trashed
	for rows.Next() {
		var item trashed
		if err := rows.Scan(&item.id, &item.key); err != nil {
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
		fmt.Println("没有需要清理的对象")
		return nil
	}
	if *dryRun {
		fmt.Printf("[dry-run] 将清理 %d 条记录\n", len(items))
		return nil
	}

	objectStorage, err := cos.New(cfg.COS)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := objectStorage.Remove(ctx, item.key); err != nil {
			log.Printf("删除对象失败 %s: %v", item.key, err)
		}
	}

	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.id
	}
	if _, err := db.Pool().Exec(ctx,
		`delete from nodes where id::text = any($1::text[])`, ids); err != nil {
		return err
	}
	fmt.Printf("已清理 %d 条记录\n", len(items))
	return nil
}
