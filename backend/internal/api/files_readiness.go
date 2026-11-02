package api

import (
	"context"

	"everythingabout.me/backend/internal/fileops"
)

func (s *Server) filesWriteReadiness(ctx context.Context, f *fileRequest) (bool, *fileops.Usage, error) {
	if s.cfg == nil || !s.cfg.Storage.CoordinatedWrites || s.filesJournal == nil {
		return false, nil, nil
	}
	native, ok := f.Driver.(fileops.NativeActions)
	if !ok {
		return false, nil, nil
	}
	b := native.StorageBinding()
	root := fileops.Root{OwnerID: b.OwnerID, ConnectionID: b.ConnectionID, Generation: b.Generation, ContentRoot: b.ContentRoot, SystemRoot: b.SystemRoot, CaseSensitive: native.StorageNameRules().CaseSensitive}
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
func filesUsageFields(usage *fileops.Usage, wholeOwner bool) (any, any) {
	if usage == nil || !wholeOwner {
		return nil, nil
	}
	quota := map[string]any{"logicalBytes": usage.LogicalBytes, "logicalLimitBytes": usage.LogicalLimit, "physicalBytes": usage.PhysicalBytes, "physicalLimitBytes": usage.PhysicalLimit}
	reservations := map[string]any{"logicalBytes": usage.LogicalReserved, "physicalBytes": usage.PhysicalReserved}
	return quota, reservations
}
