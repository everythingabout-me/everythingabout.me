package api

import (
	"fmt"
	"log/slog"
	"net/http"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/treescan"
)

// filesSyncRequest carries the only choice the caller has: preview or apply.
type filesSyncRequest struct {
	DryRun bool `json:"dryRun"`
}

// filesSyncResponse is the scan report. RemovedPaths is populated for a preview so
// the caller can show what a sync would delete before it happens.
type filesSyncResponse struct {
	DryRun       bool     `json:"dryRun"`
	Scanned      int      `json:"scanned"`
	Inserted     int      `json:"inserted"`
	Updated      int      `json:"updated"`
	Unchanged    int      `json:"unchanged"`
	Removed      int      `json:"removed"`
	RemovedPaths []string `json:"removedPaths,omitempty"`
}

// handleFilesSync makes the index equal the native tree for this owner: entries the
// tree no longer has are removed (with an audit record and a deleted change fact)
// and observed entries are imported. Configuration decides which tree is served, so
// the caller is not choosing a source of truth here, only asking for a refresh.
func (s *Server) handleFilesSync(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if s.db == nil {
		return fc.NewProblem(503, "temporarily_unavailable", "The index database is not available.")
	}
	var request filesSyncRequest
	if r.ContentLength != 0 {
		if err := strictFilesJSON(r, &request); err != nil {
			return err
		}
	}
	native, root, _, err := s.fileMutationContext(r.Context(), f)
	if err != nil {
		return err
	}
	// The request context is the only deadline. The scan is already bounded by the
	// driver's own timeouts and treescan's entry/page limits; adding a wall clock on
	// top would invent a failure nobody can explain from the response.
	ctx := r.Context()
	owner := f.Principal.User.ID
	// The scan lock also takes the owner write-admission lock every /api/files
	// settlement takes, so a sync never races an upload or a delete and a second
	// sync is refused instead of queued.
	conn, unlock, err := nodes.LockScanOwner(ctx, s.db.Pool(), owner)
	if err != nil {
		return fc.NewProblem(409, "sync_in_progress", "Another scan or write session holds this owner's storage lock.")
	}
	defer unlock()
	// A scan must not race a deletion: an accepted delete has already lost its index
	// rows while its native purge still runs, so re-importing those objects would
	// resurrect a subtree the user deleted. The operation journal records that
	// accepted-but-not-settled purge; the legacy fs_jobs table this check used to
	// consult could not see a `/api/files` delete at all.
	deleting, err := fileops.PendingDeleteRoots(ctx, conn, owner, root.Namespace())
	if err != nil {
		return err
	}
	if len(deleting) > 0 {
		return fc.NewProblem(409, "delete_in_progress", fmt.Sprintf("The permanent deletion of %q has not finished purging; wait for it to settle before syncing.", deleting[0]))
	}
	descriptor := native.Describe()
	result, err := treescan.Scan(ctx, native, treescan.Options{
		Names:      descriptor.Names,
		PageSize:   1000,
		MaxPages:   100000,
		MaxEntries: 1000000,
		MaxDepth:   128,
	})
	if err != nil {
		return fc.NewProblem(502, "storage_failure", "The native tree could not be scanned: "+err.Error())
	}
	report, err := nodes.ApplyScan(ctx, conn, owner, result, nodes.ScanApplyOptions{DryRun: request.DryRun, ChangeRoot: &root, Sync: true})
	if err != nil {
		slog.Error("owner storage sync failed", "owner", owner, "dry_run", request.DryRun, "error", err)
		return fc.NewProblem(503, "sync_failed", "The index could not be synchronized: "+err.Error())
	}
	return writeFilesJSON(w, http.StatusOK, filesSyncResponse{
		DryRun:       request.DryRun,
		Scanned:      report.Scanned,
		Inserted:     report.Inserted,
		Updated:      report.Updated,
		Unchanged:    report.Unchanged,
		Removed:      report.Removed,
		RemovedPaths: report.RemovedPaths,
	})
}
