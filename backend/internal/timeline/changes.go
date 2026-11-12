package timeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/filechanges"
)

const (
	// changePollInterval is how often the organizer asks the durable change feed
	// whether an owner has facts beyond its checkpoint. It is a wakeup cadence, not
	// a work budget: one statement per tick over one row per owner, and the work it
	// wakes is bounded by changeBatch rather than by this interval.
	changePollInterval = 5 * time.Second
	// changeBatch is how many feed facts one `changes` job consumes. It matches the
	// file page size: at most 200 analyses, and at most 200 ambient reads capped by
	// maxAmbientBytes. The checkpoint advances only after the whole batch settled,
	// so a crash replays at most one batch. It is a work bound, not a timer.
	changeBatch = 200
)

// changeFeeder is the replacement for the old 20-minute sweep: it asks the durable
// feed (not the nodes table) which owners have settled facts beyond their
// checkpoint and schedules one coalescing `changes` job for each. A restart loses
// nothing because both the checkpoint and the job row are persisted.
func (s *Service) changeFeeder(ctx context.Context) {
	s.enqueueChangedOwners(ctx)
	ticker := time.NewTicker(changePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.enqueueChangedOwners(ctx)
		}
	}
}

// enqueueChangedOwners inserts at most one unfinished `changes` job per owner with
// an unread feed position. The partial unique index coalesces concurrent inserts;
// the NOT EXISTS covers a failed job while its retry backoff has not elapsed, so a
// deterministic failure does not create one row per tick.
func (s *Service) enqueueChangedOwners(ctx context.Context) {
	tag, err := s.db.Pool().Exec(ctx, `insert into timeline_jobs (user_id, kind)
select h.owner_id, 'changes'
  from file_change_heads h
  left join timeline_change_positions p on p.owner_id = h.owner_id
 where h.head_position > coalesce(p.position, 0)
   and not exists (
     select 1 from timeline_jobs j
      where j.user_id = h.owner_id
        and j.kind = 'changes'
        and (j.status in ('pending', 'running')
             or (j.status = 'failed' and j.available_at > now())))
on conflict do nothing`)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("timeline: failed to enqueue change consumption", "error", err)
		}
		return
	}
	if tag.RowsAffected() > 0 {
		slog.Debug("timeline: queued owners with unread changes", "owners", tag.RowsAffected())
	}
}

