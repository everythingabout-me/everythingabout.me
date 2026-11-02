package fileops

import (
	"context"
	"encoding/json"
)

// Pending returns bounded durable work, never permission to repeat a native
// final effect. Callers must select the exact root and run recovery/authorization.
func (p *Postgres) Pending(ctx context.Context, limit int) ([]Operation, error) {
	if limit < 1 || limit > 1000 {
		return nil, E("invalid_request", nil)
	}
	rows, err := p.pool.Query(ctx, `select snapshot from file_operations where snapshot->>'State' in ('queued','running','outcome_unknown')
 and (snapshot->>'CreatedAt')::timestamptz < clock_timestamp()-interval '30 seconds'
 and (coalesce(snapshot->'Lease'->>'Token','')='' or (snapshot->'Lease'->>'ExpiresAt')::timestamptz <= clock_timestamp())
 order by snapshot->>'CreatedAt',id limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		var b []byte
		var op Operation
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &op); err != nil {
			return nil, err
		}
		if err = canonicalOperation(&op); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}
