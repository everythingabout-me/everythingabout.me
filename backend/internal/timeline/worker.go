package timeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"everythingabout.me/backend/internal/paths"
)

const (
	// How many files one pass handles. Too large and it holds the transaction and
	// lease too long; too small and it takes more rounds.
	fileBatch = 200
	// Read cap for health CSVs. Anything bigger should not be casually pulled into
	// memory in the background.
	maxAmbientBytes = 8 << 20
)

// Start launches the background organizer. It assumes a single instance; multiple
// instances would need leases backed by heartbeats.
func (s *Service) Start(ctx context.Context) {
	if !s.cfg.Enabled {
		slog.Info("timeline: disabled")
		return
	}
	if !s.started.CompareAndSwap(false, true) {
		return
	}

	done := make(chan struct{})
	s.doneMu.Lock()
	s.done = done
	s.doneMu.Unlock()

	s.recoverJobs(ctx)
	var running sync.WaitGroup
	running.Add(s.cfg.Workers + 2)
	for i := 0; i < s.cfg.Workers; i++ {
		go func() {
			defer running.Done()
			s.worker(ctx)
		}()
	}
	go func() {
		defer running.Done()
		s.bootstrap(ctx)
	}()
	go func() {
		defer running.Done()
		s.sweeper(ctx)
	}()
	go func() {
		running.Wait()
		close(done)
	}()
	slog.Info("timeline: started", "workers", s.cfg.Workers, "budget", s.engine.Budget)
}

// Wait blocks until every goroutine Start launched has returned, or ctx ends
// first. It returns immediately when the organizer is disabled or never started
// (a coordinated-writes deployment starts none at all).
//
// Waiting is not the same as draining a job. A worker that is asked to stop
// abandons its job with status 'running', which recoverJobs resets at the next
// start; that is deliberate resumption. Without this wait, though, the caller
// closes the pool while the worker is still finishing the database call that
// observes the cancellation, turning a clean restart into a spurious failure.
func (s *Service) Wait(ctx context.Context) error {
	s.doneMu.Lock()
	done := s.done
	s.doneMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) recoverJobs(ctx context.Context) {
	// Put jobs left running by the previous process back in the queue; safe for a
	// single instance.
	if _, err := s.db.Pool().Exec(ctx,
		`update timeline_jobs
		    set status = 'pending', lease_until = null
		  where status = 'running'`); err != nil {
		slog.Warn("timeline: failed to recover interrupted jobs", "error", err)
	}
}

// bootstrap queues users that were never organized at startup. A database with
// existing data backfills from here.
func (s *Service) bootstrap(ctx context.Context) {
	// Give the HTTP server a moment to come up so we do not fight it for connections.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	if _, err := s.db.Pool().Exec(ctx,
		`insert into timeline_jobs (user_id, kind)
		 select n.owner_id, 'backfill'
		   from nodes n
		   left join timeline_assets a on a.user_id = n.owner_id and a.node_id = n.id
		  where n.owner_id is not null
		    and n.is_dir = false
		    and a.node_id is null
		  group by n.owner_id
		 on conflict do nothing`); err != nil {
		slog.Warn("timeline: failed to enqueue startup backfill", "error", err)
	}

	// Since 013 made events the read model, snapshots in older databases have a null
	// source_rev. Queue those months right here instead of relying on a scan: otherwise
	// a library with no new files keeps an empty stream until the next file change.
	tag, err := s.db.Pool().Exec(ctx,
		`insert into timeline_jobs (user_id, kind, month)
		 select s.user_id, 'month', s.month
		   from timeline_snapshots s
		  where s.source_rev is null
		 on conflict do nothing`)
	if err != nil {
		slog.Warn("timeline: failed to enqueue startup months", "error", err)
	} else if tag.RowsAffected() > 0 {
		slog.Info("timeline: queued months for event backfill", "months", tag.RowsAffected())
	}
}

// sweeper is the safety net: changes that never pass through Notify (CLI reconcile,
// writes from the public API) get picked up via users with recently updated rows.
func (s *Service) sweeper(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.db.Pool().Exec(ctx,
				`insert into timeline_jobs (user_id, kind)
				 select distinct owner_id, 'scan'
				   from nodes
				  where owner_id is not null
				    and is_dir = false
				    and updated_at > now() - interval '25 minutes'
				 on conflict do nothing`); err != nil {
				slog.Warn("timeline: failed to enqueue periodic scan", "error", err)
			}
			if _, err := s.db.Pool().Exec(ctx,
				`insert into timeline_jobs (user_id, kind)
				 select n.owner_id, 'backfill'
				   from nodes n
				   left join timeline_assets a on a.user_id = n.owner_id and a.node_id = n.id
				  where n.owner_id is not null and n.is_dir = false and a.node_id is null
				  group by n.owner_id
				 on conflict do nothing`); err != nil {
				slog.Warn("timeline: failed to enqueue periodic backfill", "error", err)
			}
		}
	}
}

func (s *Service) worker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		item, err := s.claimJob(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("timeline: failed to claim job", "error", err)
			sleep(ctx, 3*time.Second)
			continue
		}
		if item == nil {
			sleep(ctx, 2*time.Second)
			continue
		}
		s.runJob(ctx, item)
	}
}

