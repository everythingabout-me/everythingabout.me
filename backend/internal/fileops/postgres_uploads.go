package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

var _ UploadJournal = (*Postgres)(nil)

// readUpload reads a session for a transition. The caller already holds the owner lock,
// which orders every transition of this owner, and both rows are locked: operation first,
// then session, the order every transition uses (see terminateOpenUpload and freezeUpload).
// It is the only session read that locks, so it is the only one a write path may call.
func readUpload(ctx context.Context, tx pgx.Tx, root Root, actor Actor, id string, write bool) (Upload, error) {
	var u Upload
	if err := validateRoot(root); err != nil {
		return u, err
	}
	if !validID(id) {
		return u, E("not_found", nil)
	}
	op, err := readOperation(ctx, tx, id)
	if err != nil {
		return u, err
	}
	if err = authorizeOperation(op, root, actor, write); err != nil {
		return u, err
	}
	u.ID, u.Operation = id, op
	if err = scanUpload(tx.QueryRow(ctx, `select part_size,part_count,mode,state,received_parts,received_bytes,created_at,expires_at,chunks_cleaned_at from file_uploads where operation_id=$1 and owner_id=$2 for update`, id, root.OwnerID), &u); err != nil {
		return Upload{}, err
	}
	u.State = uploadState(u)
	return u, nil
}

// readUploadSnapshot reads one session for a listing: no `for update`, no owner lock, and
// no write. Both are load-bearing, not a shortcut.
//
// A listing takes no owner lock because it has nothing to order: the pages of a session are
// read while that session settles, so an owner-locked listing waited for the next settlement
// (and every settlement waited for the listings) — the queue's poller and commit response
// produced exactly that traffic. `p.readOnly` also sets the transaction to READ ONLY, so an
// edit that reintroduces a lock or a write here fails at query time (SQLSTATE 25006) instead
// of quietly widening what a read may do.
//
// An elapsed open session is reported as expired by deriving the state (uploadStateAt)
// rather than by terminating it here. Terminating is a transition: it cancels the operation
// and releases the logical reservation, which this transaction must not do. The row stays
// `open` for ExpireUploads (driven by Service.Sweep), which makes the same decision under the
// owner lock; until then the derived state is the answer a status read gives, and the
// listing must not depend on the sweeper having run.
func readUploadSnapshot(ctx context.Context, tx pgx.Tx, root Root, actor Actor, id string) (Upload, error) {
	var u Upload
	if err := validateRoot(root); err != nil {
		return u, err
	}
	if !validID(id) {
		return u, E("not_found", nil)
	}
	op, err := readOperationSnapshot(ctx, tx, id)
	if err != nil {
		return u, err
	}
	if err = authorizeOperation(op, root, actor, false); err != nil {
		return u, err
	}
	u.ID, u.Operation = id, op
	if err = scanUpload(tx.QueryRow(ctx, `select part_size,part_count,mode,state,received_parts,received_bytes,created_at,expires_at,chunks_cleaned_at from file_uploads where operation_id=$1 and owner_id=$2`, id, root.OwnerID), &u); err != nil {
		return Upload{}, err
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return Upload{}, err
	}
	u.State = uploadStateAt(u, now)
	return u, nil
}

func scanUpload(row pgx.Row, u *Upload) error {
	err := row.Scan(&u.PartSize, &u.PartCount, &u.Mode, &u.State, &u.ReceivedParts, &u.ReceivedBytes, &u.CreatedAt, &u.ExpiresAt, &u.ChunksCleanedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return E("not_found", nil)
	}
	return err
}

