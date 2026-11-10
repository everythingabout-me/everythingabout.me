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
}

type postgresStore struct{ db *store.DB }

func NewPostgresStore(db *store.DB) Store { return postgresStore{db: db} }

func (p postgresStore) Connections(ctx context.Context) ([]Connection, error) {
	rows, err := p.db.Pool().Query(ctx, `select `+connectionColumns+` from storage_connections order by name`)
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

// Insert is idempotent for the same tree: a second INSERT of an identity that a row
// already claims does nothing, so a repeated bootstrap or a re-run of an operator
// command cannot produce two rows for one tree.
func (p postgresStore) Insert(ctx context.Context, c Connection) error {
	config, err := json.Marshal(c.Config)
	if err != nil {
		return fmt.Errorf("storage connection %q: encode config: %w", c.Name, err)
	}
	secret, err := json.Marshal(c.Secret)
	if err != nil {
		return fmt.Errorf("storage connection %q: encode secret: %w", c.Name, err)
	}
	var admission any
	if len(c.Admission) > 0 {
		admission = []byte(c.Admission)
	}
	_, err = p.db.Pool().Exec(ctx, `
		insert into storage_connections(owner_id, name, backend, role, identity, config, secret, generation, admission, enabled)
		values(nullif($1,'')::uuid,$2,$3,$4,$5,$6,$7,$8,$9,true)
		on conflict (identity) do nothing`,
		c.OwnerID, c.Name, c.Backend, c.Role, c.Identity, config, secret, c.Generation, admission)
	if err != nil {
		return fmt.Errorf("insert storage connection %q (identity %s): %w", c.Name, c.Identity, err)
	}
	return nil
}

const connectionColumns = `id::text, coalesce(owner_id::text, ''), name, backend, role, identity, config, secret, generation, admission, enabled`

type scanner interface{ Scan(...any) error }

func scanConnection(row scanner) (Connection, error) {
	var (
		c                  Connection
		config, secret, ad []byte
	)
	if err := row.Scan(&c.ID, &c.OwnerID, &c.Name, &c.Backend, &c.Role, &c.Identity, &config, &secret, &c.Generation, &ad, &c.Enabled); err != nil {
		return Connection{}, err
	}
	if err := json.Unmarshal(config, &c.Config); err != nil {
		return Connection{}, fmt.Errorf("storage connection %q: decode config: %w", c.Name, err)
	}
	if err := json.Unmarshal(secret, &c.Secret); err != nil {
		return Connection{}, fmt.Errorf("storage connection %q: decode secret: %w", c.Name, err)
	}
	c.Admission = ad
	return c, nil
}
