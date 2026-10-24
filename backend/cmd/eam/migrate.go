package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/store"
)

// findSQLDir 支持从仓库根目录或 backend/ 目录运行。
func findSQLDir() (string, error) {
	if dir := os.Getenv("EAM_SQL_DIR"); dir != "" {
		return dir, nil
	}
	for _, candidate := range []string{"sql", filepath.Join("..", "sql"), filepath.Join("..", "..", "sql")} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("找不到 sql/ 目录，可用 EAM_SQL_DIR 指定")
}

func runMigrate(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("migrate 不接受参数: %v", args)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir, err := findSQLDir()
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("%s 里没有 .sql 文件", dir)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer db.Close()

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, file := range files {
		fmt.Printf("→ %s ", file)
		content, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return err
		}
		// 没有参数时 pgx 会走 simple protocol，可以一次执行多条语句
		if _, err := tx.Exec(ctx, string(content)); err != nil {
			fmt.Println("✗")
			return fmt.Errorf("%s: %w", file, err)
		}
		fmt.Println("✓")
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("\n已执行 %d 个迁移。\n", len(files))
	return nil
}
