package objectmutation

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// LoadAdmission consumes explicit operator-owned probe evidence. It does not
// manufacture capability support or load ordinary provider credentials.
func LoadAdmission(path string) (Admission, error) {
	var a Admission
	if !filepath.IsAbs(path) {
		return a, storage.NewError("invalid_config", "", nil)
	}
	f, err := os.Open(path)
	if err != nil {
		return a, storage.NewError("invalid_config", "", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return a, storage.NewError("invalid_config", "", err)
	}
	d := json.NewDecoder(io.LimitReader(f, (1<<20)+1))
	d.DisallowUnknownFields()
	if err = d.Decode(&a); err != nil {
		return a, storage.NewError("invalid_config", "", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return a, storage.NewError("invalid_config", "", err)
	}
	if err = ValidateAdmission(a); err != nil {
		return a, err
	}
	return a, nil
}
func ValidateAdmission(a Admission) error {
	if strings.TrimSpace(a.ConnectionID) == "" || a.Endpoint == "" || a.Bucket == "" || a.Region == "" || a.CheckedAt.IsZero() || a.CheckedAt.After(time.Now().Add(5*time.Minute)) || strings.TrimSpace(a.ProbeReference) == "" || !a.AtomicPublish || !a.ConditionalCreate || !a.SingleGateway {
		return storage.NewError("invalid_config", "", nil)
	}
	return nil
}
func (a Admission) Matches(connection, endpoint, bucket, region string) bool {
	return a.ConnectionID == connection && a.Endpoint == endpoint && a.Bucket == bucket && a.Region == region
}
