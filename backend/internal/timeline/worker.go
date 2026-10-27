package timeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"everythingabout.me/backend/internal/paths"
)

const (
	// 单次处理多少个文件。太大会长时间占着事务/租约，太小会多跑几轮。
	fileBatch = 200
	// 健康 CSV 的读取上限。再大的文件不适合在后台随手读进内存。
	maxAmbientBytes = 8 << 20
)

// Start 启动后台整理器。单实例假设；多实例需要把租约换成心跳。
func (s *Service) Start(ctx context.Context) {
	if !s.cfg.Enabled {
		slog.Info("timeline: disabled")
		return
	}
	if !s.started.CompareAndSwap(false, true) {
		return
	}

	s.recoverJobs(ctx)
	for i := 0; i < s.cfg.Workers; i++ {
		go s.worker(ctx)
	}
	go s.bootstrap(ctx)
	go s.sweeper(ctx)
	slog.Info("timeline: started", "workers", s.cfg.Workers, "budget", s.engine.Budget)
}

func (s *Service) recoverJobs(ctx context.Context) {
	// 上个进程留下的 running 任务放回队列；单实例部署下这是安全的。
	if _, err := s.db.Pool().Exec(ctx,
		`update timeline_jobs
		    set status = 'pending', lease_until = null
		  where status = 'running'`); err != nil {
		slog.Warn("timeline: failed to recover interrupted jobs", "error", err)
	}
}

// bootstrap 启动时把还没整理过的用户排进队列。已有数据的库会从这里开始回填。
func (s *Service) bootstrap(ctx context.Context) {
	// 给 HTTP 服务一点启动时间，避免启动瞬间抢连接。
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
}

// sweeper 兜底扫描：CLI reconcile、外部 API 写入这类不经过 Notify 的变更，
// 靠"最近更新过的用户"补上。
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
			// 进程正在退出，把任务留在 running，下次启动会恢复。
			return
		}
		slog.Warn("timeline: job failed", "job", item.ID, "kind", item.Kind, "error", err)
		s.failJob(context.Background(), item, err)
		return
	}
	s.finishJob(context.Background(), item.ID)
}

func (s *Service) runScan(ctx context.Context, item *job) error {
	// 一次 scan 最多处理这么多轮，防止异常数据把 worker 卡死。
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

// processFile 把一个文件变成 timeline_assets 里的一行。
// 这是唯一会碰对象字节的地方（而且只对健康 CSV 这类小文件）。
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

// saveSamples 把日粒度观测写进 timeline_series / timeline_points。
// 同一份数据重复导入是幂等的（按 series + 日期 upsert）。
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
