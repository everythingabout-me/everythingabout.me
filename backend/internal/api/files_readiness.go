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
func filesUsageFields(usage *fileops.Usage, wholeOwner bool) (*filesQuota, *filesReservations) {
	if usage == nil || !wholeOwner {
		return nil, nil
	}
	quota := &filesQuota{LogicalBytes: usage.LogicalBytes, LogicalLimitBytes: usage.LogicalLimit, PhysicalBytes: usage.PhysicalBytes, PhysicalLimitBytes: usage.PhysicalLimit}
	reservations := &filesReservations{LogicalBytes: usage.LogicalReserved, PhysicalBytes: usage.PhysicalReserved}
	return quota, reservations
}