func replayUploadTx(ctx context.Context, tx pgx.Tx, root Root, actor Actor, key, fingerprint string) (Upload, bool, error) {
	if key == "" {
		return Upload{}, false, nil
	}
	var id, original string
	err := tx.QueryRow(ctx, `select operation_id::text,client_fingerprint from file_uploads where owner_id=$1 and principal_id=$2 and idempotency_key=$3`, root.OwnerID, actor.ID, key).Scan(&id, &original)
	if errors.Is(err, pgx.ErrNoRows) {
		return Upload{}, false, nil
	}
	if err != nil {
		return Upload{}, false, err
	}
	u, err := readUpload(ctx, tx, root, actor, id, true)
	if err != nil {
		return Upload{}, false, err
	}
	if original != fingerprint {
		return Upload{}, false, E("idempotency_conflict", nil)
	}
	if err = expireLoadedUpload(ctx, tx, &u); err != nil {
		return Upload{}, false, err
	}
	return u, true, nil
}

func (p *Postgres) ReplayUpload(ctx context.Context, root Root, actor Actor, key string, clientIntent storage.WriteIntent) (Upload, bool, error) {
	var u Upload
	var found bool
	if err := validateRoot(root); err != nil {
		return u, false, err
	}
	if actor.ID == "" || !actor.CanWrite || storage.ValidatePath(actor.Scope) != nil {
		return u, false, E("forbidden", nil)
	}
	if len(key) > 1024 || strings.ContainsRune(key, 0) {
		return u, false, E("invalid_request", nil)
	}
	fingerprint, err := clientUploadFingerprint(clientIntent)
	if err != nil {
		return u, false, err
	}
	err = p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var err error
		u, found, err = replayUploadTx(ctx, tx, root, actor, key, fingerprint)
		return err
	})
	return u, found, err
}

func (p *Postgres) CreateUpload(ctx context.Context, input BeginRequest, options UploadOptions) (Upload, bool, error) {
	var u Upload
	var replay bool
	fingerprint, err := clientUploadFingerprint(options.ClientIntent)
	if err != nil {
		return u, false, err
	}
	var w storage.WriteIntent
	if json.Unmarshal(input.Intent, &w) != nil || input.Kind != "upload_commit" || len(input.Manifest) != 0 {
		return u, false, E("invalid_request", nil)
	}
	actual, err := clientUploadFingerprint(w)
	if err != nil || actual != fingerprint {
		return u, false, E("intent_conflict", nil)
	}
	count, err := uploadPartCount(w.Size, options.PartSize)
	if err != nil {
		return u, false, err
	}
	// The session declares its exact destination but does not fence it yet: beginTx admits it
	// against the guards that exist now, and the row is written when the session freezes.
	// See freezeUpload for why the fence belongs to the commit and not to the session.
	input.Guards = []Guard{{Path: w.Target}}
	key := input.IdempotencyKey
	if len(key) > 1024 || strings.ContainsRune(key, 0) {
		return u, false, E("invalid_request", nil)
	}
	// A session's client key lives in its own index; simple PUT remains independent.
	input.IdempotencyKey = ""
	req, opFingerprint, reserve, err := validateBegin(input)
	if err != nil {
		return u, false, err
	}
	if req.Reservation.PhysicalBytes < w.Size {
		return u, false, E("invalid_request", nil)
	}
	err = p.transaction(ctx, req.Root.OwnerID, func(tx pgx.Tx) error {
		var err error
		u, replay, err = replayUploadTx(ctx, tx, req.Root, req.Actor, key, fingerprint)
		if err != nil || replay {
			return err
		}
		// Before admission, so the quota and the replacement credit the older session held
		// are free for this one. If admission then fails, the whole transaction rolls back
		// and the older session is untouched.
		superseded, err := supersedeOpenUploads(ctx, tx, req.Root, comparePath(req.Root, w.Target))
		if err != nil {
			return err
		}
		op, _, err := p.beginTx(ctx, tx, req, opFingerprint, reserve, false)
		if err != nil {
			return err
		}
		mode := options.Mode
		if mode == "" {
			mode = UploadModeMultipart
		}
		u = Upload{ID: op.ID, Operation: op, PartSize: options.PartSize, PartCount: count, Mode: mode, State: UploadOpen, CreatedAt: op.CreatedAt, ExpiresAt: op.CreatedAt.Add(uploadIdleWindow)}
		var dbKey any
		if key != "" {
			dbKey = key
		}
		if _, err = tx.Exec(ctx, `insert into file_uploads(operation_id,owner_id,principal_id,idempotency_key,client_fingerprint,comparison_path,part_size,part_count,mode,state,created_at,expires_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9,'open',$10,$11)`, u.ID, req.Root.OwnerID, req.Actor.ID, dbKey, fingerprint, comparePath(req.Root, w.Target), u.PartSize, u.PartCount, u.Mode, u.CreatedAt, u.ExpiresAt); err != nil {
			return err
		}
		// The older session's own client learns this on its next request, so the record has
		// to say which session replaced it rather than only that it ended.
		for i := range superseded {
			old := &superseded[i].Operation
			old.Problem = uploadProblem("upload_superseded", "A newer upload session for the same path (upload "+u.ID+") replaced this one before it committed.")
			if err = saveOperation(ctx, tx, *old); err != nil {
				return err
			}
		}
		return nil
	})
	return u, replay, err
}

