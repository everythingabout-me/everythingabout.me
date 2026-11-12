package objectmutation

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// invalidAdmission reports a configuration failure that is safe to show an
// operator: the message body is application-authored, so it carries the file and
// the offending field while storage.Error keeps hiding provider responses.
func invalidAdmission(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, storage.NewError("invalid_config", "", nil))...)
}

// LoadAdmission consumes explicit operator-owned probe evidence. It does not
// manufacture capability support or load ordinary provider credentials.
func LoadAdmission(path string) (Admission, error) {
	var a Admission
	if !filepath.IsAbs(path) {
		return a, invalidAdmission("object admission evidence path %q must be absolute", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return a, invalidAdmission("read object admission evidence %q: %v", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return a, invalidAdmission("read object admission evidence %q: %v", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return a, invalidAdmission("object admission evidence %q must be a regular file of at most 1 MiB", path)
	}
	d := json.NewDecoder(io.LimitReader(f, (1<<20)+1))
	d.DisallowUnknownFields()
	if err = d.Decode(&a); err != nil {
		return a, invalidAdmission("parse object admission evidence %q: %v", path, err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		if err == nil {
			return a, invalidAdmission("parse object admission evidence %q: unexpected trailing data", path)
		}
		return a, invalidAdmission("parse object admission evidence %q: %v", path, err)
	}
	if err = ValidateAdmission(a); err != nil {
		return a, err
	}
	// Evidence records which probe produced it. A file written by an older probe cannot
	// answer for a property that probe never measured, so it is refused here and re-measured
	// by the caller instead of aging into a half-answer.
	if a.Version < AdmissionVersion {
		return a, invalidAdmission("object admission evidence was written by probe %d and this build needs probe %d; it will be re-measured", a.Version, AdmissionVersion)
	}
	return a, nil
}

// AdmissionVersion names the probe whose evidence this build requires. It advances when a
// probe learns a property an older file cannot answer, because a capability measured by an
// older probe is not evidence about today's connection.
//
// 3 adds conditionalPut: whether a PUT carrying a stale If-Match is refused. Evidence written
// by probe 2 has no field for it, so keeping those files would leave direct class A
// replacement (whose only guard is that header) admitted on the strength of an assumption
// nobody measured. Re-measuring is the point of the version, not a side effect.
const AdmissionVersion = 3

func ValidateAdmission(a Admission) error {
	var problems []string
	if strings.TrimSpace(a.ConnectionID) == "" {
		problems = append(problems, "connectionId")
	}
	if a.Endpoint == "" {
		problems = append(problems, "endpoint")
	}
	if a.Bucket == "" {
		problems = append(problems, "bucket")
	}
	if a.Region == "" {
		problems = append(problems, "region")
	}
	if a.CheckedAt.IsZero() {
		problems = append(problems, "checkedAt")
	} else if a.CheckedAt.After(time.Now().Add(5 * time.Minute)) {
		problems = append(problems, "checkedAt must not be in the future")
	}
	if strings.TrimSpace(a.ProbeReference) == "" {
		problems = append(problems, "probeReference")
	}
	for _, capability := range []struct {
		name   string
		vetoed bool
	}{{"atomicPublish", !a.AtomicPublish}, {"conditionalCreate", !a.ConditionalCreate}, {"singleGateway", !a.SingleGateway}} {
		if capability.vetoed {
			problems = append(problems, capability.name+" is false")
		}
	}
	if len(problems) > 0 {
		return invalidAdmission("object admission evidence is incomplete: %s", strings.Join(problems, ", "))
	}
	return nil
}
func (a Admission) Matches(connection, endpoint, bucket, region string) bool {
	return a.ConnectionID == connection && a.Endpoint == endpoint && a.Bucket == bucket && a.Region == region
}
