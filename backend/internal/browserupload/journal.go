package browserupload

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresJournal struct{ pool *pgxpool.Pool }

func NewJournal(pool *pgxpool.Pool) *PostgresJournal { return &PostgresJournal{pool: pool} }

const columns = `id,binding,owner_id::text,path,size,mime,modified_at,state,checkpoint,result,expires_at,coalesce(lease_token,'')`

func scan(row pgx.Row) (Session, error) {
	var value Session
	err := row.Scan(&value.ID, &value.Binding, &value.OwnerID, &value.Path, &value.Size, &value.Mime, &value.ModifiedAt, &value.State, &value.Checkpoint, &value.Result, &value.ExpiresAt, &value.LeaseToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, fail(404, "upload receipt not found")
	}
	return value, err
}
func (j *PostgresJournal) Create(ctx context.Context, s Session) error {
	_, err := j.pool.Exec(ctx, `insert into browser_uploads(id,binding,owner_id,path,size,mime,modified_at,state,checkpoint,expires_at,next_check_at)
 values($1,$2,$3::uuid,$4,$5,$6,$7,'open','{}'::jsonb,$8,$8)`, s.ID, s.Binding, s.OwnerID, s.Path, s.Size, s.Mime, s.ModifiedAt, s.ExpiresAt)
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) && postgres.Code == "23505" {
		return fail(409, "another upload to this path is unfinished")
	}
	return err
}
func (j *PostgresJournal) Get(ctx context.Context, binding, owner, id string) (Session, error) {
	return scan(j.pool.QueryRow(ctx, `select `+columns+` from browser_uploads where binding=$1 and owner_id=$2::uuid and id=$3`, binding, owner, id))
}
func (j *PostgresJournal) Claim(ctx context.Context, binding, owner, id string, ttl time.Duration) (Session, error) {
	token, err := newID()
	if err != nil {
		return Session{}, err
	}
	s, err := scan(j.pool.QueryRow(ctx, `update browser_uploads set lease_token=$4,lease_until=clock_timestamp()+$5::bigint*interval '1 millisecond',updated_at=clock_timestamp()
 where binding=$1 and owner_id=$2::uuid and id=$3 and state not in ('completed','aborted')
 and (lease_until is null or lease_until<=clock_timestamp()) returning `+columns, binding, owner, id, token, ttl.Milliseconds()))
	if typed, ok := err.(*Error); ok && typed.Status == 404 {
		existing, getErr := j.Get(ctx, binding, owner, id)
		if getErr != nil {
			return existing, getErr
		}
		return existing, fail(409, "upload is in progress; retry confirmation")
	}
	return s, err
}
func (j *PostgresJournal) Renew(ctx context.Context, s Session, ttl time.Duration) error {
	tag, err := j.pool.Exec(ctx, `update browser_uploads set lease_until=case when $5::bigint=0 then null else clock_timestamp()+$5::bigint*interval '1 millisecond' end,
 lease_token=case when $5::bigint=0 then null else lease_token end
 where binding=$1 and owner_id=$2::uuid and id=$3 and lease_token=$4 and lease_until>clock_timestamp()`, s.Binding, s.OwnerID, s.ID, s.LeaseToken, ttl.Milliseconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fail(409, "upload lease changed; retry confirmation")
	}
	return nil
}
func (j *PostgresJournal) Save(ctx context.Context, s Session, next time.Time) error {
	if !validJSON(s.Checkpoint) {
		return fail(500, "invalid upload checkpoint")
	}
	var result any
	if len(s.Result) > 0 {
		if !json.Valid(s.Result) {
			return fail(500, "invalid upload result")
		}
		result = string(s.Result)
	}
	tag, err := j.pool.Exec(ctx, `update browser_uploads set state=$5,checkpoint=$6::jsonb,result=$7::jsonb,next_check_at=$8,
 updated_at=clock_timestamp(),retain_until=case when $5 in ('completed','aborted') then clock_timestamp()+interval '24 hours' else null end
 where binding=$1 and owner_id=$2::uuid and id=$3 and lease_token=$4 and lease_until>clock_timestamp()
 and state not in ('completed','aborted')`, s.Binding, s.OwnerID, s.ID, s.LeaseToken, s.State, string(s.Checkpoint), result, next)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fail(409, "upload lease changed; retry confirmation")
	}
	return nil
}
func (j *PostgresJournal) Due(ctx context.Context, binding string, limit int) ([]Session, error) {
	if limit < 1 || limit > 128 {
		return nil, fail(500, "invalid recovery batch size")
	}
	rows, err := j.pool.Query(ctx, `select `+columns+` from browser_uploads where binding=$1 and state not in ('completed','aborted')
 and next_check_at<=clock_timestamp() and (lease_until is null or lease_until<=clock_timestamp()) order by next_check_at,id limit $2`, binding, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}
func (j *PostgresJournal) Cleanup(ctx context.Context, binding string, limit int) error {
	if limit < 1 || limit > 1024 {
		return fail(500, "invalid receipt cleanup batch size")
	}
	_, err := j.pool.Exec(ctx, `delete from browser_uploads where id in (select id from browser_uploads where binding=$1
 and state in ('completed','aborted') and retain_until<=clock_timestamp() order by retain_until limit $2)`, binding, limit)
	return err
}
func (j *PostgresJournal) CheckConflict(ctx context.Context, binding, owner, path string, subtree bool) error {
	var exists bool
	// Segment-boundary matching avoids SQL LIKE wildcard interpretation in names.
	err := j.pool.QueryRow(ctx, `select exists(select 1 from browser_uploads where binding=$1 and owner_id=$2::uuid
 and state not in ('completed','aborted') and (path=$3 or ($4 and left(path,length($3)+1)=$3||'/')
 or left($3,length(path)+1)=path||'/'))`, binding, owner, path, subtree).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return fail(409, "an upload affecting this path is unfinished")
	}
	return nil
}

var _ Journal = (*PostgresJournal)(nil)