// supersedeOpenUploads ends the `open` sessions of this root that target path, and returns
// them. It runs inside the creation of a new session for that path.
//
// Two sessions for one path cannot both publish, so one of them has to lose; the only
// question is which, and when. The server cannot tell whether the older one is alive: class A
// parts go straight to the provider, so a healthy transfer and an abandoned tab look the
// same for up to uploadIdleWindow. The request that is creating a session now is the one
// piece of evidence that somebody is still there, so the newer session wins. Refusing it
// instead (what a create-time guard did) made the common case — a retry after the page or
// the import died — wait hours for a session nobody was driving, unless the client knew to
// find that session and abort it first; allowing both made the loser discover it only after
// sending every byte, and reserved the quota twice meanwhile.
//
// The cost is the rare case of two live clients sending the same path: the older one gets
// upload_gone, naming the session that replaced it, on its next signature, part or commit.
//
// Only `open` sessions are ended: nothing of theirs is published, their staged parts are
// private and the ordinary cleanup discards them. A session that is already committing holds
// a guard, so the creation that reaches this point never sees one — beginTx refuses it with
// operation_in_progress and names it.
//
// The authority is the creator's write access to the path: whoever may replace the file
// there may displace an unpublished attempt at it, whichever principal opened that attempt.
func supersedeOpenUploads(ctx context.Context, tx pgx.Tx, root Root, path string) ([]Upload, error) {
	rows, err := tx.Query(ctx, `select operation_id::text,principal_id from file_uploads where owner_id=$1 and comparison_path=$2 and state='open' order by created_at,operation_id`, root.OwnerID, path)
	if err != nil {
		return nil, err
	}
	type candidate struct{ id, principal string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.principal); err != nil {
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
		// Settled as its own principal would settle it, the way expiry does.
		older, err := readUpload(ctx, tx, root, Actor{ID: c.principal, CanWrite: true}, c.id, true)
		if IsCode(err, "not_found") {
			// Another root generation's session targets another namespace.
			continue
		}
		if err != nil {
			return nil, err
		}
		if older.State != UploadOpen {
			continue
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return nil, err
		}
		state := UploadAborted
		if !older.ExpiresAt.After(now) {
			state = UploadExpired
		}
		if err = terminateOpenUpload(ctx, tx, &older, state, now, nil); err != nil {
			if IsCode(err, "recovery_required") {
				// Not provably free of a native attempt, so it is not ours to end: it keeps
				// the path and the creation is refused with its name.
				return nil, &Error{Code: "operation_in_progress", OperationID: c.id, OperationKind: "upload_commit"}
			}
			return nil, err
		}
		out = append(out, older)
	}
	return out, nil
}

// uploadProblem is the stored reason a session ended without publishing.
func uploadProblem(code, detail string) json.RawMessage {
	value, _ := json.Marshal(map[string]string{"code": code, "detail": detail})
	return value
}

