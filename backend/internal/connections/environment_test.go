package connections

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/config"
)

func testCOSConfig(secretID, secretKey, bucket, region string) config.Config {
	return config.Config{
		Storage: config.Storage{Backend: "cos", ConnectionID: "default", Generation: 1},
		COS:     config.COS{SecretID: secretID, SecretKey: secretKey, Bucket: bucket, Region: region},
	}
}

func envFor(values map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// legacyRow is a row exactly as the previous release wrote it: the environment's
// credential values copied inline, with no reference.
func legacyRow(t *testing.T, cfg config.Config, id string) Connection {
	t.Helper()
	row, err := FromConfig(cfg)
	if err != nil {
		t.Fatalf("build the row the environment used to seed: %v", err)
	}
	row.ID = id
	row.Secret = Secret{COS: COSCredential{SecretID: cfg.COS.SecretID, SecretKey: cfg.COS.SecretKey}}
	row.SecretReference = SecretReference{}
	return row
}

// A fresh database must not receive the seed's credential values. The row names
// the variables; the environment keeps the value.
func TestFromConfigStoresAReferenceInsteadOfTheSecret(t *testing.T) {
	cfg := testCOSConfig("AKIDseed", "seed-secret", "data-125", "ap-hongkong")
	row, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Secret.Empty() {
		t.Fatalf("the seeded row still holds the credential value: %+v", row.Secret)
	}
	if got, want := row.SecretReference.Names(), []string{EnvCOSSecretID, EnvCOSSecretKey}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seed reference names %v, want %v", got, want)
	}
	if _, err := row.Apply(config.Config{}); err == nil {
		t.Fatal("Apply accepted an unresolved reference instead of requiring resolution")
	}

	t.Setenv(EnvCOSSecretID, "AKIDseed")
	t.Setenv(EnvCOSSecretKey, "seed-secret")
	effective, err := row.ApplyEnvironment(config.Config{})
	if err != nil {
		t.Fatalf("resolve the seeded row: %v", err)
	}
	if effective.COS.SecretID != "AKIDseed" || effective.COS.SecretKey != "seed-secret" {
		t.Fatal("the resolved configuration did not carry the environment's credentials")
	}
	if effective.COS.Bucket != "data-125" || effective.COS.Region != "ap-hongkong" {
		t.Fatal("resolving a reference changed the row's location")
	}
	if !row.Secret.Empty() {
		t.Fatal("resolving mutated the stored row")
	}
}

func TestResolveSecretNamesTheMissingOrEmptyVariable(t *testing.T) {
	cfg := testCOSConfig("AKIDseed", "seed-secret", "data-125", "ap-hongkong")
	row, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = row.ResolveSecret(envFor(nil))
	if err == nil || !strings.Contains(err.Error(), EnvCOSSecretID) || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("a missing variable was not named: %v", err)
	}
	_, err = row.ResolveSecret(envFor(map[string]string{EnvCOSSecretID: "AKIDseed"}))
	if err == nil || !strings.Contains(err.Error(), EnvCOSSecretKey) || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("the second missing variable was not named: %v", err)
	}
	_, err = row.ResolveSecret(envFor(map[string]string{EnvCOSSecretID: "AKIDseed", EnvCOSSecretKey: "   "}))
	if err == nil || !strings.Contains(err.Error(), EnvCOSSecretKey) || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("an empty variable was not named: %v", err)
	}
}

// A row that somehow carries both values cannot be resolved by preference: choosing
// the inline copy would silently keep using a retired key.
func TestResolveSecretRefusesAnInlineCopyBesideAReference(t *testing.T) {
	cfg := testCOSConfig("AKIDseed", "seed-secret", "data-125", "ap-hongkong")
	row, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	row.Secret = Secret{COS: COSCredential{SecretID: "stale-id", SecretKey: "stale-key"}}
	secret, err := row.ResolveSecret(envFor(map[string]string{EnvCOSSecretID: "AKIDseed", EnvCOSSecretKey: "seed-secret"}))
	if err == nil || !strings.Contains(err.Error(), "both an inline secret") {
		t.Fatalf("a row with two credential sources was accepted: %v", err)
	}
	if !secret.Empty() {
		t.Fatal("resolution returned a credential from a row it refused")
	}
}

