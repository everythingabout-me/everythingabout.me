package connections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"everythingabout.me/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// Store is the durable half: the connection rows and the per-owner pointer that
// names one of them. The pointer is `file_budgets.root_binding`, which already
// records connection, generation and both roots, so no second pointer is added.
type Store interface {
	Connections(ctx context.Context) ([]Connection, error)
	Authority(ctx context.Context) (Connection, bool, error)
	ByName(ctx context.Context, ownerID, name string) (Connection, bool, error)
	Visible(ctx context.Context, ownerID string) ([]Connection, error)
	OwnerConnection(ctx context.Context, ownerID string) (string, bool, error)
	Insert(ctx context.Context, c Connection) error
	// SetSecretReference replaces a shared row's inline credentials with an
	// environment reference. It is scoped to shared rows: an account-added row
	// keeps its inline secret because its key has no deployment environment.
	SetSecretReference(ctx context.Context, id string, ref SecretReference) error
	// SetObjectPaths rewrites the two per-connection on-disk paths after adoption
	// moved them. It is scoped by row id, not name, so a rename cannot repoint
	// another row's evidence.
	SetObjectPaths(ctx context.Context, id, admissionFile, spoolRoot string) error
	// SetProbe caches the last measurement requested for one connection. It is
	// scoped by row id because a rename keeps the same store but a reused name
	// must not inherit someone else's result.
	SetProbe(ctx context.Context, id string, probe []byte) error
	// SetBackupFolder changes where snapshots are placed inside one account's own
	// connection. It refuses a row that already holds backup runs: those dates live
	// under the old folder, and a silent move would leave listed restore points
	// pointing at a directory nothing writes to any more.
	SetBackupFolder(ctx context.Context, id, ownerID, folder string) error
}

type postgresStore struct{ db *store.DB }

func NewPostgresStore(db *store.DB) Store { return postgresStore{db: db} }