// TouchUpload records server-visible progress on an open session and slides its idle
// expiry. Only activity that can still produce bytes counts: a signature request (class A
// parts go straight to the provider, so this is the only request a live transfer must keep
// making) or a received part (class C). A status read does not, because polling must not
// keep an abandoned session — and the quota it reserves — alive.
func (p *Postgres) TouchUpload(ctx context.Context, root Root, actor Actor, id string) (Upload, error) {
	var u Upload
	if err := validateRoot(root); err != nil {
		return u, err
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var err error
		u, err = readUpload(ctx, tx, root, actor, id, true)
		if err != nil || u.State != UploadOpen {
			return err
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		until := now.Add(uploadIdleWindow)
		if cap := u.CreatedAt.Add(uploadSessionLifetime); cap.Before(until) {
			until = cap
		}
		if _, err = tx.Exec(ctx, `update file_uploads set expires_at=$3 where operation_id=$1 and owner_id=$2 and state='open'`, id, root.OwnerID, until); err != nil {
			return err
		}
		u.ExpiresAt = until
		return nil
	})
	return u, err
}

func (p *Postgres) GetUpload(ctx context.Context, root Root, actor Actor, id string) (Upload, error) {
	var u Upload
	if err := validateRoot(root); err != nil {
		return u, err
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		var err error
		u, err = readUpload(ctx, tx, root, actor, id, false)
		if err != nil {
			return err
		}
		return expireLoadedUpload(ctx, tx, &u)
	})
	return u, err
}

// ListUploads uses an opaque UUID keyset, bounded by limit. Authorization and the
// exact root are checked before a row occupies a result slot.
func (p *Postgres) ListUploads(ctx context.Context, root Root, actor Actor, limit int, after string) ([]Upload, error) {
	if err := validateRoot(root); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1001 || after != "" && !validID(after) {
		return nil, E("invalid_request", nil)
	}
	if actor.ID == "" || (!actor.CanRead && !actor.CanWrite) || storage.ValidatePath(actor.Scope) != nil {
		return nil, E("forbidden", nil)
	}
	var out []Upload
	// A listing writes nothing, so it takes no owner lock: the pages of a session are read while
	// the session settles, and both used to wait for each other. The READ ONLY transaction makes
	// that structural, and readUploadSnapshot is the only reader used here because it takes no
	// `for update` (SQLSTATE 25006 would otherwise turn every list into a 500).
	err := p.readOnly(ctx, func(tx pgx.Tx) error {
		rootJSON, err := json.Marshal(root)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select u.operation_id::text from file_uploads u join file_operations o on o.id=u.operation_id and o.owner_id=u.owner_id where u.owner_id=$1 and ($2='' or u.operation_id>nullif($2,'')::uuid) and (u.principal_id=$3 or $4) and ($6='' or u.comparison_path=$6 or starts_with(u.comparison_path,$6||'/')) and o.snapshot->'Root'=$7::jsonb order by u.operation_id limit $5`, root.OwnerID, after, actor.ID, actor.ManageOwner && actor.ID == "owner:"+root.OwnerID && actor.CanWrite, limit, comparePath(root, actor.Scope), rootJSON)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			// An elapsed open session comes back as expired from the read itself. It is not
			// terminated here: that would cancel the operation and release its reservation,
			// which is a write, and ExpireUploads settles the row under the owner lock.
			u, err := readUploadSnapshot(ctx, tx, root, actor, id)
			if err != nil {
				return err
			}
			out = append(out, u)
		}
		return nil
	})
	return out, err
}

func uploadWritable(u Upload, now time.Time) error {
	if u.State == UploadOpen {
		if !u.ExpiresAt.After(now) {
			return E("upload_expired", nil)
		}
		if u.Operation.Terminal() || u.Operation.CancelRequested {
			return E("upload_gone", nil)
		}
		return nil
	}
	switch u.State {
	case UploadCommitted:
		return E("already_committed", nil)
	case UploadCommitting, UploadUnknown:
		return E("commit_in_progress", nil)
	default:
		return E("upload_gone", nil)
	}
}

func (p *Postgres) AcceptUploadPart(ctx context.Context, root Root, actor Actor, id string, part PartReceipt) (PartReceipt, bool, error) {
	var out PartReceipt
	var replay bool
	if err := validateRoot(root); err != nil {
		return out, false, err
	}
	if part.Number < 1 || part.Size < 1 || part.Handle == "" || strings.ContainsRune(part.Handle, 0) || len(part.Handle) > 8192 || len(part.SHA256) != 64 {
		return out, false, E("invalid_request", nil)
	}
	for _, c := range part.SHA256 {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return out, false, E("invalid_request", nil)
		}
	}
	err := p.transaction(ctx, root.OwnerID, func(tx pgx.Tx) error {
		u, err := readUpload(ctx, tx, root, actor, id, true)
		if err != nil {
			return err
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
		if part.Number > u.PartCount {
			return E("invalid_part", nil)
		}
		want := u.PartSize
		if part.Number == u.PartCount {
			want = w.Size - int64(u.PartCount-1)*u.PartSize
		}
		if part.Size != want {
			return E("invalid_part", nil)
		}
		err = tx.QueryRow(ctx, `select number,size,sha256,handle,sequence from file_upload_parts where operation_id=$1 and owner_id=$2 and number=$3`, id, root.OwnerID, part.Number).Scan(&out.Number, &out.Size, &out.SHA256, &out.Handle, &out.Sequence)
		if err == nil {
			if out.Size != part.Size || out.SHA256 != part.SHA256 {
				return E("part_conflict", nil)
			}
			replay = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		err = tx.QueryRow(ctx, `update file_uploads set received_parts=received_parts+1,received_bytes=received_bytes+$3,ack_sequence=ack_sequence+1,
			expires_at=least(created_at+make_interval(secs => $4),$5::timestamptz+make_interval(secs => $6)) where operation_id=$1 and owner_id=$2 returning ack_sequence`, id, root.OwnerID, part.Size, uploadSessionLifetime.Seconds(), now, uploadIdleWindow.Seconds()).Scan(&part.Sequence)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into file_upload_parts(operation_id,owner_id,number,size,sha256,handle,sequence) values($1,$2,$3,$4,$5,$6,$7)`, id, root.OwnerID, part.Number, part.Size, part.SHA256, part.Handle, part.Sequence)
		out = part
		return err
	})
	return out, replay, err
}

