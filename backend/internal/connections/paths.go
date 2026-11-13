package connections

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storage"
)

// ValidateName keeps a connection name usable as one path component. Owner bindings
// are written as name:digest, so ':' would make the separator ambiguous; the rest of
// the rule keeps a name a single label that an error message, a CLI argument and a log
// line can carry verbatim. A name is still only a human label: no on-disk path is
// derived from it (see ObjectPaths).
func ValidateName(name string) error {
	if strings.Contains(name, ":") {
		return fmt.Errorf("storage connection %q: a name cannot contain ':' because owner bindings are written as name:digest", name)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, 0) {
		return fmt.Errorf("storage connection %q: a name must be a single path-safe label without '/', '\\', NUL, \".\" or \"..\"", name)
	}
	return nil
}

// NewID returns the primary key for a connection row that has not been inserted yet.
//
// The row id is the connection's durable identity, so a caller that must place
// on-disk state before the INSERT generates the id here and writes the row with it.
// 'gen_random_uuid()' remains the fallback for a row inserted without one.
func NewID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate a storage connection id: %w", err)
	}
	raw[6] = raw[6]&0x0f | 0x40 // version 4
	raw[8] = raw[8]&0x3f | 0x80 // variant 10xx
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// validateKey checks that a row id can be used as one path component. The column is a
// uuid, so this only rejects a row that was written by hand or by a future schema; the
// check exists so a bad value fails here instead of placing a connection's spool
// outside the directory the operator configured.
func validateKey(c Connection) error {
	if c.ID == "" || c.ID == "." || c.ID == ".." || strings.ContainsAny(c.ID, "/\\") || strings.ContainsRune(c.ID, 0) {
		return fmt.Errorf("storage connection %q has no usable id (%q) to name its on-disk state: a connection's evidence and spool are keyed by its row id", c.Name, c.ID)
	}
	return nil
}

// derivedAdmission and derivedSpool are the id-keyed derivation of a connection's two
// pieces of on-disk state. Derived is separate from ObjectPaths so adoption can ask
// "where would the id put this?" and "where did the name put it?" without restating
// the layout.
func derivedAdmission(deployment config.Storage, key string) (string, bool) {
	if !filepath.IsAbs(deployment.ObjectAdmissionFile) {
		return "", false
	}
	return filepath.Join(filepath.Dir(deployment.ObjectAdmissionFile), "admission-"+key+".json"), true
}

func derivedSpool(deployment config.Storage, key string) (string, bool) {
	if !filepath.IsAbs(deployment.ObjectSpoolRoot) {
		return "", false
	}
	return filepath.Join(deployment.ObjectSpoolRoot, key), true
}

// ObjectPaths derives where one object connection keeps the two things a connection
// must own: its admission evidence and its spool.
//
// Both are per connection. Evidence describes one endpoint, so a second connection
// that inherited the first file would be refused when it loaded that evidence, and a
// probe run against the inherited path would overwrite the other connection's proof;
// a shared spool would let one target's temporary material be published to another.
//
// The leaf is the row id, never the name. 036 lets two owners each call a connection
// "nas", so the name is not unique and cannot separate two owners' state: with a
// name-keyed leaf both owners staged into one directory, both wrote one evidence file,
// and the two accounts shared a namespace on the host even though every row and query
// is owner-scoped. The id is the row's durable identity, so the directory survives a
// rename, while the name stays where a person reads it: logs, errors and the UI.
func ObjectPaths(deployment config.Storage, c Connection, admissionFlag, spoolFlag string) (string, string, error) {
	if err := validateKey(c); err != nil {
		return "", "", err
	}
	if c.Backend == "local" {
		// A local connection stages inside its own system root; it has no object
		// admission evidence and no spool.
		return admissionFlag, spoolFlag, nil
	}
	admission, spool := admissionFlag, spoolFlag
	if admission == "" {
		derived, ok := derivedAdmission(deployment, c.ID)
		if !ok {
			return "", "", fmt.Errorf("connection %q needs its own admission evidence path: this deployment has no object admission file to derive one from, so pass one explicitly", c.Name)
		}
		admission = derived
	}
	if spool == "" {
		derived, ok := derivedSpool(deployment, c.ID)
		if !ok {
			return "", "", fmt.Errorf("connection %q needs its own spool root: this deployment has no object spool root to derive one from, so pass one explicitly", c.Name)
		}
		spool = derived
	}
	for _, owned := range []struct{ what, path, deployment string }{
		{"admission evidence", admission, deployment.ObjectAdmissionFile},
		{"spool", spool, deployment.ObjectSpoolRoot},
	} {
		if owned.deployment != "" && filepath.Clean(owned.path) == filepath.Clean(owned.deployment) {
			return "", "", fmt.Errorf("connection %q would share the deployment's %s %s; each connection owns its own, because evidence describes one endpoint and a shared spool would let one target's temporary bytes be published to another",
				c.Name, owned.what, owned.path)
		}
	}
	return admission, spool, nil
}

// S3ContentPrefix reads the prefix an account chose for its own S3-compatible drive.
// nil means "no choice was made", so the deployment default applies; an explicit empty
// string means the bucket itself — the two are different requests and must not collapse
// into each other, which is why the input is a pointer.
//
// The prefix must not name the service's private system folder: that folder lives in the
// same bucket, and a tree rooted at or inside it would write service material into user
// content.
func S3ContentPrefix(chosen *string) (prefix string, rootAtBucket bool, err error) {
	if chosen == nil {
		return "drive/", false, nil
	}
	trimmed := strings.Trim(strings.TrimSpace(*chosen), "/")
	if trimmed == "" {
		return "", true, nil
	}
	if storage.ValidatePath(storage.Path(trimmed)) != nil {
		return "", false, fmt.Errorf("prefix %q is not a valid native path", *chosen)
	}
	if trimmed == "__eam_system__" || strings.HasPrefix(trimmed, "__eam_system__/") {
		return "", false, fmt.Errorf("prefix %q is reserved for the service; use another one", *chosen)
	}
	return trimmed + "/", false, nil
}
