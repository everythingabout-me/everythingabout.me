// Package mail sends email verification codes (Tencent Cloud SES).
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

// SendVerificationCode sends a verification code in the given locale ("zh" or "en"; anything else
// falls back to English, the default template).
//   - SES configured: send real mail
//   - Not configured and in development: log it on the server and return the code so the
//     page can fill it in automatically
//   - Not configured and in production: report an error explicitly, never stay silent
func (s *Sender) SendVerificationCode(ctx context.Context, to, code, locale string) (string, error) {
	if !s.Configured() {
		if s.cfg.IsProduction() {
			return "", httpx.ServiceUnavailable("email service is not configured; cannot send verification codes")
		}
		log.Printf("[email:dev] verification code %s -> %s", code, to)
		return code, nil
	}

	subject, templateID := s.emailTemplate(locale)

	templateData, err := templateDataJSON(s.cfg.SES.TemplateVar, code)
	if err != nil {
		return "", err
	}

	payload := map[string]any{
		"FromEmailAddress": s.cfg.SES.FromEmailAddress,
		"Subject":          subject,
		"Destination":      []string{to},
		"Template": map[string]any{
			"TemplateID":   templateID,
			"TemplateData": templateData,
		},
		// 1 = triggered mail (instant mail such as a verification code)
		"TriggerType": 1,
	}
	if s.cfg.SES.ReplyTo != "" {
		payload["ReplyToAddresses"] = s.cfg.SES.ReplyTo
	}

	var out sendEmailResponse
	if err := tencent.Call(ctx, s.cred, "ses", "2020-10-02", s.cfg.SES.Region, "SendEmail", payload, &out); err != nil {
		log.Printf("[email] SES SendEmail failed: %v", err)
		return "", httpx.BadGateway("failed to send the verification email: " + err.Error())
	}
	return "", nil
}

// emailTemplate returns the subject and SES template id for a locale. The default pair is English;
// the Chinese overrides are optional, and anything Chinese requests cannot resolve falls back to
// the default so a half-configured deployment still sends mail.
func (s *Sender) emailTemplate(locale string) (string, int64) {
	if locale != "zh" || s.cfg.SES.TemplateIDZh <= 0 {
		return s.cfg.SES.Subject, s.cfg.SES.TemplateID
	}
	subject := s.cfg.SES.SubjectZh
	if subject == "" {
		subject = s.cfg.SES.Subject
	}
	return subject, s.cfg.SES.TemplateIDZh
}

// templateDataJSON builds {"<var>":"<code>"}, minimally escaping the value.
func templateDataJSON(name, code string) (string, error) {
	if name == "" {
		name = "code"
	}
	if strings.ContainsAny(name, `"\\`) {
		return "", fmt.Errorf("SES_TEMPLATE_VAR contains invalid characters")
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(code)
	return `{"` + name + `":"` + escaped + `"}`, nil
}
