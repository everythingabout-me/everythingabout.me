package config

import (
	"fmt"

	"everythingabout.me/backend/internal/filecontract"
)

// ExternalUploadIngress separates byte reception from the application HTTP process.
// Both processes still use the same public origin and durable upload database.
func (c *Config) ExternalUploadIngress() bool { return c.FileUploadIngress == "external" }

func (c *Config) ValidateUploadIngress() error {
	switch c.FileUploadIngress {
	case "", "embedded":
		return nil
	case "external":
		if !c.Storage.CoordinatedWrites || c.BackendName() == "local" {
			return fmt.Errorf("external upload ingress requires admitted COS or S3 coordinated writes")
		}
		return nil
	default:
		return fmt.Errorf("FILE_UPLOAD_INGRESS must be embedded or external")
	}
}

// ValidateUploadGateway validates the restricted receiver without requiring mail
// settings: a gateway has no login, account management, web or background workers.
func (c *Config) ValidateUploadGateway() error {
	if err := c.ValidateUploadIngress(); err != nil {
		return err
	}
	if c.AppSecret == "" {
		return fmt.Errorf("APP_SECRET must match the application deployment")
	}
	for _, origin := range c.FileCookieOrigins {
		if _, err := filecontract.NewCookieMutationPolicy(origin); err != nil {
			return fmt.Errorf("FILE_COOKIE_ORIGINS requires explicit HTTP(S) origins without paths")
		}
	}
	if !c.Storage.CoordinatedWrites || c.BackendName() == "local" {
		return fmt.Errorf("upload-gateway requires admitted COS or S3 coordinated writes")
	}
	return c.ValidateStorage()
}
