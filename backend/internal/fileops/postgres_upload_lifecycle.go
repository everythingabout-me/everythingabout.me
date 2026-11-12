package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
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
	return terminateOpenUpload(ctx, tx, u, UploadExpired, now, nil)
}

// UploadBinding verifies, inside the freeze transaction, that the index still holds what a
// session bound when it was opened: the destination folder under the same identity at the
// same path, and for a replacement the same file at the target. It reports a session that
// can never commit as parent_missing or target_changed, with the disagreement as its cause.
// It lives outside this package because the index does; nil skips the check for callers
// that have no index.
type UploadBinding func(context.Context, pgx.Tx, Operation) error

// FreezeUpload freezes a session whose part coverage is proven by durable application
// receipts. It is the class C transition: every part passed through this application.
func (p *Postgres) FreezeUpload(ctx context.Context, root Root, actor Actor, id string, bind UploadBinding) (Upload, error) {
	return p.freezeUpload(ctx, root, actor, id, false, false, bind)
}

// FreezeDirect freezes a session whose bytes the client already PUT at the destination.
// Nothing was staged as parts and no provider listing can prove coverage, so the freeze
// takes the guard and checks the destination binding only; the commit's own observation
// of the object is what proves the bytes.
func (p *Postgres) FreezeDirect(ctx context.Context, root Root, actor Actor, id string, bind UploadBinding) (Upload, error) {
	return p.freezeUpload(ctx, root, actor, id, false, true, bind)
}

// FreezeProviderListed freezes a class A session, whose parts never pass through this
// application and therefore have no receipts. Coverage was read from the provider's own
// part listing by the caller; sealing re-lists that state before the single publication,
// so this transition is never the only evidence a part exists.
func (p *Postgres) FreezeProviderListed(ctx context.Context, root Root, actor Actor, id string, bind UploadBinding) (Upload, error) {
	return p.freezeUpload(ctx, root, actor, id, true, false, bind)
}

// freezeUpload turns an open session into a commit, and is where the session takes its
// guard.
//
// A class A or C session can change the native tree in exactly one place: the publication
// its commit performs. Before that it is staged parts and a reservation, so a guard held
// from creation fenced a path for the hours a session may stay open in order to protect a
// window of seconds — and an abandoned session kept fencing it: a folder above 188 of them
// could not be deleted, a folder holding one could not be renamed, and a file with a pending
// replacement could not be read by ID. Taking the guard here makes the fence cover the
// window in which something can actually go wrong, and nothing else.
//
// What the create-time guard also did was keep the destination from changing under the
// session. That now has to be re-established at the moment the fence goes up, in the same
// transaction and under the owner lock that every index settlement takes:
//
//   - a conflicting guard (a move, copy, delete or folder creation still in flight over the
//     path) refuses the commit with operation_in_progress and leaves the session open: the
//     holder may yet fail and leave everything where it was, so this answer is retryable;
//   - a binding the index no longer honours (the folder was deleted, moved or re-created;
//     the file being replaced is gone) fails the session for good, in this transaction. It
//     can never pass later, so leaving it open would hold its quota until the idle expiry
//     and hand the client the same refusal on every retry. Without this check a commit
//     racing a recursive delete publishes under a prefix the index already removed: the
//     purge adopts an object written after the acceptance, and the next rebuild from the
//     tree resurrects a folder the user deleted.
//
// A class whose client can produce a native effect before commit (a cloud-drive session
// that completes on its last chunk) needs its fence earlier than this; see
// docs/design/transfer-protocol.md §5.
func (p *Postgres) freezeUpload(ctx context.Context, root Root, actor Actor, id string, providerListed, direct bool, bind UploadBinding) (Upload, error) {
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
		if !providerListed && !direct {
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
		holder, err := guardHolder(ctx, tx, root, u.Operation.Kind, u.Operation.Guards, id)
		if err != nil {
			return err
		}
		if holder != nil {
			return holder
		}
		if bind != nil {
			if err = bind(ctx, tx, u.Operation); err != nil {
				var broken *Error
				if !errors.As(err, &broken) || broken.Code != "parent_missing" && broken.Code != "target_changed" {
					return err
				}
				detail := "The upload cannot be committed to the destination it was opened for"
				if broken.Cause != nil {
					detail += ": " + broken.Cause.Error()
				}
				// Returning nil commits the termination; the caller reads the failure from
				// the terminal operation, the same answer every later status read gives.
				return terminateOpenUpload(ctx, tx, &u, UploadFailed, now, uploadProblem(broken.Code, detail+". Start a new upload."))
			}
		}
		if err = insertGuards(ctx, tx, u.Operation); err != nil {
			return err
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
//
// UploadFailed is the commit that was refused before any native effect: the session row
// becomes `committing` over a failed operation, which is how every failed commit is
// stored. The other states cancel the operation. problem, when given, is the stored reason.
func terminateOpenUpload(ctx context.Context, tx pgx.Tx, u *Upload, state UploadState, now time.Time, problem json.RawMessage) error {
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
	// An open session holds no guard; this only removes the row of a session opened while
	// guards were still taken at creation.
	if err := releaseGuards(ctx, tx, op); err != nil {
		return err
	}
	stored, finished := string(state), Cancelled
	if state == UploadFailed {
		stored, finished = string(UploadCommitting), Failed
	} else {
		op.CancelRequested = true
	}
	if len(problem) > 0 {
		op.Problem = problem
	}
	finish(op, finished, now)
	if err := saveOperation(ctx, tx, *op); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update file_uploads set state=$3 where operation_id=$1 and owner_id=$2`, op.ID, op.Root.OwnerID, stored); err != nil {
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
		return terminateOpenUpload(ctx, tx, &u, UploadAborted, now, nil)
	})
	return u, err
}

// ExpireUploads scans a bounded batch without locking session rows first. Each
// candidate is rechecked under the owner lock before cancellation, preventing a
// receipt/commit from racing with the expiry decision.
func (p *Postgres) ExpireUploads(ctx context.Context, limit int) ([]Upload, error) {
	if err := scanLimit("upload expiry scan", limit, 1000); err != nil {
		return nil, err
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
			if err = terminateOpenUpload(ctx, tx, &u, UploadExpired, now, nil); err != nil {
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
