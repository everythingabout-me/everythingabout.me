package fileops

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/storage"
)

// expireLoadedUpload is called only after authorization under the owner lock.
// Status reads can retire an elapsed open session without waiting for the sweeper;
// all physical liability remains until actual temporary-byte cleanup.
func expireLoadedUpload(ctx context.Context, tx pgx.Tx, u *Upload) error {
	if u.State != UploadOpen {
		return nil
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return err
	}
	if u.ExpiresAt.After(now) {
		return nil
	}
	return terminateOpenUpload(ctx, tx, u, UploadExpired, now)
}

// FreezeUpload freezes a session whose part coverage is proven by durable application
// receipts. It is the class C transition: every part passed through this application.
func (p *Postgres) FreezeUpload(ctx context.Context, root Root, actor Actor, id string) (Upload, error) {
	return p.freezeUpload(ctx, root, actor, id, false)
}

// FreezeProviderListed freezes a class A session, whose parts never pass through this
// application and therefore have no receipts. Coverage was read from the provider's own
// part listing by the caller; sealing re-lists that state before the single publication,
// so this transition is never the only evidence a part exists.
func (p *Postgres) FreezeProviderListed(ctx context.Context, root Root, actor Actor, id string) (Upload, error) {
	return p.freezeUpload(ctx, root, actor, id, true)
}

func (p *Postgres) freezeUpload(ctx context.Context, root Root, actor Actor, id string, providerListed bool) (Upload, error) {
	var u Upload
	if err := validateRoot(root); err != nil {
		return u, err
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var err error
		u, err = readUpload(ctx, tx, root, actor, id, true)
		if err != nil {
			return err
		}
		switch u.State {
		case UploadCommitting, UploadUnknown, UploadCommitted:
			return nil
		case UploadFailed, UploadAborted, UploadExpired:
			return E("upload_gone", nil)
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if err = uploadWritable(u, now); err != nil {
			return err
		}
		w, err := uploadIntent(u)
		if err != nil {
			return err
		}
		if !providerListed {
			var count, bad int64
			var size int64
			err = tx.QueryRow(ctx, `select count(*),coalesce(sum(size),0),count(*) filter(where number<1 or number>$3 or size<>case when number=$3 then $4::bigint-($3::bigint-1)*$5::bigint else $5::bigint end) from file_upload_parts where operation_id=$1 and owner_id=$2`, id, root.OwnerID, u.PartCount, w.Size, u.PartSize).Scan(&count, &size, &bad)
			if err != nil {
				return err
			}
			if count != int64(u.PartCount) || size != w.Size || bad != 0 || u.ReceivedParts != u.PartCount || u.ReceivedBytes != w.Size {
				return E("upload_incomplete", nil)
			}
		}
		// Claim is fenced until this state change commits. No receipt can be inserted
		// concurrently because receipt, abort, expiry and freeze share the owner lock.
		_, err = tx.Exec(ctx, `update file_uploads set state='committing' where operation_id=$1 and owner_id=$2`, id, root.OwnerID)
		if err != nil {
			return err
		}
		u.State = UploadCommitting
		return nil
	})
	return u, err
}

// terminateOpenUpload proves there has never been a native attempt: Claim cannot
// lease an open session. Logical quota/guards can be released immediately, while
// all physical reservation survives until actual private spool/native cleanup.
func terminateOpenUpload(ctx context.Context, tx pgx.Tx, u *Upload, state UploadState, now time.Time) error {
	op := &u.Operation
	if u.State != UploadOpen || op.Terminal() || op.Lease.Token != "" || op.Stage != nil || op.Phase != Admitted || op.NativeResult != nil {
		return E("recovery_required", nil)
	}
	var logical int64
	if err := tx.QueryRow(ctx, `select logical_reserved from file_reservations where operation_id=$1 and owner_id=$2 for update`, op.ID, op.Root.OwnerID).Scan(&logical); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update file_budgets set logical_reserved=logical_reserved-$2 where owner_id=$1`, op.Root.OwnerID, logical); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update file_reservations set logical_reserved=0,settled=true where operation_id=$1 and owner_id=$2`, op.ID, op.Root.OwnerID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from file_credit_claims where operation_id=$1 and owner_id=$2`, op.ID, op.Root.OwnerID); err != nil {
		return err
	}
	if err := releaseGuards(ctx, tx, op); err != nil {
		return err
	}
	op.CancelRequested = true
	finish(op, Cancelled, now)
	if err := saveOperation(ctx, tx, *op); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update file_uploads set state=$3 where operation_id=$1 and owner_id=$2`, op.ID, op.Root.OwnerID, string(state)); err != nil {
		return err
	}
	u.State = state
	return nil
}

