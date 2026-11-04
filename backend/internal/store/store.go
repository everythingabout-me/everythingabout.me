// Package store manages the Postgres connection pool.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	// A part receiver holds one connection for the whole native part transfer
	// (seconds per 32 MiB part), and fileops admits up to UploadPartSlots of them
	// per upload while keeping half the pool free for receipt transactions and
	// ordinary requests. Sizing the pool below twice that admission would throttle
	// receivers silently, below the parallelism their sessions advertise.
	cfg.MaxConns = 40
	cfg.MaxConnIdleTime = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool}, nil
}

func (db *DB) Pool() *pgxpool.Pool { return db.pool }

func (db *DB) Close() { db.pool.Close() }

// IsUniqueViolation reports a Postgres unique-constraint conflict (a duplicate email, say).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