func (s *Service) runJob(parent context.Context, item *job) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Minute)
	defer cancel()

	var err error
	switch item.Kind {
	case "scan":
		err = s.runScan(ctx, item)
	case "backfill":
		err = s.runBackfill(ctx, item)
	case "month":
		err = s.runMonth(ctx, item)
	default:
		err = fmt.Errorf("unknown job kind %q", item.Kind)
	}

	if err != nil {
		if parent.Err() != nil {
			// The process is shutting down; leave the job running so the next start
			// recovers it.
			return
		}
		slog.Warn("timeline: job failed", "job", item.ID, "kind", item.Kind, "error", err)
		s.failJob(context.Background(), item, err)
		return
	}
	s.finishJob(context.Background(), item.ID)
}

func (s *Service) runScan(ctx context.Context, item *job) error {
	// Cap the rounds per scan so bad data cannot wedge the worker.
	for round := 0; round < 50; round++ {
		files, err := s.recentFiles(ctx, item.UserID, 300)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			break
		}
		for _, file := range files {
			if err := s.processFile(ctx, file); err != nil {
				return err
			}
		}
		if len(files) < 300 {
			break
		}
	}
	return s.enqueueStaleMonths(ctx, item.UserID)
}

func (s *Service) runBackfill(ctx context.Context, item *job) error {
	cursor := item.Cursor
	processed := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		files, next, err := s.backfillFiles(ctx, item.UserID, cursor, fileBatch)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			break
		}
		for _, file := range files {
			if err := s.processFile(ctx, file); err != nil {
				return err
			}
			processed++
		}
		cursor = next
		s.saveCursor(ctx, item.ID, cursor)
	}
	if err := s.enqueueStaleMonths(ctx, item.UserID); err != nil {
		return err
	}
	if processed > 0 {
		slog.Info("timeline: backfill finished", "user", item.UserID, "files", processed)
	}
	return nil
}

func (s *Service) runMonth(ctx context.Context, item *job) error {
	month, ok := parseMonth(item.Month, s.engine.Location)
	if !ok {
		return fmt.Errorf("invalid month: %q", item.Month)
	}
	return s.rebuildMonth(ctx, item.UserID, month)
}

// processFile turns one file into a row in timeline_assets.
// It is the only place that touches object bytes (and only for small files such as
// health CSVs).
func (s *Service) processFile(ctx context.Context, file FileInput) error {
	facts, err := s.engine.Analyzer.Analyze(ctx, file)
	if err != nil {
		return err
	}
	localDate := s.engine.localDate(facts.EventTime)

	ambient := false
	if facts.Ambient {
		for _, extractor := range s.engine.Ambient {
			if !extractor.Match(file.Path, file.Mime) {
				continue
			}
			samples, err := s.extractAmbient(ctx, file, extractor)
			if err != nil {
				slog.Debug("timeline: failed to parse data file",
					"path", file.Path, "extractor", extractor.Name(), "error", err)
				break
			}
			if len(samples) > 0 {
				if err := s.saveSamples(ctx, file, samples); err != nil {
					return err
				}
				ambient = true
			}
			break
		}
	}

	_, err = s.upsertFacts(ctx, file, facts, localDate, ambient)
	return err
}

func (s *Service) extractAmbient(
	ctx context.Context,
	file FileInput,
	extractor AmbientExtractor,
) ([]SeriesSample, error) {
	if file.Size > maxAmbientBytes {
		return nil, nil
	}
	reader, err := s.storage.Get(ctx, paths.KeyFor(file.Path, file.UserID))
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	data, err := io.ReadAll(io.LimitReader(reader, maxAmbientBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAmbientBytes {
		return nil, nil
	}
	return extractor.Extract(ctx, file, data)
}

// saveSamples writes day-granularity observations into timeline_series /
// timeline_points. Importing the same data twice is idempotent (upsert by series +
// day).
func (s *Service) saveSamples(ctx context.Context, file FileInput, samples []SeriesSample) error {
	source := file.Parent
	if source == "" {
		source = "data"
	}

	type group struct {
		label  string
		unit   string
		points []SeriesSample
	}
	groups := make(map[string]*group)
	order := make([]string, 0, 4)
	for _, sample := range samples {
		if sample.Metric == "" {
			continue
		}
		item, ok := groups[sample.Metric]
		if !ok {
			item = &group{label: sample.Label, unit: sample.Unit}
			if item.label == "" {
				item.label = sample.Metric
			}
			groups[sample.Metric] = item
			order = append(order, sample.Metric)
		}
		item.points = append(item.points, sample)
	}

	for _, metric := range order {
		item := groups[metric]
		var seriesID string
		err := s.db.Pool().QueryRow(ctx,
			`insert into timeline_series (user_id, source, metric, label, unit)
			 values ($1::uuid, $2, $3, $4, $5)
			 on conflict (user_id, source, metric) do update set
			   label = excluded.label,
			   unit = excluded.unit
			 returning id::text`,
			file.UserID, source, metric, item.label, item.unit).Scan(&seriesID)
		if err != nil {
			return err
		}

		byDay := make(map[string]SeriesSample, len(item.points))
		for _, point := range item.points {
			byDay[point.Date.Format("2006-01-02")] = point
		}
		days := make([]time.Time, 0, len(byDay))
		values := make([]float64, 0, len(byDay))
		for _, point := range byDay {
			days = append(days, s.engine.localDate(point.Date))
			values = append(values, point.Value)
		}
		if _, err := s.db.Pool().Exec(ctx,
			`insert into timeline_points (series_id, local_date, value)
			 select $1::uuid, day, value
			   from unnest($2::date[], $3::double precision[]) as t(day, value)
			 on conflict (series_id, local_date) do update set value = excluded.value`,
			seriesID, days, values); err != nil {
			return err
		}
	}
	return nil
}

func sleep(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
