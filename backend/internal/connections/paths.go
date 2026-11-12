package connections

import (
	"fmt"
	"path/filepath"
	"strings"

	"everythingabout.me/backend/internal/config"
)

// ValidateName keeps a connection name usable as one path component. Two things depend
// on that: owner bindings are written as name:digest, so ':' would make the separator
// ambiguous, and an object connection derives its evidence and spool paths from the
// name, so a separator or a parent reference would put those files somewhere the
// operator did not name.
func ValidateName(name string) error {
	if strings.Contains(name, ":") {
		return fmt.Errorf("storage connection %q: a name cannot contain ':' because owner bindings are written as name:digest", name)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, 0) {
		return fmt.Errorf("storage connection %q: a name must be a single path-safe label without '/', '\\', NUL, \".\" or \"..\"", name)
	}
	return nil
}

// ObjectPaths derives where one object connection keeps the two things a connection
// must own: its admission evidence and its spool.
//
// Both are per connection. Evidence describes one endpoint, so a second connection
// that inherited the first file would be refused when it loaded that evidence, and a
// probe run against the inherited path would overwrite the other connection's proof;
// a shared spool would let one target's temporary material be published to another.
// The deployment's paths therefore act as directories, and the connection name is
// the leaf that separates them.
func ObjectPaths(deployment config.Storage, name, backend, admissionFlag, spoolFlag string) (string, string, error) {
	if err := ValidateName(name); err != nil {
		return "", "", err
	}
	if backend == "local" {
		// A local connection stages inside its own system root; it has no object
		// admission evidence and no spool.
		return admissionFlag, spoolFlag, nil
	}
	admission, spool := admissionFlag, spoolFlag
	if admission == "" {
		if !filepath.IsAbs(deployment.ObjectAdmissionFile) {
			return "", "", fmt.Errorf("connection %q needs its own admission evidence path: this deployment has no object admission file to derive one from, so pass one explicitly", name)
		}
		admission = filepath.Join(filepath.Dir(deployment.ObjectAdmissionFile), "admission-"+name+".json")
	}
	if spool == "" {
		if !filepath.IsAbs(deployment.ObjectSpoolRoot) {
			return "", "", fmt.Errorf("connection %q needs its own spool root: this deployment has no object spool root to derive one from, so pass one explicitly", name)
		}
		spool = filepath.Join(deployment.ObjectSpoolRoot, name)
	}
	for _, owned := range []struct{ what, path, deployment string }{
		{"admission evidence", admission, deployment.ObjectAdmissionFile},
		{"spool", spool, deployment.ObjectSpoolRoot},
	} {
		if owned.deployment != "" && filepath.Clean(owned.path) == filepath.Clean(owned.deployment) {
			return "", "", fmt.Errorf("connection %q would share the deployment's %s %s; each connection owns its own, because evidence describes one endpoint and a shared spool would let one target's temporary bytes be published to another",
				name, owned.what, owned.path)
		}
	}
	return admission, spool, nil
}
