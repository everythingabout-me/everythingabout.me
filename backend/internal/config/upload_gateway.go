package config

import (
	"fmt"

	"everythingabout.me/backend/internal/storage"
)

// EdgeOneUploadIngress selects the optional same-origin edge relay for class A
// part uploads. It is a transport detail of class A, never a storage class.
func (c *Config) EdgeOneUploadIngress() bool { return c.FileUploadIngress == "edgeone" }

// ExternalUploadIngress identifies modes in which the app must refuse byte PUTs.
func (c *Config) ExternalUploadIngress() bool { return c.EdgeOneUploadIngress() }

func (c *Config) ValidateUploadIngress() error {
	switch c.FileUploadIngress {
	case "", "embedded":
		return nil
	case "edgeone":
		if c.Storage.TransferClass != storage.TransferPresignedParts {
			return fmt.Errorf("edgeone upload relay is only available on a class A presigned-parts connection")
		}
		if !c.Storage.CoordinatedWrites || c.BackendName() != "cos" {
			return fmt.Errorf("edgeone upload ingress requires admitted COS coordinated writes")
		}
		if c.COS.SignSecretID == "" || c.COS.SignSecretKey == "" || c.COS.SignSecretID == c.COS.SecretID {
			return fmt.Errorf("edgeone ingress requires a distinct COS_SIGN_SECRET_ID and COS_SIGN_SECRET_KEY limited to UploadPart and GetObject")
		}
		return nil
	default:
		return fmt.Errorf("FILE_UPLOAD_INGRESS must be embedded or edgeone")
	}
}