func TestAdoptEnvironmentSecretConvertsAMatchingRow(t *testing.T) {
	cfg := testCOSConfig("AKIDseed", "seed-secret", "data-125", "ap-hongkong")
	store := newFakeStore(legacyRow(t, cfg, "row-1"))
	lookup := envFor(map[string]string{EnvCOSSecretID: "AKIDseed", EnvCOSSecretKey: "seed-secret"})
	if err := AdoptEnvironmentSecret(context.Background(), store, cfg, lookup); err != nil {
		t.Fatalf("convert a matching row: %v", err)
	}
	got := store.rows[0]
	if !got.Secret.Empty() {
		t.Fatal("the converted row still holds the credential value")
	}
	if want := []string{EnvCOSSecretID, EnvCOSSecretKey}; !reflect.DeepEqual(got.SecretReference.Names(), want) {
		t.Fatalf("converted reference names %v, want %v", got.SecretReference.Names(), want)
	}
	if secret, err := got.ResolveSecret(lookup); err != nil || secret.COS.SecretKey != "seed-secret" {
		t.Fatalf("the converted row does not resolve: %v", err)
	}
	// A repeated start is a no-op, not a second conversion.
	if err := AdoptEnvironmentSecret(context.Background(), store, cfg, lookup); err != nil {
		t.Fatalf("second conversion: %v", err)
	}
	if len(store.rows) != 1 || !reflect.DeepEqual(store.rows[0].SecretReference.Names(), []string{EnvCOSSecretID, EnvCOSSecretKey}) {
		t.Fatal("a repeated conversion changed the row")
	}
}

// The inline copy is the last copy of the key the service actually used, so a
// disagreement must stop the start rather than let either value win by default.
func TestAdoptEnvironmentSecretRefusesAMismatchAndKeepsTheInlineCopy(t *testing.T) {
	rowCfg := testCOSConfig("row-id", "row-key", "row-bucket-125", "ap-hongkong")
	envCfg := testCOSConfig("env-id", "env-key", "env-bucket-125", "ap-beijing")
	store := newFakeStore(legacyRow(t, rowCfg, "row-1"))
	err := AdoptEnvironmentSecret(context.Background(), store, envCfg, envFor(map[string]string{EnvCOSSecretID: "env-id", EnvCOSSecretKey: "env-key"}))
	if err == nil {
		t.Fatal("a row that disagrees with the environment was converted anyway")
	}
	// A location disagreement is a tree move, so the report names the add command,
	// not rotate: rotation must not be offered as the fix for the wrong tree.
	for _, want := range []string{"bucket", "region", "secret-id", "eam storage-connection add"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mismatch report does not mention %q:\n%v", want, err)
		}
	}
	// The report names the secret id, which is an identifier, but never either key.
	for _, forbidden := range []string{"row-key", "env-key"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("mismatch report printed a secret key: %v", err)
		}
	}
	if store.rows[0].Secret.COS.SecretKey != "row-key" || !store.rows[0].SecretReference.Empty() {
		t.Fatal("a refused conversion still changed the row")
	}
}

// A credential-only disagreement is a rotation, and the report says which command
// adopts the environment's value.
func TestAdoptEnvironmentSecretSendsACredentialMismatchToRotate(t *testing.T) {
	rowCfg := testCOSConfig("old-id", "old-key", "data-125", "ap-hongkong")
	envCfg := testCOSConfig("new-id", "new-key", "data-125", "ap-hongkong")
	store := newFakeStore(legacyRow(t, rowCfg, "row-1"))
	err := AdoptEnvironmentSecret(context.Background(), store, envCfg, envFor(map[string]string{EnvCOSSecretID: "new-id", EnvCOSSecretKey: "new-key"}))
	if err == nil || !strings.Contains(err.Error(), "rotate --name default --from-environment") {
		t.Fatalf("credential mismatch did not name the rotate command: %v", err)
	}
	if strings.Contains(err.Error(), "never moves a tree") {
		t.Fatal("a credential-only disagreement was reported as a tree move")
	}
	if strings.Contains(err.Error(), "old-key") || strings.Contains(err.Error(), "new-key") {
		t.Fatalf("credential mismatch printed a secret key: %v", err)
	}
}