// A zero watermark captures the current maximum ACK sequence. Subsequent pages
// use after=last.Sequence and the same watermark; ACKs after it never shift pages.
func (p *Postgres) ListUploadParts(ctx context.Context, root Root, actor Actor, id string, limit int, after, watermark int64) ([]PartReceipt, int64, error) {
	if err := validateRoot(root); err != nil {
		return nil, 0, err
	}
	if limit < 1 || limit > 1001 || after < 0 || watermark < 0 || watermark == 0 && after != 0 {
		return nil, 0, E("invalid_request", nil)
	}
	var out []PartReceipt
	err := p.readOnly(ctx, func(tx pgx.Tx) error {
		// Authorization only; this path reports receipts, so the session's derived expiry
		// state is not part of the answer. The snapshot read is required all the same: the
		// transaction is READ ONLY and any `for update` here is SQLSTATE 25006.
		if _, err := readUploadSnapshot(ctx, tx, root, actor, id); err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRow(ctx, `select ack_sequence from file_uploads where operation_id=$1 and owner_id=$2`, id, root.OwnerID).Scan(&current); err != nil {
			return err
		}
		if watermark == 0 {
			watermark = current
		}
		if watermark > current || after > watermark {
			return E("invalid_request", nil)
		}
		rows, err := tx.Query(ctx, `select number,size,sha256,handle,sequence from file_upload_parts where operation_id=$1 and owner_id=$2 and sequence>$3 and sequence<=$4 order by sequence limit $5`, id, root.OwnerID, after, watermark, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r PartReceipt
			if err = rows.Scan(&r.Number, &r.Size, &r.SHA256, &r.Handle, &r.Sequence); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, watermark, err
}
