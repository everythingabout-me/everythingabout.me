package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/store"
)

// runToken 管理对外 API 的访问令牌。
//
//	eam token create --email you@example.com --name Obsidian [--prefix Apps/obsidian] [--write=false]
//	eam token list   --email you@example.com
//	eam token revoke --email you@example.com --id 7f3a9c2b1d4e
func runToken(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`用法:
  eam token create --email 邮箱 --name 名字 [--prefix 路径] [--write=false] [--expires 720h]
  eam token list   --email 邮箱
  eam token revoke --email 邮箱 --id 令牌ID`)
	}
	switch args[0] {
	case "create":
		return runTokenCreate(args[1:])
	case "list":
		return runTokenList(args[1:])
	case "revoke":
		return runTokenRevoke(args[1:])
	default:
		return fmt.Errorf("未知的 token 子命令 %q", args[0])
	}
}

func runTokenCreate(args []string) error {
	flags := flag.NewFlagSet("token create", flag.ContinueOnError)
	email := flags.String("email", "", "账号邮箱")
	name := flags.String("name", "", "令牌名字，如 Obsidian")
	prefix := flags.String("prefix", "", "只能访问的路径前缀，留空表示整个盘")
	canRead := flags.Bool("read", true, "允许读")
	canWrite := flags.Bool("write", true, "允许写")
	expires := flags.Duration("expires", 0, "有效期，如 720h（默认永不过期）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *email == "" || *name == "" {
		return fmt.Errorf("--email 和 --name 必填")
	}

	db, svc, err := openAuthService()
	if err != nil {
		return err
	}
	defer db.Close()

	user, err := svc.FindUserByEmail(context.Background(), *email)
	if err != nil {
		return err
	}
	if user == nil {
		return fmt.Errorf("找不到账号 %s", *email)
	}

	var expiry *time.Time
	if *expires > 0 {
		at := time.Now().Add(*expires)
		expiry = &at
	}

	plain, token, err := svc.CreateToken(context.Background(), auth.NewTokenInput{
		UserID:   user.ID,
		Name:     *name,
		Prefix:   *prefix,
		CanRead:  *canRead,
		CanWrite: *canWrite,
		Expiry:   expiry,
	})
	if err != nil {
		return err
	}

	fmt.Printf("\n令牌已创建（只显示这一次，请立刻保存）：\n\n  %s\n\n", plain)
	fmt.Printf("  名字: %s\n", token.Name)
	if token.Prefix == "" {
		fmt.Printf("  前缀: 整个盘\n")
	} else {
		fmt.Printf("  前缀: %s\n", token.Prefix)
	}
	fmt.Printf("  权限: %s\n", permissionText(token.CanRead, token.CanWrite))
	if token.ExpiresAt == nil {
		fmt.Printf("  过期: 永不过期\n")
	} else {
		fmt.Printf("  过期: %s\n", *token.ExpiresAt)
	}
	fmt.Printf("  ID:   %s（用于 eam token revoke）\n\n", token.ID)
	return nil
}

func runTokenList(args []string) error {
	flags := flag.NewFlagSet("token list", flag.ContinueOnError)
	email := flags.String("email", "", "账号邮箱")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return fmt.Errorf("--email 必填")
	}

	db, svc, err := openAuthService()
	if err != nil {
		return err
	}
	defer db.Close()

	user, err := svc.FindUserByEmail(context.Background(), *email)
	if err != nil {
		return err
	}
	if user == nil {
		return fmt.Errorf("找不到账号 %s", *email)
	}

	tokens, err := svc.ListTokens(context.Background(), user.ID)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		fmt.Println("还没有任何令牌。")
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\t名字\t前缀\t权限\t状态\t最近使用\t过期")
	for _, token := range tokens {
		prefix := token.Prefix
		if prefix == "" {
			prefix = "(整个盘)"
		}
		lastUsed := "-"
		if token.LastUsedAt != nil {
			lastUsed = *token.LastUsedAt
		}
		expiresAt := "永不"
		if token.ExpiresAt != nil {
			expiresAt = *token.ExpiresAt
		}
		status := "有效"
		if token.Revoked {
			status = "已撤销"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			token.ID, token.Name, prefix, permissionText(token.CanRead, token.CanWrite),
			status, lastUsed, expiresAt)
	}
	return writer.Flush()
}

func runTokenRevoke(args []string) error {
	flags := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	email := flags.String("email", "", "账号邮箱")
	id := flags.String("id", "", "令牌 ID（eam token list 里那一列）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *email == "" || *id == "" {
		return fmt.Errorf("--email 和 --id 必填")
	}

	db, svc, err := openAuthService()
	if err != nil {
		return err
	}
	defer db.Close()

	user, err := svc.FindUserByEmail(context.Background(), *email)
	if err != nil {
		return err
	}
	if user == nil {
		return fmt.Errorf("找不到账号 %s", *email)
	}
	if err := svc.RevokeToken(context.Background(), user.ID, *id); err != nil {
		return err
	}
	fmt.Printf("已撤销令牌 %s。它立刻失效；已经发出去的短期凭证最多再活 2 小时。\n", *id)
	return nil
}

func openAuthService() (*store.DB, *auth.Service, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return db, auth.New(db, cfg), nil
}

func permissionText(read, write bool) string {
	switch {
	case read && write:
		return "读+写"
	case read:
		return "只读"
	case write:
		return "只写"
	default:
		return "无"
	}
}
