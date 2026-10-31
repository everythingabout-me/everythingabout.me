package uploadindex

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresJournal retains observations indefinitely. Expiry only changes polling
// frequency; it never establishes provider quiescence or authorizes data removal.
type PostgresJournal struct {
	pool *pgxpool.Pool
}

func NewJournal(pool *pgxpool.Pool) *PostgresJournal {
	return &PostgresJournal{pool: pool}
}

func (j *PostgresJournal) Register(ctx context.Context, key Key, issuedUntil, now time.Time) error {
	_, err := j.pool.Exec(ctx, `
insert into upload_index_watches
  (binding,owner_id,path,is_dir,issued_until,next_check_at)
values ($1,$2::uuid,$3,$4,$5,$6)
on conflict (binding,owner_id,path,is_dir) do update set
  issued_until=greatest(upload_index_watches.issued_until,excluded.issued_until),
  next_check_at=excluded.next_check_at, settled=false,
  generation=upload_index_watches.generation+1,
  lease_until=null, retry_count=0, last_error='', updated_at=$6`,
		key.Binding, key.OwnerID, key.Path, key.IsDir, issuedUntil, now)
	return err
}

func (j *PostgresJournal) Settle(ctx context.Context, key Key, now, nextCheck time.Time) error {
	_, err := j.pool.Exec(ctx, `
update upload_index_watches set
  settled=true,next_check_at=$5,generation=generation+1,
  lease_until=null,retry_count=0,last_error='',updated_at=$6
where binding=$1 and owner_id=$2::uuid and path=$3 and is_dir=$4`,
		key.Binding, key.OwnerID, key.Path, key.IsDir, nextCheck, now)
	return err
}

func (j *PostgresJournal) Claim(ctx context.Context, binding string, now, leaseUntil time.Time, limit int) ([]Watch, error) {
	rows, err := j.pool.Query(ctx, `
with due as (
  select binding,owner_id,path,is_dir from upload_index_watches
  where binding=$1 and next_check_at <= $2
    and (lease_until is null or lease_until <= $2)
  order by next_check_at,owner_id,path,is_dir
  limit $4 for update skip locked
)
update upload_index_watches w set
  lease_until=$3,lease_version=w.lease_version+1
from due d
where w.binding=d.binding and w.owner_id=d.owner_id
  and w.path=d.path and w.is_dir=d.is_dir
returning w.binding,w.owner_id::text,w.path,w.is_dir,
  w.issued_until,w.settled,w.generation,w.lease_version`, binding, now, leaseUntil, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var watches []Watch
	for rows.Next() {
		var watch Watch
		if err := rows.Scan(&watch.Binding, &watch.OwnerID, &watch.Path, &watch.IsDir,
			&watch.IssuedUntil, &watch.Settled, &watch.Generation, &watch.LeaseVersion); err != nil {
			return nil, err
		}
		watches = append(watches, watch)
	}
	return watches, rows.Err()
}

func (j *PostgresJournal) Finish(ctx context.Context, watch Watch, result Result) error {
	_, err := j.pool.Exec(ctx, `
update upload_index_watches set
  next_check_at=$7,checked_at=$8,last_error=$10,
  last_fingerprint=case when $10='' then $9 else last_fingerprint end,
  last_success_at=case when $10='' then $8 else last_success_at end,
  retry_count=case when $10='' then 0 else least(retry_count+1,1000000) end,
  lease_until=null,updated_at=$8
where binding=$1 and owner_id=$2::uuid and path=$3 and is_dir=$4
  and generation=$5 and lease_version=$6`,
		watch.Binding, watch.OwnerID, watch.Path, watch.IsDir, watch.Generation, watch.LeaseVersion,
		result.NextCheck, result.CheckedAt, result.Fingerprint, result.Error)
	return err
}

var _ Journal = (*PostgresJournal)(nil)
