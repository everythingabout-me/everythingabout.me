package main

import (
	"context"
	"flag"
	"fmt"
	"regexp"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/store"
)

var credentialRe = regexp.MustCompile(`//[^@]*@`)

type doctor struct {
	failed int
}

func (d *doctor) ok(label, detail string) {
	fmt.Printf("  ✓ %s%s\n", label, detail)
}

func (d *doctor) bad(label string, err error, hint string) {
	d.failed++
	fmt.Printf("  ✗ %s  %v\n", label, err)
	if hint != "" {
		fmt.Printf("    → %s\n", hint)
	}
}

func (d *doctor) warn(label string) {
	fmt.Printf("  ! %s\n", label)
}

func runDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	writeTest := flags.Bool("write", false, "额外测试一次写入+删除")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	checks := &doctor{}

	fmt.Println("\n配置")
	fmt.Printf("  bucket   %s\n", cfg.COS.Bucket)
	fmt.Printf("  region   %s\n", cfg.COS.Region)
	fmt.Printf("  database %s\n", credentialRe.ReplaceAllString(cfg.DatabaseURL, "//***@"))

	ctx := context.Background()
	db, dbErr := store.Open(ctx, cfg.DatabaseURL)

	fmt.Println("\n数据库")
	if dbErr != nil {
		checks.bad("连接失败", dbErr, "检查 DATABASE_URL；端口被别的库占用时换一个端口")
	} else {
		defer db.Close()
		checkDatabase(ctx, db, checks)
	}

	fmt.Println("\n邮件")
	if cfg.SESConfigured() {
		checks.ok("腾讯云 SES 已配置", " "+cfg.SES.Region+" · "+cfg.SES.FromEmailAddress)
	} else if cfg.IsProduction() {
		checks.bad("腾讯云 SES 未配置",
			fmt.Errorf("生产环境无法发送验证码"),
			"填 SES_FROM_EMAIL_ADDRESS / SES_TEMPLATE_ID 等；或在开发环境运行")
	} else {
		checks.warn("腾讯云 SES 未配置：开发环境验证码会打印到服务端日志，并在登录页自动填入")
	}

	fmt.Println("\nCOS")
	if err := cfg.ValidateCOS(); err != nil {
		checks.bad("COS 配置不完整", err, "填 COS_SECRET_ID / COS_SECRET_KEY / COS_BUCKET / COS_REGION")
	} else {
		checkCOS(ctx, cfg, checks, *writeTest)
	}

	if checks.failed == 0 {
		fmt.Print("\n一切正常。\n\n")
		return nil
	}
	fmt.Printf("\n有 %d 项没通过。\n", checks.failed)
	return fmt.Errorf("%d 项检查未通过", checks.failed)
}

func checkDatabase(ctx context.Context, db *store.DB, checks *doctor) {
	pool := db.Pool()

	var nodesTable string
	if err := pool.QueryRow(ctx, `select coalesce(to_regclass('public.nodes')::text, '')`).Scan(&nodesTable); err != nil {
		checks.bad("查询 nodes 表失败", err, "")
		return
	}
	if nodesTable == "" {
		checks.bad("nodes 表不存在", fmt.Errorf("missing"), "先跑 eam migrate")
	} else {
		var count int
		if err := pool.QueryRow(ctx, `select count(*)::int from nodes`).Scan(&count); err != nil {
			checks.bad("统计 nodes 失败", err, "")
		} else {
			checks.ok("连接正常，nodes 表就绪", fmt.Sprintf(" 当前 %d 条记录", count))
		}
	}

	var usersTable, sessionsTable, codesTable string
	err := pool.QueryRow(ctx, `select
		coalesce(to_regclass('public.users')::text, ''),
		coalesce(to_regclass('public.sessions')::text, ''),
		coalesce(to_regclass('public.email_codes')::text, '')`).Scan(&usersTable, &sessionsTable, &codesTable)
	if err != nil {
		checks.bad("查询用户相关表失败", err, "")
		return
	}
	if usersTable == "" || sessionsTable == "" || codesTable == "" {
		checks.bad("users / sessions / email_codes 表不存在", fmt.Errorf("missing"), "先跑 eam migrate")
		return
	}

	var users, unowned int
	if err := pool.QueryRow(ctx, `select count(*)::int from users`).Scan(&users); err != nil {
		checks.bad("统计 users 失败", err, "")
		return
	}
	if err := pool.QueryRow(ctx,
		`select count(*)::int from nodes where owner_id is null and deleted_at is null`).Scan(&unowned); err != nil {
		checks.bad("统计无主文件失败", err, "")
		return
	}
	checks.ok("用户表就绪", fmt.Sprintf(" 当前 %d 个账号", users))
	if unowned > 0 {
		checks.warn(fmt.Sprintf("还有 %d 条旧文件没有归属，跑 eam claim --email 你@example.com 认领", unowned))
	}
}

func checkCOS(ctx context.Context, cfg *config.Config, checks *doctor, writeTest bool) {
	objectStorage, err := cos.New(cfg.COS)
	if err != nil {
		checks.bad("初始化 COS 客户端失败", err, "")
		return
	}
	probe := fmt.Sprintf("drive/__doctor__/probe-%d.txt", time.Now().UnixNano())

	info, err := objectStorage.Head(ctx, probe)
	if err != nil {
		checks.bad("headObject 失败", err, "多为桶名缺 APPID、region 填错、或密钥无权限")
		return
	}
	if info != nil {
		checks.ok("鉴权与桶可达", " （意外：探针对象存在）")
	} else {
		checks.ok("鉴权与桶可达", " （探针对象不存在，符合预期）")
	}

	credentials, err := objectStorage.TemporaryCredentials(ctx, probe, 900)
	if err != nil {
		checks.bad("STS 签发失败", err, "子账号需要 sts:GetFederationToken 权限，或密钥不是有效的 API 密钥")
	} else {
		expires := time.Unix(credentials.ExpiredTime, 0).Format(time.DateTime)
		checks.ok("STS 临时凭证签发成功", " 有效期至 "+expires)
	}

	if !writeTest {
		return
	}
	if err := objectStorage.PutEmpty(ctx, probe); err != nil {
		checks.bad("写入失败", err, "检查子账号是否有 PutObject / DeleteObject 权限")
		return
	}
	written, err := objectStorage.Head(ctx, probe)
	if err != nil || written == nil {
		checks.bad("读取刚写入的对象失败", err, "")
		return
	}
	if err := objectStorage.Remove(ctx, probe); err != nil {
		checks.bad("删除探针对象失败", err, "")
		return
	}
	checks.ok("写入 / 读取 / 删除", fmt.Sprintf(" 大小 %d 字节", written.Size))
}
