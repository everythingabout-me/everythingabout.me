package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var nativeMetadataPermits sync.Map // *pgxpool.Pool -> chan struct{}

// WithUploadMetadata holds session or part locks on one dedicated connection.
// A receiver uses at most one connection while reading/uploading a part; the
// receipt callback uses a separate short transaction. No DB transaction spans I/O.
func (p *Postgres) WithUploadMetadata(ctx context.Context, binding, id string, number int, fn func(storage.UploadMetadataRecord) error) error {
	if binding == "" || id == "" || number < -1 || number > 10000 || fn == nil {
		return E("invalid_request", nil)
	}
	// Every admitted holder may call back into a short receipt transaction. At
	// least half the pool remains available even across reconstructed journals.
	capacity := int(p.pool.Config().MaxConns) / 2
	if capacity < 1 {
		return E("invalid_config", nil)
	}
	value, _ := nativeMetadataPermits.LoadOrStore(p.pool, make(chan struct{}, capacity))
	permits := value.(chan struct{})
	select {
	case permits <- struct{}{}:
		defer func() { <-permits }()
	case <-ctx.Done():
		return ctx.Err()
	}
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	var keys []struct {
		key    string
		shared bool
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for i := len(keys) - 1; i >= 0; i-- {
			query := `select pg_advisory_unlock(hashtextextended($1,0))`
			if keys[i].shared {
				query = `select pg_advisory_unlock_shared(hashtextextended($1,0))`
			}
			if _, e := conn.Exec(clean, query, keys[i].key); e != nil {
				_ = conn.Conn().Close(clean)
				break
			}
		}
		conn.Release()
	}()
	take := func(key string, wait, shared bool) (bool, error) {
		for {
			var ok bool
			query := `select pg_try_advisory_lock(hashtextextended($1,0))`
			if shared {
				query = `select pg_try_advisory_lock_shared(hashtextextended($1,0))`
			}
			if e := conn.QueryRow(ctx, query, key).Scan(&ok); e != nil {
				return false, e
			}
			if ok {
				keys = append(keys, struct {
					key    string
					shared bool
				}{key, shared})
				return true, nil
			}
			if !wait {
				return false, nil
			}
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false, ctx.Err()
			case <-timer.C:
			}
		}
	}
	prefix := "native-upload:" + id + ":" + binding
	if _, err = take(prefix+":session", true, number != 0); err != nil {
		return err
	}
	if number > 0 {
		admitted := false
		for slot := 0; slot < 4; slot++ {
			ok, e := take(fmt.Sprintf("%s:slot:%d", prefix, slot), false, false)
			if e != nil {
				return e
			}
			if ok {
				admitted = true
				break
			}
		}
		if !admitted {
			return storage.NewError("upload_busy", "", nil)
		}
	}
	if _, err = take(fmt.Sprintf("%s:part:%d", prefix, number), true, false); err != nil {
		return err
	}
	var owner string
	if err = conn.QueryRow(ctx, `select owner_id::text from file_uploads where operation_id=$1::uuid`, id).Scan(&owner); err != nil {
		return err
	}
	return fn(&nativeMetadataRecord{conn: conn, id: id, owner: owner, binding: binding})
}

type nativeMetadataRecord struct {
	conn               *pgxpool.Conn
	id, owner, binding string
}

func (r *nativeMetadataRecord) Get(ctx context.Context, key string) ([]byte, error) {
	var value []byte
	err := r.conn.QueryRow(ctx, `select value from file_upload_native_metadata where operation_id=$1::uuid and owner_id=$2::uuid and binding=$3 and key=$4`, r.id, r.owner, r.binding, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return value, err
}
func (r *nativeMetadataRecord) Put(ctx context.Context, key string, value []byte) error {
	if key == "" || len(key) > 128 || !json.Valid(value) || len(value) > 65536 {
		return E("invalid_request", nil)
	}
	_, err := r.conn.Exec(ctx, `insert into file_upload_native_metadata(operation_id,owner_id,binding,key,value) values($1::uuid,$2::uuid,$3,$4,$5::jsonb) on conflict(operation_id,binding,key) do update set value=excluded.value where file_upload_native_metadata.owner_id=excluded.owner_id`, r.id, r.owner, r.binding, key, string(value))
	return err
}
