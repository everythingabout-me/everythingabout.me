// Package mail 发送邮箱验证码（腾讯云 SES）。
package mail

import (
	"context"
	"fmt"
	"log"
	"strings"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/tencent"
)

type Sender struct {
	cfg  *config.Config
	cred tencent.Credentials
}

func New(cfg *config.Config) *Sender {
	return &Sender{
		cfg:  cfg,
		cred: tencent.Credentials{SecretID: cfg.SES.SecretID, SecretKey: cfg.SES.SecretKey},
	}
}

func (s *Sender) Configured() bool { return s.cfg.SESConfigured() }

type sendEmailResponse struct {
	MessageID string `json:"MessageId"`
	RequestID string `json:"RequestId"`
}

// SendVerificationCode 发验证码。
//   - 配好 SES：真实发信
//   - 没配且是开发环境：打印到服务端日志，并把验证码返回给页面自动填入
//   - 没配且是生产环境：明确报错，不静默
func (s *Sender) SendVerificationCode(ctx context.Context, to, code string) (string, error) {
	if !s.Configured() {
		if s.cfg.IsProduction() {
			return "", httpx.ServiceUnavailable("邮件服务未配置，暂时无法发送验证码")
		}
		log.Printf("[email:dev] 验证码 %s -> %s", code, to)
		return code, nil
	}

	templateData, err := templateDataJSON(s.cfg.SES.TemplateVar, code)
	if err != nil {
		return "", err
	}

	payload := map[string]any{
		"FromEmailAddress": s.cfg.SES.FromEmailAddress,
		"Subject":          s.cfg.SES.Subject,
		"Destination":      []string{to},
		"Template": map[string]any{
			"TemplateID":   s.cfg.SES.TemplateID,
			"TemplateData": templateData,
		},
		// 1 = 触发类邮件（验证码这类即时邮件）
		"TriggerType": 1,
	}
	if s.cfg.SES.ReplyTo != "" {
		payload["ReplyToAddresses"] = s.cfg.SES.ReplyTo
	}

	var out sendEmailResponse
	if err := tencent.Call(ctx, s.cred, "ses", "2020-10-02", s.cfg.SES.Region, "SendEmail", payload, &out); err != nil {
		log.Printf("[email] SES SendEmail failed: %v", err)
		return "", httpx.BadGateway("验证码邮件发送失败：" + err.Error())
	}
	return "", nil
}

// templateDataJSON 生成 {"<var>":"<code>"}，对编码后的值做最小转义。
func templateDataJSON(name, code string) (string, error) {
	if name == "" {
		name = "code"
	}
	if strings.ContainsAny(name, `"\\`) {
		return "", fmt.Errorf("SES_TEMPLATE_VAR 含有非法字符")
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(code)
	return `{"` + name + `":"` + escaped + `"}`, nil
}
