package mediameta

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cache keeps parsed results so a file is read once per content state. contentKey names
// that state; a result stored under another key, or by another parser Version, is a miss.
//
// Implementations hold derived data only. Losing every entry costs one re-parse per file
// viewed afterwards and nothing else.
type Cache interface {
	Get(ctx context.Context, ownerID, entryID, contentKey string) (Info, bool, error)
	Put(ctx context.Context, ownerID, entryID, contentKey string, info Info) error
}

// PostgresCache stores results in file_media_metadata (sql/040).
type PostgresCache struct{ pool *pgxpool.Pool }

func NewPostgresCache(pool *pgxpool.Pool) *PostgresCache { return &PostgresCache{pool: pool} }

func (c *PostgresCache) Get(ctx context.Context, ownerID, entryID, contentKey string) (Info, bool, error) {
	var raw []byte
	err := c.pool.QueryRow(ctx, `select info from file_media_metadata
 where owner_id=$1::uuid and node_id=$2::uuid and content_key=$3 and parser_version=$4`,
		ownerID, entryID, contentKey, Version).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Info{}, false, nil
	}
	if err != nil {
		return Info{}, false, err
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil {
		// An unreadable row is a miss: the next Put replaces it.
		return Info{}, false, nil
	}
	return info, true, nil
}

func (c *PostgresCache) Put(ctx context.Context, ownerID, entryID, contentKey string, info Info) error {
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	_, err = c.pool.Exec(ctx, `insert into file_media_metadata(owner_id,node_id,content_key,parser_version,info)
 values($1::uuid,$2::uuid,$3,$4,$5::jsonb)
 on conflict(owner_id,node_id) do update
 set content_key=excluded.content_key, parser_version=excluded.parser_version, info=excluded.info, parsed_at=now()`,
		ownerID, entryID, contentKey, Version, raw)
	return err
}
