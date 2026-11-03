package config

import "fmt"

// EdgeOneUploadIngress sends signed file-part bytes through ordinary same-origin
// EdgeOne forwarding to COS. No application receiver or edge function handles them.
func (c *Config) EdgeOneUploadIngress() bool { return c.FileUploadIngress == "edgeone" }

// ExternalUploadIngress identifies modes in which the app must refuse byte PUTs.
func (c *Config) ExternalUploadIngress() bool { return c.EdgeOneUploadIngress() }

func (c *Config) ValidateUploadIngress() error {
	switch c.FileUploadIngress {
	case "", "embedded":
		return nil
	case "edgeone":
		if !c.Storage.CoordinatedWrites || c.BackendName() != "cos" {
			return fmt.Errorf("edgeone upload ingress requires admitted COS coordinated writes")
		}
		if c.COS.UploadSignSecretID == "" || c.COS.UploadSignSecretKey == "" || c.COS.UploadSignSecretID == c.COS.SecretID {
			return fmt.Errorf("edgeone ingress requires separate COS_UPLOAD_SIGN_SECRET_ID and COS_UPLOAD_SIGN_SECRET_KEY limited to UploadPart")
		}
		return nil
	default:
		return fmt.Errorf("FILE_UPLOAD_INGRESS must be embedded or edgeone; external upload gateways are no longer supported")
	}
}