func (p *Postgres) AbortUpload(ctx context.Context, root Root, actor Actor, id string) (Upload, error) {
	var u Upload
	if err := validateRoot(root); err != nil {
		return u, err
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var err error
		u, err = readUpload(ctx, tx, root, actor, id, true)
		if err != nil {
			return err
		}
		if u.State == UploadAborted {
			return nil
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if u.State != UploadOpen {
			return uploadWritable(u, now)
		}
		if !u.ExpiresAt.After(now) {
			return E("upload_expired", nil)
		}
		return terminateOpenUpload(ctx, tx, &u, UploadAborted, now)
	})
	return u, err
}

// ExpireUploads scans a bounded batch without locking session rows first. Each
// candidate is rechecked under the owner lock before cancellation, preventing a
// receipt/commit from racing with the expiry decision.
func (p *Postgres) ExpireUploads(ctx context.Context, limit int) ([]Upload, error) {
	if limit < 1 || limit > 1000 {
		return nil, E("invalid_request", nil)
	}
	rows, err := p.pool.Query(ctx, `select operation_id::text,owner_id::text from file_uploads where state='open' and expires_at<=clock_timestamp() order by expires_at,operation_id limit $1`, limit)
	if err != nil {
		return nil, err
	}
	type candidate struct{ id, owner string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.owner); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []Upload
	for _, c := range candidates {
		err = p.transaction(ctx, c.owner, func(tx pgx.Tx) error {
			op, err := readOperation(ctx, tx, c.id)
			if err != nil {
				return err
			}
			actor := Actor{ID: op.PrincipalID, CanWrite: true}
			u, err := readUpload(ctx, tx, op.Root, actor, c.id, true)
			if err != nil {
				return err
			}
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			if u.State != UploadOpen || u.ExpiresAt.After(now) {
				return nil
			}
			if err = terminateOpenUpload(ctx, tx, &u, UploadExpired, now); err != nil {
				return err
			}
			out = append(out, u)
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// CancelOpenUploadsWithin aborts the upload sessions that never committed and still hold a
// target at or inside target, and returns how many it settled.
//
// It exists for the recursive delete. That confirmation already covers every path in the
// subtree, so a session inside it can never publish anything the user keeps; until it is
// settled it fences the deletion with operation_in_progress for the rest of a session
// lifetime that can be 24 hours, and holds quota for bytes nobody sent. The alternative is
// a client cancelling them one 409 at a time, which a folder above more than a few dozen
// abandoned sessions can never finish — 188 of them from one interrupted import made a
// folder undeletable for the day.
//
// Only `open` sessions are settled. A commit that already started may have a native effect
// behind it, so it is left to finish or fail on its own and the caller reports it by name.
// An open session has published nothing at its target — staged parts, if any, are private
// staging — and the ordinary cleanup scan discards them once the operation is terminal.
func (p *Postgres) CancelOpenUploadsWithin(ctx context.Context, root Root, target storage.Path) (int, error) {
	if err := validateRoot(root); err != nil {
		return 0, err
	}
	base := comparePath(root, target)
	rows, err := p.pool.Query(ctx, `select u.operation_id::text from file_uploads u
		join file_operation_guards g on g.operation_id=u.operation_id and g.owner_id=u.owner_id
		where u.owner_id=$1::uuid and u.state='open'
		  and (g.comparison_path=$2 or left(g.comparison_path,length($2)+1)=$2||'/')
		order by u.created_at`, root.OwnerID, base)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	cancelled := 0
	for _, id := range ids {
		var settled bool
		err = p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
			op, err := readOperation(ctx, tx, id)
			if err != nil {
				return err
			}
			// The delete's authority over the subtree is the authority over these targets,
			// so the session is settled as its own principal would settle it.
			actor := Actor{ID: op.PrincipalID, CanWrite: true}
			u, err := readUpload(ctx, tx, op.Root, actor, id, true)
			if err != nil {
				return err
			}
			if u.State != UploadOpen {
				return nil
			}
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			if err = terminateOpenUpload(ctx, tx, &u, UploadAborted, now); err != nil {
				// A leased session is not provably native-free; leave it to recovery and let
				// the delete answer with its name instead of guessing.
				if IsCode(err, "recovery_required") {
					return nil
				}
				return err
			}
			settled = true
			return nil
		})
		if err != nil {
			return cancelled, err
		}
		if settled {
			cancelled++
		}
	}
	return cancelled, nil
}

// MarkUploadChunksCleaned records a successful spool discard. It releases no
// budget: MarkCleaned remains the proof that every staged/native allocation was
// discarded and is the sole physical-reservation cleanup transition.
func (p *Postgres) MarkUploadChunksCleaned(ctx context.Context, root Root, actor Actor, id string) error {
	if err := validateRoot(root); err != nil {
		return err
	}
	return p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		u, err := readUpload(ctx, tx, root, actor, id, true)
		if err != nil {
			return err
		}
		if !u.Operation.Terminal() {
			return E("invalid_transition", nil)
		}
		_, err = tx.Exec(ctx, `update file_uploads set chunks_cleaned_at=coalesce(chunks_cleaned_at,clock_timestamp()) where operation_id=$1 and owner_id=$2`, id, root.OwnerID)
		return err
	})
}