// runChanges consumes one bounded batch of settled change facts. It is the only
// trigger that turns a completed file operation into stream analysis: the fact was
// committed with the index settlement, and the checkpoint is advanced only after
// the batch's facts have been analyzed and their months queued. A restart, a
// duplicated fact or a crash mid-batch therefore costs a replay, never a lost file.
func (s *Service) runChanges(ctx context.Context, item *job) error {
	owner := item.UserID
	head, err := filechanges.ReadHead(ctx, s.db.Pool(), owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	position, err := s.changePosition(ctx, owner)
	if err != nil {
		return err
	}
	if position < head.RetainedAfter || position > head.Position {
		return s.recoverChangeGap(ctx, owner, head)
	}
	if position == head.Position {
		return nil
	}

	changes, err := filechanges.Read(ctx, s.db.Pool(), owner, position, head.Position, changeBatch)
	if err != nil {
		return err
	}
	// Positions are contiguous, so the first fact of this page must follow the
	// checkpoint. A higher position means a prune moved the retained floor past
	// rows this consumer never saw; reading the surviving tail and advancing would
	// silently lose them.
	if len(changes) > 0 && changes[0].Position != position+1 {
		return s.recoverChangeGapAfterRead(ctx, owner)
	}
	if len(changes) > 0 {
		if err := s.analyzeChanges(ctx, owner, changes); err != nil {
			return err
		}
		// Queue before advancing the checkpoint: if this step fails or the process
		// dies, the batch replays instead of leaving analyzed facts that never
		// produced a month (and therefore never produced events).
		if err := s.enqueueStaleMonths(ctx, owner); err != nil {
			return err
		}
		position = changes[len(changes)-1].Position
	}

	// Re-read the boundary after the batch: an explicit prune takes the same head
	// lock but does not fence this reader, so rows this job never saw may have been
	// removed. A checkpoint below the retained floor is missing work, and it must
	// become one full backfill rather than a silent skip.
	latest, err := filechanges.ReadHead(ctx, s.db.Pool(), owner)
	if err != nil {
		return err
	}
	if len(changes) == 0 || position < latest.RetainedAfter {
		return s.recoverChangeGap(ctx, owner, latest)
	}
	return s.saveChangePosition(ctx, owner, position)
}

// recoverChangeGapAfterRead re-reads the boundary because the head may have moved
// while this job was reading an older page.
func (s *Service) recoverChangeGapAfterRead(ctx context.Context, owner string) error {
	head, err := filechanges.ReadHead(ctx, s.db.Pool(), owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.recoverChangeGap(ctx, owner, head)
}

// recoverChangeGap is the only correct response to a feed that no longer carries
// the checkpoint: the retained rows cannot reconstruct the skipped transitions, so
// the organizer queues the existing full backfill once and moves the checkpoint to
// the current head. Reset also lands here, because it sets the retained floor to
// the head. This is the same recovery the manual rebuild performs; it is not a
// second task concept.
func (s *Service) recoverChangeGap(ctx context.Context, owner string, head filechanges.Head) error {
	slog.Warn("timeline: change checkpoint is outside the retained feed; queueing a full backfill",
		"user", owner, "head", head.Position, "retained_after", head.RetainedAfter)
	if err := s.enqueueBackfill(ctx, owner); err != nil {
		return err
	}
	// This may move the checkpoint backwards (a head reset or a lost head row), which
	// normal advancement must never do but recovery must: the feed head is the
	// authority, and a checkpoint ahead of it would otherwise repeat this gap forever.
	return s.resetChangePosition(ctx, owner, head.Position)
}

// analyzeChanges turns the newest fact per entry into a FileInput and hands it to
// the existing Analyzer. Facts for one entry in one batch (create then update) are
// collapsed to the last one; the per-revision check below makes a replay of an
// already-analyzed fact a no-op. Folders, deletions and entries that vanished after
// the fact are skipped here: a deletion already cascaded the asset row, and the
// month comparison in enqueueStaleMonths removes the resulting hollow month.
func (s *Service) analyzeChanges(ctx context.Context, owner string, changes []filechanges.Change) error {
	order := make([]string, 0, len(changes))
	latest := make(map[string]filechanges.Change, len(changes))
	for _, change := range changes {
		if change.EntryID == nil || *change.EntryID == "" || change.After == nil || change.After.Kind != "file" {
			continue
		}
		id := *change.EntryID
		if _, seen := latest[id]; !seen {
			order = append(order, id)
		}
		latest[id] = change
	}
	if len(order) == 0 {
		return nil
	}

	stored, err := s.assetRevisions(ctx, owner, order)
	if err != nil {
		return err
	}
	for _, id := range order {
		change := latest[id]
		revision := change.After.Revision
		if asset, ok := stored[id]; ok && asset.revision == revision && asset.pipeline >= pipelineVersion {
			continue
		}
		file, err := s.fileByID(ctx, owner, id)
		if err != nil {
			return err
		}
		if file == nil {
			// The entry changed again and was deleted before this batch ran; its asset
			// cascaded with it and the month comparison removes the hollow snapshot.
			continue
		}
		if err := s.processFile(ctx, *file, revision); err != nil {
			return err
		}
	}
	return nil
}

type assetRevision struct {
	revision string
	pipeline int
}

func (s *Service) assetRevisions(ctx context.Context, owner string, ids []string) (map[string]assetRevision, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select node_id::text, source_revision, pipeline_version
		   from timeline_assets
		  where user_id = $1::uuid and node_id = any($2::uuid[])`, owner, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]assetRevision, len(ids))
	for rows.Next() {
		var id string
		var asset assetRevision
		if err := rows.Scan(&id, &asset.revision, &asset.pipeline); err != nil {
			return nil, err
		}
		out[id] = asset
	}
	return out, rows.Err()
}

// fileByID reads the entry's current indexed facts. Reading the current row rather
// than the fact's snapshot makes analysis converge on the newest state even when
// more changes arrived after this batch; a newer revision will simply be analyzed
// again, while a stale snapshot would be recorded as current.
func (s *Service) fileByID(ctx context.Context, owner, nodeID string) (*FileInput, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select n.id::text, n.owner_id::text, n.path, n.name, n.parent, n.size,
		        coalesce(n.mime, ''),
		        coalesce(n.source_modified_at, n.updated_at, now()),
		        n.updated_at, n.created_at, coalesce(n.etag, '')
		   from file_entries n
		  where n.owner_id = $1::uuid and n.id = $2::uuid and n.is_dir = false`,
		owner, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files, err := scanFileInputs(rows)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	return &files[0], nil
}

// changePosition returns the last fully analyzed feed position for an owner. A
// missing row is position zero: the retained feed is replayed from its start, which
// is idempotent, and a pruned start is detected as a gap and becomes a backfill.
func (s *Service) changePosition(ctx context.Context, owner string) (int64, error) {
	var position int64
	err := s.db.Pool().QueryRow(ctx,
		`select position from timeline_change_positions where owner_id = $1::uuid`, owner).Scan(&position)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return position, err
}

// saveChangePosition only moves the checkpoint forward. A rebuild may have jumped
// it to the head while this job was analyzing an older batch, and a regression
// would replay that batch (or, worse, re-read a now-pruned range).
func (s *Service) saveChangePosition(ctx context.Context, owner string, position int64) error {
	_, err := s.db.Pool().Exec(ctx,
		`insert into timeline_change_positions (owner_id, position, updated_at)
		 values ($1::uuid, $2, now())
		 on conflict (owner_id) do update
		    set position = greatest(timeline_change_positions.position, excluded.position),
		        updated_at = now()
		  where timeline_change_positions.position < excluded.position`,
		owner, position)
	return err
}

// resetChangePosition moves the checkpoint to whatever the feed head is, including
// backwards. It exists only for gap recovery, where a stale checkpoint would
// otherwise re-trigger the same gap on every poll.
func (s *Service) resetChangePosition(ctx context.Context, owner string, position int64) error {
	_, err := s.db.Pool().Exec(ctx,
		`insert into timeline_change_positions (owner_id, position, updated_at)
		 values ($1::uuid, $2, now())
		 on conflict (owner_id) do update
		    set position = excluded.position, updated_at = now()`,
		owner, position)
	return err
}

func (s *Service) enqueueBackfill(ctx context.Context, owner string) error {
	_, err := s.db.Pool().Exec(ctx,
		`insert into timeline_jobs (user_id, kind) values ($1::uuid, 'backfill')
		 on conflict do nothing`, owner)
	return err
}