func TestRotateToEnvironmentAdoptsCredentialsWithoutMovingTheTree(t *testing.T) {
	rowCfg := testCOSConfig("old-id", "old-key", "data-125", "ap-hongkong")
	envCfg := testCOSConfig("new-id", "new-key", "data-125", "ap-hongkong")
	store := newFakeStore(legacyRow(t, rowCfg, "row-1"))
	lookup := envFor(map[string]string{EnvCOSSecretID: "new-id", EnvCOSSecretKey: "new-key"})
	updated, already, err := RotateToEnvironment(context.Background(), store, envCfg, "default", lookup)
	if err != nil {
		t.Fatalf("rotate credentials: %v", err)
	}
	if already {
		t.Fatal("a row that held an inline secret was reported as already referencing the environment")
	}
	if !updated.Secret.Empty() || updated.SecretReference.COS.SecretKey != EnvCOSSecretKey {
		t.Fatalf("rotated row %+v", updated)
	}
	if secret, err := store.rows[0].ResolveSecret(lookup); err != nil || secret.COS.SecretKey != "new-key" {
		t.Fatalf("the rotated row does not resolve the new value: %v", err)
	}
	if _, already, err = RotateToEnvironment(context.Background(), store, envCfg, "default", lookup); err != nil || !already {
		t.Fatalf("second rotation: already=%t err=%v", already, err)
	}
}

func TestRotateRefusesToMoveTheTree(t *testing.T) {
	rowCfg := testCOSConfig("id", "key", "old-bucket-125", "ap-hongkong")
	envCfg := testCOSConfig("id", "key", "new-bucket-125", "ap-hongkong")
	store := newFakeStore(legacyRow(t, rowCfg, "row-1"))
	_, _, err := RotateToEnvironment(context.Background(), store, envCfg, "default", envFor(map[string]string{EnvCOSSecretID: "id", EnvCOSSecretKey: "key"}))
	if err == nil || !strings.Contains(err.Error(), "never moves a tree") || !strings.Contains(err.Error(), "bucket") {
		t.Fatalf("a location change was accepted as a rotation: %v", err)
	}
	if store.rows[0].Secret.COS.SecretKey != "key" {
		t.Fatal("a refused rotation changed the row")
	}
}

// After conversion, the row is unchanged by a rotation: the new value comes from
// the environment when the connection is built, which is exactly why no UPDATE path
// is needed for an ordinary key rotation.
func TestARotatedEnvironmentValueIsUsedWithoutARowUpdate(t *testing.T) {
	cfg := testCOSConfig("AKIDseed", "first-key", "data-125", "ap-hongkong")
	row, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeStore(row)
	before := store.rows[0]

	t.Setenv(EnvCOSSecretID, "AKIDseed")
	t.Setenv(EnvCOSSecretKey, "first-key")
	first, err := store.rows[0].ApplyEnvironment(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCOSSecretKey, "rotated-key")
	second, err := store.rows[0].ApplyEnvironment(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if first.COS.SecretKey != "first-key" || second.COS.SecretKey != "rotated-key" {
		t.Fatalf("resolved keys %q then %q", first.COS.SecretKey, second.COS.SecretKey)
	}
	if !reflect.DeepEqual(before, store.rows[0]) {
		t.Fatal("the rotation needed a row update; the row must only name the variable")
	}
}

// A converted row is verified at startup too: removing a variable fails by name
// instead of waiting for the first request.
func TestAdoptEnvironmentSecretVerifiesAnExistingReference(t *testing.T) {
	cfg := testCOSConfig("AKIDseed", "seed-secret", "data-125", "ap-hongkong")
	row, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	row.ID = "row-1"
	store := newFakeStore(row)
	err = AdoptEnvironmentSecret(context.Background(), store, cfg, envFor(nil))
	if err == nil || !strings.Contains(err.Error(), EnvCOSSecretID) {
		t.Fatalf("a broken reference did not fail by variable name: %v", err)
	}
}

func TestS3ReferenceNamesTheSessionTokenOnlyWhenOneExists(t *testing.T) {
	cfg := config.Config{Storage: config.Storage{Backend: "s3", ConnectionID: "default", Generation: 1,
		S3: config.S3{Endpoint: "https://nas.invalid", Region: "us-east-1", Bucket: "backup", SessionToken: "token"}}}
	names := EnvironmentReference(cfg).Names()
	if !reflect.DeepEqual(names, []string{EnvS3AccessKeyID, EnvS3SecretAccessKey, EnvS3SessionToken}) {
		t.Fatalf("reference names %v", names)
	}
	cfg.Storage.S3.SessionToken = ""
	names = EnvironmentReference(cfg).Names()
	if !reflect.DeepEqual(names, []string{EnvS3AccessKeyID, EnvS3SecretAccessKey}) {
		t.Fatalf("long-term reference names %v", names)
	}
}
