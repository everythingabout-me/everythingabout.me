package api

import (
	"context"

	"everythingabout.me/backend/internal/fileops"
)

func (s *Server) filesWriteReadiness(ctx context.Context, f *fileRequest) (bool, *fileops.Usage, error) {
	if s.cfg == nil || !s.cfg.Storage.CoordinatedWrites || s.filesJournal == nil {
		return false, nil, nil
	}
	native, ok := filesNativeDriver(f)
	if !ok {
		return false, nil, nil
	}
	root, ok := filesRootFor(native)
	if !ok {
		return false, nil, nil
	}
	usage, err := s.filesJournal.Usage(ctx, root, fileops.Actor{ID: "owner:" + root.OwnerID, CanRead: true, CanWrite: true, ManageOwner: true})
	if fileops.IsCode(err, "baseline_required") || fileops.IsCode(err, "root_conflict") {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	allowed := f.Principal.Token == nil || f.Principal.Token.CanWrite
	return allowed, &usage, nil
}

// filesUsageFields publishes the two byte counters a client sees. They answer
// different questions and are not interchangeable:
//
//   - GET B/me usage.bytes/files (read separately from the index) count the indexed
//     files visible in the selected scope. They are a database view and may lag the
//     native tree.
//   - quota.logicalBytes is the owner's charged budget: the initial compared baseline
//     plus application journal changes. Admission compares it against
//     quota.logicalLimitBytes, and reservations.logicalBytes is the in-flight part of
//     that budget, so remaining room is
//     logicalLimitBytes - logicalBytes - reservations.logicalBytes. Never use
//     usage.bytes for admission: it is a derived index total, not the ledger.
//
// The two counters can differ because the budget is not recomputed from the index,
// and pending, deleted, outside or reconciled state can make one lag the other. See
// docs/api/api.md §8 for the practical rule.
//
// wholeOwner selects owner-root output: GET B/me passes f.Scope == "/", while
// capabilities passes f.Scope == "/" && selected path == "/". A scoped read gets nil
// for both quota and reservations rather than a partial owner-wide number.
func filesUsageFields(usage *fileops.Usage, wholeOwner bool) (*filesQuota, *filesReservations) {
	if usage == nil || !wholeOwner {
		return nil, nil
	}
	quota := &filesQuota{LogicalBytes: usage.LogicalBytes, LogicalLimitBytes: usage.LogicalLimit, PhysicalBytes: usage.PhysicalBytes, PhysicalLimitBytes: usage.PhysicalLimit}
	reservations := &filesReservations{LogicalBytes: usage.LogicalReserved, PhysicalBytes: usage.PhysicalReserved}
	return quota, reservations
}
