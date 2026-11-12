package fileops

import (
	"testing"
	"time"
)

// TestUploadStateAt covers the derivation a listing relies on: an open session past
// expires_at is reported as expired without the row being written. The boundary is inclusive
// because expireLoadedUpload uses the same `ExpiresAt.After(now)` test, so a derived and a
// settled answer never disagree.
func TestUploadStateAt(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		u     Upload
		state UploadState
	}{
		{"open before expiry", Upload{State: UploadOpen, ExpiresAt: now.Add(time.Second)}, UploadOpen},
		{"open at expiry", Upload{State: UploadOpen, ExpiresAt: now}, UploadExpired},
		{"open past expiry", Upload{State: UploadOpen, ExpiresAt: now.Add(-time.Hour)}, UploadExpired},
		// Derivation is only about time; a settled session keeps its stored answer even if
		// its old deadline has passed.
		{"aborted stays aborted", Upload{State: UploadAborted, ExpiresAt: now.Add(-time.Hour)}, UploadAborted},
		{"expired stays expired", Upload{State: UploadExpired, ExpiresAt: now.Add(time.Hour)}, UploadExpired},
		// The committing projection composes with the expiry derivation instead of
		// replacing it.
		{
			"committing follows its operation",
			Upload{State: UploadCommitting, ExpiresAt: now.Add(-time.Hour), Operation: Operation{State: Succeeded}},
			UploadCommitted,
		},
		{
			"committing with an unknown outcome",
			Upload{State: UploadCommitting, ExpiresAt: now.Add(time.Hour), Operation: Operation{State: Unknown}},
			UploadUnknown,
		},
	} {
		if got := uploadStateAt(tt.u, now); got != tt.state {
			t.Errorf("%s: uploadStateAt = %q, want %q", tt.name, got, tt.state)
		}
	}
}
