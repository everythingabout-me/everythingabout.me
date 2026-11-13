package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"

	"everythingabout.me/backend/internal/storage"
)

// A direct upload publishes at the destination with the client's own conditional PUT, so
// the bytes can be in the tree when the commit that would index them fails. Retrying that
// row used to create a new session and be refused with name_conflict — the destination
// exists, but only natively, so the library shows nothing and the person has no way to
// finish the upload. The bytes are the source of truth, so the retry adopts them.
//
// Adoption needs proof that the object is *this owner's own* publication and not somebody
// else's file at the same name: the failed session recorded the exact native revision its
// client wrote, and that revision must still be the object's own. Nothing weaker is
// accepted, because a wrong adoption would publish a stranger's bytes under this path.
type filesPublicationCandidate struct {
	// size is the published file's length, as the session recorded it.
	size int64
	// handle is the native revision the session observed after the client's PUT.
	handle []byte
}

// filesReclaimsPublication answers whether the object at target is a failed direct
// publication of this owner that the retry may settle instead of uploading again.
func (s *Server) filesReclaimsPublication(ctx context.Context, owner, target string, entry storage.Entry, size int64) bool {
	if s.db == nil || entry.Kind != storage.File || len(entry.Revision) == 0 {
		return false
	}
	// Only a direct session can leave published bytes behind before settlement: parts of a
	// multipart session are never visible as an object.
	rows, err := s.db.Pool().Query(ctx, `select o.stage_size, o.stage_handle
		from file_operations o
		join file_uploads u on u.operation_id = o.id and u.owner_id = o.owner_id
		where o.owner_id = $1::uuid
		  and o.action = 'upload_commit'
		  and o.intent_target = $2
		  and o.state = 'failed'
		  and u.mode = 'direct'
		  and o.stage_handle is not null
		  -- Only a path the index does not know: an indexed destination is an ordinary
		  -- conflict, and replacing a known file is the client's decision, not a recovery.
		  and file_tree_resolve(o.owner_id, o.intent_target) is null
		order by o.created_at desc
		limit 8`, owner, target)
	if err != nil {
		// A proof that cannot be read is not a proof. The retry keeps the ordinary answer.
		slog.Warn("could not read failed publications for a retried upload", "target", target, "cause", err)
		return false
	}
	defer rows.Close()
	var candidates []filesPublicationCandidate
	for rows.Next() {
		var recorded int64
		var handle []byte
		if err := rows.Scan(&recorded, &handle); err != nil {
			slog.Warn("could not read a failed publication", "target", target, "cause", err)
			return false
		}
		candidates = append(candidates, filesPublicationCandidate{size: recorded, handle: handle})
	}
	if err := rows.Err(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("could not read failed publications for a retried upload", "target", target, "cause", err)
		return false
	}
	return filesPublicationMatches(entry, size, candidates)
}

// filesPublicationMatches is the whole rule, kept separate so it can be read and tested
// without a database: the object must be a file of exactly the size the client is
// uploading, and one failed session of this path must have left exactly this revision.
func filesPublicationMatches(entry storage.Entry, size int64, candidates []filesPublicationCandidate) bool {
	if entry.Kind != storage.File || len(entry.Revision) == 0 || size != entry.Size {
		return false
	}
	for _, candidate := range candidates {
		if candidate.size == entry.Size && bytes.Equal(candidate.handle, entry.Revision) {
			return true
		}
	}
	return false
}
