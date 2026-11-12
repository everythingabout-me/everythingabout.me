package connections

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"everythingabout.me/backend/internal/config"
)

// The variables an environment-seeded connection references. They are exactly the
// names config.Load already reads, so a deployment whose environment works needs no
// second mapping: the row names what the process already has.
const (
	EnvCOSSecretID       = "COS_SECRET_ID"
	EnvCOSSecretKey      = "COS_SECRET_KEY"
	EnvS3AccessKeyID     = "S3_ACCESS_KEY_ID"
	EnvS3SecretAccessKey = "S3_SECRET_ACCESS_KEY"
	EnvS3SessionToken    = "S3_SESSION_TOKEN"
)

// LookupFunc is os.LookupEnv's shape, so resolution can be tested with an explicit
// environment instead of mutating the process.
type LookupFunc func(string) (string, bool)

// EnvironmentReference is what an environment-seeded connection stores instead of
// the credential values. It is fixed to the variables the deployment already uses;
// inventing a second name for a key already in .env would be one more place for the
// two to drift.
//
// An optional S3 session token is named only when the environment has one. A named
// variable is required (a missing or empty value refuses the connection), so a row
// without temporary credentials must not name the token variable: its absence is
// the long-term-credential case, not a missing value.
func EnvironmentReference(cfg config.Config) SecretReference {
	switch cfg.BackendName() {
	case "cos":
		return SecretReference{COS: COSSecretReference{SecretID: EnvCOSSecretID, SecretKey: EnvCOSSecretKey}}
	case "s3":
		s3 := S3SecretReference{AccessKeyID: EnvS3AccessKeyID, SecretAccessKey: EnvS3SecretAccessKey}
		if cfg.Storage.S3.SessionToken != "" {
			s3.SessionToken = EnvS3SessionToken
		}
		return SecretReference{S3: s3}
	}
	return SecretReference{}
}

// Empty reports whether this reference names no variable at all.
func (r SecretReference) Empty() bool {
	return r.COS.SecretID == "" && r.COS.SecretKey == "" &&
		r.S3.AccessKeyID == "" && r.S3.SecretAccessKey == "" && r.S3.SessionToken == ""
}