func (p postgresStore) Connections(ctx context.Context) ([]Connection, error) {
	// Creation order is the adoption order: when an older release let two rows share
	// one name-keyed directory, the older row is the one that adopted it first, so a
	// repeat start hands the same directory to the same row.
	rows, err := p.db.Pool().Query(ctx, `select `+connectionColumns+` from storage_connections order by created_at, name`)
	if err != nil {
		return nil, fmt.Errorf("read storage connections: %w", err)
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p postgresStore) Authority(ctx context.Context) (Connection, bool, error) {
	return p.one(ctx, `select `+connectionColumns+` from storage_connections where role='authority' and enabled order by created_at limit 1`)
}

// ByName always resolves for one owner: that owner's own row wins over the shared
// default with the same name, which is what makes two accounts able to call their
// connection the same thing.
func (p postgresStore) ByName(ctx context.Context, ownerID, name string) (Connection, bool, error) {
	return p.one(ctx, `select `+connectionColumns+` from storage_connections
		where name=$2 and (owner_id=nullif($1,'')::uuid or owner_id is null)
		order by owner_id nulls last limit 1`, ownerID, name)
}

// Visible is what an account may see: its own rows plus the deployment default.
func (p postgresStore) Visible(ctx context.Context, ownerID string) ([]Connection, error) {
	rows, err := p.db.Pool().Query(ctx, `select `+connectionColumns+` from storage_connections
		where owner_id=nullif($1,'')::uuid or owner_id is null order by owner_id nulls last, name`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("read visible storage connections for owner %s: %w", ownerID, err)
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p postgresStore) one(ctx context.Context, query string, args ...any) (Connection, bool, error) {
	c, err := scanConnection(p.db.Pool().QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, false, nil
	}
	if err != nil {
		return Connection{}, false, err
	}
	return c, true, nil
}

// OwnerConnection reads the connection name an owner is already bound to. No row,
// or a binding written before connections existed, means "not bound yet": the
// resolver then falls back to the authority row, which is what a single-connection
// deployment has always done.
func (p postgresStore) OwnerConnection(ctx context.Context, ownerID string) (string, bool, error) {
	var binding string
	err := p.db.Pool().QueryRow(ctx, `select coalesce(root_binding->>'ConnectionID', '') from file_budgets where owner_id=$1::uuid`, ownerID).Scan(&binding)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read root binding for owner %s: %w", ownerID, err)
	}
	if strings.TrimSpace(binding) == "" {
		return "", false, nil
	}
	return bindingName(binding), true, nil
}

// ErrLocationTaken reports that another row already claims this physical location.
// One location holds one tree, so the second claim is a configuration conflict, not
// a retry.
var ErrLocationTaken = errors.New("storage connection location already claimed")

// Insert is idempotent for the same tree: re-inserting the row that already holds an
// identity does nothing, so a repeated bootstrap or a re-run of an operator command
// cannot produce two rows for one tree. A *different* row claiming the same identity
// is refused, because the rows would deliberately collide in one location — and
// because `on conflict do nothing` would otherwise report success for a row that was
// never written.
func (p postgresStore) Insert(ctx context.Context, c Connection) error {
	config, err := json.Marshal(c.Config)
	if err != nil {
		return fmt.Errorf("storage connection %q: encode config: %w", c.Name, err)
	}
	// A reference row stores no inline copy at all; writing "{}" rather than the
	// zero struct keeps that visible in a dump instead of looking like empty fields.
	secret := []byte("{}")
	if !c.Secret.Empty() {
		secret, err = json.Marshal(c.Secret)
		if err != nil {
			return fmt.Errorf("storage connection %q: encode secret: %w", c.Name, err)
		}
	}
	var reference any
	if !c.SecretReference.Empty() {
		encoded, err := json.Marshal(c.SecretReference)
		if err != nil {
			return fmt.Errorf("storage connection %q: encode environment reference: %w", c.Name, err)
		}
		reference = encoded
	}
	var admission any
	if len(c.Admission) > 0 {
		admission = []byte(c.Admission)
	}
	// The id is supplied by a caller that had to place on-disk state before the
	// INSERT, because a connection's evidence and spool are keyed by it; a caller
	// without one still gets the schema's gen_random_uuid().
	tag, err := p.db.Pool().Exec(ctx, `
		insert into storage_connections(id, owner_id, name, backend, role, identity, config, secret, secret_reference, generation, admission, enabled)
		values(coalesce(nullif($1,'')::uuid, gen_random_uuid()), nullif($2,'')::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,true)
		on conflict (identity) do nothing`,
		c.ID, c.OwnerID, c.Name, c.Backend, c.Role, c.Identity, config, secret, reference, c.Generation, admission)
	if err != nil {
		return fmt.Errorf("insert storage connection %q (identity %s): %w", c.Name, c.Identity, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var name, owner string
	if err := p.db.Pool().QueryRow(ctx, `select name, coalesce(owner_id::text,'') from storage_connections where identity=$1`, c.Identity).Scan(&name, &owner); err != nil {
		return fmt.Errorf("storage connection %q: read the row that already holds identity %s: %w", c.Name, c.Identity, err)
	}
	if name == c.Name && owner == c.OwnerID {
		return nil
	}
	holder := fmt.Sprintf("connection %q, shared by the deployment", name)
	if owner != "" {
		holder = fmt.Sprintf("connection %q of account %s", name, owner)
	}
	return fmt.Errorf("%w: %s already holds this location; a connection needs its own location, so remove that row first or name a different bucket/prefix", ErrLocationTaken, holder)
}

// SetSecretReference is the one write that removes a credential from the cache.
// The owner condition is not decorative: an account's own connection must never be
// repointed at the deployment's environment variables, because its key belongs to
// that account.
func (p postgresStore) SetSecretReference(ctx context.Context, id string, ref SecretReference) error {
	if ref.Empty() {
		return fmt.Errorf("storage connection %s: an empty environment reference cannot replace a secret", id)
	}
	encoded, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("storage connection %s: encode environment reference: %w", id, err)
	}
	tag, err := p.db.Pool().Exec(ctx, `
		update storage_connections
		set secret='{}'::jsonb, secret_reference=$2, updated_at=now()
		where id=$1::uuid and owner_id is null`, id, encoded)
	if err != nil {
		return fmt.Errorf("storage connection %s: write environment reference: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage connection %s: no shared row was updated; an account-added connection keeps its inline secret", id)
	}
	return nil
}

// SetObjectPaths records the id-keyed paths adoption moved a row's directories to.
// The two paths live in the row's config, so the driver, the probe command and the
// CLI all read one answer; leaving the row on a legacy path after moving the
// directory would make the next start adopt a directory that is no longer there.
func (p postgresStore) SetObjectPaths(ctx context.Context, id, admissionFile, spoolRoot string) error {
	if id == "" {
		return fmt.Errorf("storage connection path adoption: no row id to update")
	}
	tag, err := p.db.Pool().Exec(ctx, `
		update storage_connections
		set config = jsonb_set(jsonb_set(config, '{objectAdmissionFile}', to_jsonb($2::text)), '{objectSpoolRoot}', to_jsonb($3::text)),
		    updated_at=now()
		where id=$1::uuid`, id, admissionFile, spoolRoot)
	if err != nil {
		return fmt.Errorf("storage connection %s: write adopted object paths: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage connection %s: no row was updated with its adopted object paths", id)
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanConnection(row scanner) (Connection, error) {
	var (
		c                   Connection
		config, secret, ref []byte
		ad, probe           []byte
	)
	if err := row.Scan(&c.ID, &c.OwnerID, &c.Name, &c.Backend, &c.Role, &c.Identity, &config, &secret, &ref, &c.Generation, &ad, &c.Enabled, &probe); err != nil {
		return Connection{}, err
	}
	if err := json.Unmarshal(config, &c.Config); err != nil {
		return Connection{}, fmt.Errorf("storage connection %q: decode config: %w", c.Name, err)
	}
	if err := json.Unmarshal(secret, &c.Secret); err != nil {
		return Connection{}, fmt.Errorf("storage connection %q: decode secret: %w", c.Name, err)
	}
	if len(ref) > 0 {
		if err := json.Unmarshal(ref, &c.SecretReference); err != nil {
			return Connection{}, fmt.Errorf("storage connection %q: decode environment reference: %w", c.Name, err)
		}
	}
	c.Admission = ad
	c.LastProbe = probe
	return c, nil
}

// ErrConnectionNotFound reports that no row matched the id and account the caller
// asked about. It is separate from a folder that is merely locked, because one is a
// stale page and the other is a setting with a reason.
var ErrConnectionNotFound = errors.New("storage connection not found on this account")

// ErrBackupFolderLocked reports that the destination already holds snapshots, so
// moving its folder would orphan every listed restore point. Removing those dates
// first (or keeping the folder) is the only way through.
var ErrBackupFolderLocked = errors.New("this destination already holds backups")

func (p postgresStore) SetProbe(ctx context.Context, id string, probe []byte) error {
	if len(probe) == 0 {
		return fmt.Errorf("storage connection %s: an empty probe result cannot be recorded", id)
	}
	tag, err := p.db.Pool().Exec(ctx, `update storage_connections set last_probe=$2 where id=$1::uuid`, id, probe)
	if err != nil {
		return fmt.Errorf("storage connection %s: record probe result: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrConnectionNotFound, id)
	}
	return nil
}

// SetBackupFolder serializes against a backup start by taking the same row lock
// `recordBackupStart` takes. The lock is what makes "no run exists yet" a fact
// rather than a race: without it a run could read the old folder, the update could
// pass its no-runs check, and the snapshot would land somewhere the row no longer
// names.
func (p postgresStore) SetBackupFolder(ctx context.Context, id, ownerID, folder string) error {
	tx, err := p.db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner string
	err = tx.QueryRow(ctx, `select coalesce(owner_id::text,'') from storage_connections where id=$1::uuid for update`, id).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrConnectionNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("storage connection %s: read before changing the backup folder: %w", id, err)
	}
	// A separate statement, after the lock is held, so it reads a snapshot taken
	// after the run that held the lock committed. A subquery in the locking
	// statement keeps the snapshot from before the wait under READ COMMITTED.
	var runs int
	if err := tx.QueryRow(ctx, `select count(*) from backup_runs where connection_id=$1::uuid`, id).Scan(&runs); err != nil {
		return fmt.Errorf("storage connection %s: count existing runs: %w", id, err)
	}
	if owner != ownerID {
		return fmt.Errorf("%w: %s belongs to another account or to the deployment", ErrConnectionNotFound, id)
	}
	if runs > 0 {
		return fmt.Errorf("%w: %d run(s) already write under the current folder; keep it or remove those dates first", ErrBackupFolderLocked, runs)
	}
	if _, err := tx.Exec(ctx, `
		update storage_connections
		set config = jsonb_set(coalesce(config, '{}'::jsonb), '{backupFolder}', to_jsonb($2::text)),
		    updated_at = now()
		where id=$1::uuid`, id, folder); err != nil {
		return fmt.Errorf("storage connection %s: write backup folder: %w", id, err)
	}
	return tx.Commit(ctx)
}

const connectionColumns = `id::text, coalesce(owner_id::text, ''), name, backend, role, identity, config, secret, secret_reference, generation, admission, enabled, last_probe`