// Names lists the referenced variables for an operator-facing message. It never
// contains a value.
func (r SecretReference) Names() []string {
	var names []string
	for _, name := range []string{r.COS.SecretID, r.COS.SecretKey, r.S3.AccessKeyID, r.S3.SecretAccessKey, r.S3.SessionToken} {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// Empty reports whether the row carries no inline credential at all.
func (s Secret) Empty() bool {
	return s.COS.SecretID == "" && s.COS.SecretKey == "" &&
		s.S3.AccessKeyID == "" && s.S3.SecretAccessKey == "" && s.S3.SessionToken == ""
}

// ResolveSecret returns the credential values this connection should use. A row
// with a reference resolves every variable it names; a row without one uses its
// inline copy.
//
// A broken reference is fatal for that connection and never falls back to an
// inline copy. The fallback would be worse than the outage it hides: after a
// rotation the inline copy is exactly the retired key, so the service would keep
// signing with a credential the operator believes is gone.
func (c Connection) ResolveSecret(lookup LookupFunc) (Secret, error) {
	if c.SecretReference.Empty() {
		return c.Secret, nil
	}
	if !c.Secret.Empty() {
		return Secret{}, fmt.Errorf("storage connection %q: both an inline secret and an environment reference are present; refusing to choose between them", c.Name)
	}
	if err := c.validateSecretReference(); err != nil {
		return Secret{}, err
	}
	return c.resolveReference(c.SecretReference, lookup)
}

func (c Connection) resolveReference(reference SecretReference, lookup LookupFunc) (Secret, error) {
	resolve := func(field, variable string) (string, error) {
		if variable == "" {
			return "", fmt.Errorf("storage connection %q: the environment reference does not name a variable for %s", c.Name, field)
		}
		value, ok := lookup(variable)
		if !ok {
			return "", fmt.Errorf("storage connection %q: environment variable %s is not set, and %s is kept only there; refusing to serve this connection instead of falling back to an inline copy — set %s in .env and restart", c.Name, variable, field, variable)
		}
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("storage connection %q: environment variable %s is empty, and %s is kept only there; refusing to serve this connection instead of falling back to an inline copy — set %s in .env and restart", c.Name, variable, field, variable)
		}
		return value, nil
	}
	var secret Secret
	switch c.Backend {
	case "cos":
		var err error
		if secret.COS.SecretID, err = resolve("secret.cos.secretId", reference.COS.SecretID); err != nil {
			return Secret{}, err
		}
		if secret.COS.SecretKey, err = resolve("secret.cos.secretKey", reference.COS.SecretKey); err != nil {
			return Secret{}, err
		}
	case "s3":
		var err error
		if secret.S3.AccessKeyID, err = resolve("secret.s3.accessKeyId", reference.S3.AccessKeyID); err != nil {
			return Secret{}, err
		}
		if secret.S3.SecretAccessKey, err = resolve("secret.s3.secretAccessKey", reference.S3.SecretAccessKey); err != nil {
			return Secret{}, err
		}
		if reference.S3.SessionToken != "" {
			if secret.S3.SessionToken, err = resolve("secret.s3.sessionToken", reference.S3.SessionToken); err != nil {
				return Secret{}, err
			}
		}
	default:
		return Secret{}, fmt.Errorf("storage connection %q: backend %q cannot resolve an environment reference", c.Name, c.Backend)
	}
	return secret, nil
}

// ApplyEnvironment resolves this row against the process environment and then
// folds it into configuration exactly as Apply does. It is the only path a row
// with a reference may take to a driver: Apply itself refuses an unresolved
// reference so an empty secret cannot silently become the credential.
func (c Connection) ApplyEnvironment(base config.Config) (config.Config, error) {
	secret, err := c.ResolveSecret(os.LookupEnv)
	if err != nil {
		return base, err
	}
	c.Secret = secret
	c.SecretReference = SecretReference{}
	return c.Apply(base)
}

// AdoptEnvironmentSecret converts the row the environment seeded from an inline
// credential copy to a reference. It runs once per database, at startup.
//
// The conversion is irreversible for the row, so it verifies the environment
// still describes what the row was created from before discarding the copy. If
// they disagree it refuses instead of picking a winner: the inline value is the
// last copy of the key the service has actually been using, while the environment
// may have been edited by mistake, for another host, or for a tree that moved.
//
// It also verifies an already-converted row's reference, so a variable removed
// from .env fails at startup with the variable's name rather than on the first
// request that needs the connection.
func AdoptEnvironmentSecret(ctx context.Context, store Store, cfg config.Config, lookup LookupFunc) error {
	if cfg.BackendName() == "local" {
		return nil
	}
	name := cfg.Storage.ConnectionID
	c, ok, err := store.ByName(ctx, "", name)
	if err != nil {
		return fmt.Errorf("read the environment-seeded storage connection %q: %w", name, err)
	}
	if !ok || c.Role != RoleAuthority {
		return nil
	}
	if !c.SecretReference.Empty() {
		if _, err := c.ResolveSecret(lookup); err != nil {
			return err
		}
		return nil
	}
	if c.Secret.Empty() {
		return nil
	}
	if diffs := environmentDifferences(c, cfg); len(diffs) > 0 {
		return environmentMismatchError(c, diffs)
	}
	reference := EnvironmentReference(cfg)
	// Resolve the reference before the update: discarding the only stored copy
	// and then discovering that the variable is empty would leave the connection
	// with no usable credential at all.
	probe := c
	probe.Secret = Secret{}
	probe.SecretReference = reference
	if _, err := probe.ResolveSecret(lookup); err != nil {
		return err
	}
	if err := store.SetSecretReference(ctx, c.ID, reference); err != nil {
		return fmt.Errorf("convert storage connection %q to an environment reference: %w", c.Name, err)
	}
	slog.Info("storage connection credentials now reference the environment", "name", c.Name, "variables", strings.Join(reference.Names(), ", "))
	return nil
}

// RotateToEnvironment points an environment-seeded authority row at the current
// environment credentials and discards its inline copy. The operator runs it to
// resolve a conversion mismatch, and to change which variables back a row; ordinary
// rotation after conversion is only an .env edit plus a restart, because the row
// already names the variables and is re-read at startup.
//
// It refuses a location change. A different bucket, region or endpoint is a
// different tree, not a rotated key, and moving a tree needs a rebind (new
// generation, change-stream reset, index rebuild) that this command must not
// perform by accident.
func RotateToEnvironment(ctx context.Context, store Store, cfg config.Config, name string, lookup LookupFunc) (Connection, bool, error) {
	if cfg.BackendName() == "local" {
		return Connection{}, false, fmt.Errorf("the environment configures a local connection, which has no credentials to rotate")
	}
	c, ok, err := store.ByName(ctx, "", name)
	if err != nil {
		return Connection{}, false, fmt.Errorf("read storage connection %q: %w", name, err)
	}
	if !ok {
		return Connection{}, false, fmt.Errorf("storage connection %q does not exist as a deployment default; list the rows with `eam storage-connection list`", name)
	}
	if c.Role != RoleAuthority {
		return Connection{}, false, fmt.Errorf("storage connection %q has role %q; only the environment-seeded authority row can reference the environment — an account-added connection keeps its inline secret because its key has no deployment environment to point at", name, c.Role)
	}
	var locationDiffs []difference
	for _, diff := range environmentDifferences(c, cfg) {
		if diff.kind == locationDifference {
			locationDiffs = append(locationDiffs, diff)
		}
	}
	if len(locationDiffs) > 0 {
		return Connection{}, false, environmentMismatchError(c, locationDiffs)
	}
	reference := EnvironmentReference(cfg)
	probe := c
	probe.Secret = Secret{}
	probe.SecretReference = reference
	if _, err := probe.ResolveSecret(lookup); err != nil {
		return Connection{}, false, err
	}
	already := !c.SecretReference.Empty()
	if err := store.SetSecretReference(ctx, c.ID, reference); err != nil {
		return Connection{}, false, fmt.Errorf("point storage connection %q at the environment: %w", name, err)
	}
	c.Secret = Secret{}
	c.SecretReference = reference
	return c, already, nil
}

// difference is one disagreement between an environment-seeded row and the
// environment, with values only where they are safe to print.
type difference struct {
	kind   differenceKind
	field  string
	row    string
	env    string
	hidden bool
}

type differenceKind int

const (
	locationDifference differenceKind = iota
	credentialDifference
)

func (d difference) String() string {
	if d.hidden {
		return fmt.Sprintf("%s: row and environment differ (values are never printed)", d.field)
	}
	return fmt.Sprintf("%s: row %q, environment %q", d.field, d.row, d.env)
}

// environmentDifferences compares everything the row was created from: backend,
// location and credentials. Secret-key and session-token values are compared but
// never printed; a secret id is an identifier, not a signing credential, and is
// part of what the operator needs to see to resolve the disagreement.
func environmentDifferences(c Connection, cfg config.Config) []difference {
	var diffs []difference
	add := func(kind differenceKind, field, row, env string, hidden bool) {
		if row == env {
			return
		}
		diffs = append(diffs, difference{kind: kind, field: field, row: row, env: env, hidden: hidden})
	}
	location := func(field, row, env string) { add(locationDifference, field, row, env, false) }
	credential := func(field, row, env string) { add(credentialDifference, field, row, env, false) }
	hidden := func(field, row, env string) { add(credentialDifference, field, row, env, true) }
	location("backend", c.Backend, cfg.BackendName())
	switch cfg.BackendName() {
	case "cos":
		location("bucket", c.Config.COS.Bucket, cfg.COS.Bucket)
		location("region", c.Config.COS.Region, cfg.COS.Region)
		credential("secret-id", c.Secret.COS.SecretID, cfg.COS.SecretID)
		hidden("secret-key", c.Secret.COS.SecretKey, cfg.COS.SecretKey)
	case "s3":
		location("endpoint", c.Config.S3.Endpoint, cfg.Storage.S3.Endpoint)
		location("region", c.Config.S3.Region, cfg.Storage.S3.Region)
		location("bucket", c.Config.S3.Bucket, cfg.Storage.S3.Bucket)
		location("content-prefix", c.Config.S3.ContentPrefix, cfg.Storage.S3.ContentPrefix)
		location("system-prefix", c.Config.S3.SystemPrefix, cfg.Storage.S3.SystemPrefix)
		location("path-style", strconv.FormatBool(c.Config.S3.PathStyle), strconv.FormatBool(cfg.Storage.S3.PathStyle))
		location("root-at-bucket", strconv.FormatBool(c.Config.RootAtBucket), strconv.FormatBool(cfg.Storage.RootAtBucket))
		credential("secret-id", c.Secret.S3.AccessKeyID, cfg.Storage.S3.AccessKeyID)
		hidden("secret-key", c.Secret.S3.SecretAccessKey, cfg.Storage.S3.SecretAccessKey)
		hidden("session-token", c.Secret.S3.SessionToken, cfg.Storage.S3.SessionToken)
	}
	return diffs
}

// environmentMismatchError refuses a conversion and names both what disagreed and
// the one command that resolves it. A credential-only disagreement is a rotation;
// a location disagreement is a tree move and needs different work, so the error
// says so rather than sending the operator to a command that would fail.
func environmentMismatchError(c Connection, diffs []difference) error {
	var b strings.Builder
	fmt.Fprintf(&b, "storage connection %q was seeded from the environment, but the environment no longer matches what the row was created from:\n", c.Name)
	moved := false
	for _, d := range diffs {
		fmt.Fprintf(&b, "  %s\n", d)
		if d.kind == locationDifference {
			moved = true
		}
	}
	b.WriteString("the row still holds the credentials it was created with and the environment holds different ones; refusing to keep either silently.\n")
	if moved {
		b.WriteString("rotation replaces credentials only and never moves a tree. Restore the location variables in .env to the row's values; if the tree really moved, add a connection for the new location with `eam storage-connection add --name <new-name> --generation <n+1> ...` and rebind owners, then rebuild the index.\n")
	} else {
		fmt.Fprintf(&b, "run `eam storage-connection rotate --name %s --from-environment` to adopt the environment's credentials for this row.\n", c.Name)
	}
	return errors.New(strings.TrimSuffix(b.String(), "\n"))
}
