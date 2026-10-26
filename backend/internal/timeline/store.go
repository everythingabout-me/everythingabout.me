package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

// Service 是内容流的读写入口。后台整理（worker）和 HTTP 读取共用它。
type Service struct {
	db      *store.DB
	storage storage.Storage
	engine  *Engine
	cfg     config.Timeline
	started atomic.Bool
}

type job struct {
	ID       int64
	UserID   string
	Kind     string
	Month    string
	Cursor   string
	Attempts int
}

// ---------- 文件事实 ----------

func (s *Service) recentFiles(ctx context.Context, userID string, limit int) ([]FileInput, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select n.id::text, n.owner_id::text, n.path, n.name, n.parent, n.size,
		        coalesce(n.mime, ''),
		        coalesce(n.source_modified_at, n.updated_at, now()),
		        n.updated_at, n.created_at, coalesce(n.etag, '')
		   from nodes n
		   left join timeline_assets a on a.user_id = n.owner_id and a.node_id = n.id
		  where n.owner_id = $1::uuid
		    and n.is_dir = false
		    and n.updated_at > now() - interval '3 days'
		    and (a.node_id is null
		         or a.source_sig is distinct from
		            (coalesce(n.etag, '') || ':' || n.size::text || ':' ||
		             floor(extract(epoch from n.updated_at))::bigint::text || ':' || coalesce(n.mime, ''))
		         or a.pipeline_version < $3)
		  order by n.updated_at desc
		  limit $2`, userID, limit, pipelineVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFileInputs(rows)
}

func (s *Service) backfillFiles(
	ctx context.Context,
	userID, afterID string,
	limit int,
) ([]FileInput, string, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select n.id::text, n.owner_id::text, n.path, n.name, n.parent, n.size,
		        coalesce(n.mime, ''),
		        coalesce(n.source_modified_at, n.updated_at, now()),
		        n.updated_at, n.created_at, coalesce(n.etag, '')
		   from nodes n
		  where n.owner_id = $1::uuid
		    and n.is_dir = false
		    and n.id > coalesce(nullif($2::text, '')::uuid,
		                        '00000000-0000-0000-0000-000000000000'::uuid)
		  order by n.id
		  limit $3`, userID, afterID, limit)
	if err != nil {
		return nil, afterID, err
	}
	defer rows.Close()
	files, err := scanFileInputs(rows)
	if err != nil {
		return nil, afterID, err
	}
	if len(files) > 0 {
		afterID = files[len(files)-1].NodeID
	}
	return files, afterID, nil
}

func scanFileInputs(rows pgx.Rows) ([]FileInput, error) {
	files := make([]FileInput, 0, 64)
	for rows.Next() {
		var file FileInput
		if err := rows.Scan(
			&file.NodeID, &file.UserID, &file.Path, &file.Name, &file.Parent, &file.Size,
			&file.Mime, &file.SourceModified, &file.UpdatedAt, &file.CreatedAt, &file.ETag,
		); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// upsertFacts 写入一个文件事实。内容签名没变时返回 false，避免无谓的月重算。
func (s *Service) upsertFacts(
	ctx context.Context,
	file FileInput,
	facts Facts,
	localDate time.Time,
	ambient bool,
) (bool, error) {
	signature := sourceSignature(file)
	isNarrative := facts.Narrative && !ambient
	tag, err := s.db.Pool().Exec(ctx,
		`insert into timeline_assets
		   (user_id, node_id, event_time, local_date, time_source, time_confidence,
		    media_type, is_narrative, is_ambient, is_screenshot, quality, source_sig,
		    pipeline_version, analyzed_at)
		 values ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now())
		 on conflict (user_id, node_id) do update set
		   event_time = excluded.event_time,
		   local_date = excluded.local_date,
		   time_source = excluded.time_source,
		   time_confidence = excluded.time_confidence,
		   media_type = excluded.media_type,
		   is_narrative = excluded.is_narrative,
		   is_ambient = excluded.is_ambient,
		   is_screenshot = excluded.is_screenshot,
		   quality = excluded.quality,
		   source_sig = excluded.source_sig,
		   pipeline_version = excluded.pipeline_version,
		   analyzed_at = now()
		 where timeline_assets.source_sig <> excluded.source_sig
		    or timeline_assets.pipeline_version < excluded.pipeline_version`,
		file.UserID, file.NodeID, facts.EventTime, localDate, facts.TimeSource,
		facts.TimeConfidence, facts.MediaType, isNarrative, ambient, facts.Screenshot,
		facts.Quality, signature, pipelineVersion)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func sourceSignature(file FileInput) string {
	return fmt.Sprintf("%s:%d:%d:%s", file.ETag, file.Size, file.UpdatedAt.Unix(), file.Mime)
}

// ---------- 队列 ----------

func (s *Service) enqueueScan(ctx context.Context, userID string) error {
	_, err := s.db.Pool().Exec(ctx,
		`insert into timeline_jobs (user_id, kind) values ($1::uuid, 'scan')
		 on conflict do nothing`, userID)
	return err
}

func (s *Service) enqueueMonth(ctx context.Context, userID string, month time.Time) error {
	_, err := s.db.Pool().Exec(ctx,
		`insert into timeline_jobs (user_id, kind, month) values ($1::uuid, 'month', $2)
		 on conflict do nothing`, userID, month)
	return err
}

// enqueueStaleMonths 对比快照和当前事实，把不一致的月份排进队列。
// 删除文件、换时区、改评分逻辑都能靠它收敛。
func (s *Service) enqueueStaleMonths(ctx context.Context, userID string) error {
	rows, err := s.db.Pool().Query(ctx,
		`select coalesce(s.month, a.month), a.files, a.rev
		   from (
		     select date_trunc('month', local_date)::date as month,
		            count(*)::int as files,
		            max(analyzed_at) as rev
		       from timeline_assets
		      where user_id = $1::uuid
		      group by 1
		   ) a
		   full join timeline_snapshots s
		     on s.user_id = $1::uuid and s.month = a.month
		  where a.month is null
		     or s.month is null
		     or s.file_count <> a.files
		     or s.source_rev is distinct from a.rev`, userID)
	if err != nil {
		return err
	}
	months := make([]time.Time, 0, 24)
	for rows.Next() {
		var month *time.Time
		var files *int
		var rev *time.Time
		if err := rows.Scan(&month, &files, &rev); err != nil {
			rows.Close()
			return err
		}
		if month != nil && files != nil {
			months = append(months, *month)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, month := range months {
		if err := s.enqueueMonth(ctx, userID, month); err != nil {
			return err
		}
	}

	// 事实已经清空的月份，快照也删掉，不要留一个空洞的旧月份。
	_, err = s.db.Pool().Exec(ctx,
		`delete from timeline_snapshots s
		  where s.user_id = $1::uuid
		    and not exists (
		      select 1 from timeline_assets a
		       where a.user_id = $1::uuid
		         and date_trunc('month', a.local_date)::date = s.month
		    )`, userID)
	return err
}

func (s *Service) claimJob(ctx context.Context) (*job, error) {
	var item job
	err := s.db.Pool().QueryRow(ctx,
		`update timeline_jobs j
		    set status = 'running',
		        lease_until = now() + interval '30 minutes',
		        attempts = j.attempts + 1
		  where j.id = (
		    select id
		      from timeline_jobs
		     where (status = 'pending' and available_at <= now())
		        or (status = 'running' and lease_until < now())
		     order by id
		     limit 1
		     for update skip locked
		  )
		  returning j.id, j.user_id::text, j.kind,
		            coalesce(to_char(j.month, 'YYYY-MM-DD'), ''),
		            coalesce(j.cursor, ''), j.attempts`).
		Scan(&item.ID, &item.UserID, &item.Kind, &item.Month, &item.Cursor, &item.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Service) finishJob(ctx context.Context, id int64) {
	if _, err := s.db.Pool().Exec(ctx,
		`update timeline_jobs
		    set status = 'done', lease_until = null, error = null, available_at = now()
		  where id = $1`, id); err != nil {
		slog.Warn("timeline: 标记任务完成失败", "job", id, "error", err)
	}
}

func (s *Service) failJob(ctx context.Context, item *job, cause error) {
	status := "pending"
	if item.Attempts >= 5 {
		status = "failed"
	}
	backoff := 30 * time.Second
	if item.Attempts > 1 {
		backoff = time.Duration(1<<min(item.Attempts, 6)) * 30 * time.Second
	}
	_, err := s.db.Pool().Exec(ctx,
		`update timeline_jobs
		    set status = $2,
		        lease_until = null,
		        error = $3,
		        available_at = now() + make_interval(secs => $4)
		  where id = $1`, item.ID, status, cause.Error(), int(backoff.Seconds()))
	if err != nil {
		slog.Warn("timeline: 标记任务失败失败", "job", item.ID, "error", err)
	}
}

func (s *Service) saveCursor(ctx context.Context, id int64, cursor string) {
	_, err := s.db.Pool().Exec(ctx,
		`update timeline_jobs
		    set cursor = $2, lease_until = now() + interval '30 minutes'
		  where id = $1`, id, cursor)
	if err != nil {
		slog.Warn("timeline: 保存回填游标失败", "job", id, "error", err)
	}
}

// ---------- 月份读取 ----------

func (s *Service) monthAssets(
	ctx context.Context,
	userID string,
	start, end time.Time,
) ([]Asset, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select a.node_id::text, n.path, n.name, n.parent, n.size, a.media_type,
		        a.event_time, a.local_date, a.quality, a.is_narrative, a.is_ambient, a.is_screenshot
		   from timeline_assets a
		   join nodes n on n.id = a.node_id
		  where a.user_id = $1::uuid
		    and a.local_date >= $2 and a.local_date < $3
		  order by a.event_time`, userID, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	assets := make([]Asset, 0, 256)
	for rows.Next() {
		var asset Asset
		if err := rows.Scan(
			&asset.NodeID, &asset.Path, &asset.Name, &asset.Parent, &asset.Size,
			&asset.MediaType, &asset.EventTime, &asset.LocalDate, &asset.Quality,
			&asset.Narrative, &asset.Ambient, &asset.Screenshot,
		); err != nil {
			return nil, err
		}
		asset.LocalDate = time.Date(asset.LocalDate.Year(), asset.LocalDate.Month(),
			asset.LocalDate.Day(), 0, 0, 0, 0, s.engine.Location)
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func (s *Service) monthStats(
	ctx context.Context,
	userID string,
	start, end time.Time,
) (int, time.Time, error) {
	var count int
	var rev *time.Time
	if err := s.db.Pool().QueryRow(ctx,
		`select count(*)::int, max(analyzed_at)
		   from timeline_assets
		  where user_id = $1::uuid and local_date >= $2 and local_date < $3`,
		userID, start, end).Scan(&count, &rev); err != nil {
		return 0, time.Time{}, err
	}
	if rev == nil {
		return count, time.Now(), nil
	}
	return count, *rev, nil
}

// loadSeries 返回这个月有数据的环境序列。最多取 3 条，避免边栏变图表墙。
func (s *Service) loadSeries(
	ctx context.Context,
	userID string,
	start, end time.Time,
) ([]Series, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select s.metric, s.label, s.unit, p.local_date, p.value
		   from timeline_series s
		   join timeline_points p on p.series_id = s.id
		  where s.user_id = $1::uuid
		    and s.pinned = true
		    and p.local_date >= $2 and p.local_date < $3
		  order by s.metric, p.local_date`, userID, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byMetric := make(map[string]*Series)
	order := make([]string, 0, 4)
	for rows.Next() {
		var metric, label, unit string
		var day time.Time
		var value float64
		if err := rows.Scan(&metric, &label, &unit, &day, &value); err != nil {
			return nil, err
		}
		item, ok := byMetric[metric]
		if !ok {
			item = &Series{
				Metric: metric,
				Label:  label,
				Unit:   unit,
				Min:    value,
				Max:    value,
			}
			if item.Label == "" {
				item.Label = metric
			}
			byMetric[metric] = item
			order = append(order, metric)
		}
		item.Days = append(item.Days, day.Day())
		item.Values = append(item.Values, value)
		item.Latest = value
		if value < item.Min {
			item.Min = value
		}
		if value > item.Max {
			item.Max = value
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	series := make([]Series, 0, len(order))
	for _, metric := range order {
		series = append(series, *byMetric[metric])
	}
	if len(series) > 3 {
		series = series[:3]
	}
	return series, nil
}

// ---------- 快照 ----------

func (s *Service) readSnapshot(
	ctx context.Context,
	userID string,
	month time.Time,
) (*Month, error) {
	var payload []byte
	err := s.db.Pool().QueryRow(ctx,
		`select payload from timeline_snapshots where user_id = $1::uuid and month = $2`,
		userID, month).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot Month
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (s *Service) saveSnapshot(
	ctx context.Context,
	userID string,
	month time.Time,
	snapshot Month,
	fileCount int,
	sourceRev time.Time,
) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = s.db.Pool().Exec(ctx,
		`insert into timeline_snapshots
		   (user_id, month, payload, coverage, file_count, moment_count,
		    source_rev, pipeline_version, generated_at)
		 values ($1::uuid, $2, $3::jsonb, $4, $5, $6, $7, $8, now())
		 on conflict (user_id, month) do update set
		   payload = excluded.payload,
		   coverage = excluded.coverage,
		   file_count = excluded.file_count,
		   moment_count = excluded.moment_count,
		   source_rev = excluded.source_rev,
		   pipeline_version = excluded.pipeline_version,
		   generated_at = now()`,
		userID, month, string(payload), snapshot.Coverage, fileCount, snapshot.MomentCount,
		sourceRev, pipelineVersion)
	return err
}

func (s *Service) deleteSnapshot(ctx context.Context, userID string, month time.Time) error {
	_, err := s.db.Pool().Exec(ctx,
		`delete from timeline_snapshots where user_id = $1::uuid and month = $2`, userID, month)
	return err
}

// ---------- moment 写入 ----------

func (s *Service) persistMoments(
	ctx context.Context,
	userID string,
	monthStart, monthEnd time.Time,
	moments []Moment,
) error {
	keys := make([]string, 0, len(moments))
	for i := range moments {
		moment := &moments[i]
		keys = append(keys, moment.DedupeKey)

		var reason []byte
		if moment.Reason != nil {
			reason, _ = json.Marshal(moment.Reason)
		} else {
			reason = []byte("{}")
		}

		var id string
		err := s.db.Pool().QueryRow(ctx,
			`insert into timeline_moments
			   (user_id, dedupe_key, kind, starts_at, ends_at, local_date, title, subtitle,
			    cover_node_id, item_count, source_kinds, score, cost, layout, reason,
			    state, pipeline_version)
			 values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, nullif($9, '')::uuid, $10,
			         $11::text[], $12, $13, $14, $15::jsonb, 'published', $16)
			 on conflict (user_id, dedupe_key) do update set
			   kind = excluded.kind,
			   starts_at = excluded.starts_at,
			   ends_at = excluded.ends_at,
			   local_date = excluded.local_date,
			   title = excluded.title,
			   subtitle = excluded.subtitle,
			   cover_node_id = excluded.cover_node_id,
			   item_count = excluded.item_count,
			   source_kinds = excluded.source_kinds,
			   score = excluded.score,
			   cost = excluded.cost,
			   layout = excluded.layout,
			   reason = excluded.reason,
			   state = 'published',
			   pipeline_version = excluded.pipeline_version,
			   updated_at = now()
			 where timeline_moments.state <> 'user_edited'
			 returning id::text`,
			userID, moment.DedupeKey, moment.Kind, moment.StartsAt, moment.EndsAt,
			moment.LocalDate, moment.Title, moment.Subtitle, moment.CoverNodeID,
			moment.ItemCount, moment.SourceKinds, moment.Score, moment.Cost,
			moment.Layout, string(reason), pipelineVersion).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// 用户改过的片段不覆盖，也不重建它的明细。
			continue
		}
		if err != nil {
			return err
		}
		moment.ID = id

		if _, err := s.db.Pool().Exec(ctx,
			`delete from timeline_moment_items where moment_id = $1::uuid`, id); err != nil {
			return err
		}
		if len(moment.ItemIDs) > 0 {
			if _, err := s.db.Pool().Exec(ctx,
				`insert into timeline_moment_items (moment_id, node_id, rank, role)
				 select $1::uuid, item, ord - 1,
				        case when ord = 1 then 'cover' else 'grid' end
				   from unnest($2::uuid[]) with ordinality as t(item, ord)`,
				id, moment.ItemIDs); err != nil {
				return err
			}
		}
	}

	// 这个月不再出现的旧片段隐藏起来，保留深链和用户操作。
	_, err := s.db.Pool().Exec(ctx,
		`update timeline_moments
		    set state = 'hidden', updated_at = now()
		  where user_id = $1::uuid
		    and local_date >= $2 and local_date < $3
		    and state = 'published'
		    and dedupe_key <> all($4::text[])`,
		userID, monthStart, monthEnd, keys)
	return err
}

func (s *Service) deleteMonthMoments(
	ctx context.Context,
	userID string,
	monthStart, monthEnd time.Time,
) error {
	_, err := s.db.Pool().Exec(ctx,
		`delete from timeline_moments
		  where user_id = $1::uuid
		    and local_date >= $2 and local_date < $3
		    and state <> 'user_edited'`, userID, monthStart, monthEnd)
	return err
}

// ---------- 状态 ----------

func (s *Service) status(ctx context.Context, userID string) (Status, error) {
	status := Status{Enabled: s.cfg.Enabled}
	if err := s.db.Pool().QueryRow(ctx,
		`select count(*)::int from timeline_jobs
		  where user_id = $1::uuid and status in ('pending', 'running')`,
		userID).Scan(&status.Pending); err != nil {
		return status, err
	}
	if err := s.db.Pool().QueryRow(ctx,
		`select count(*)::int from timeline_assets where user_id = $1::uuid`,
		userID).Scan(&status.IndexedFiles); err != nil {
		return status, err
	}
	var earliest, latest *time.Time
	if err := s.db.Pool().QueryRow(ctx,
		`select min(local_date), max(local_date) from timeline_assets where user_id = $1::uuid`,
		userID).Scan(&earliest, &latest); err != nil {
		return status, err
	}
	if earliest != nil {
		status.EarliestMonth = earliest.Format("2006-01")
	}
	if latest != nil {
		status.LatestMonth = latest.Format("2006-01")
	}
	var generated *time.Time
	if err := s.db.Pool().QueryRow(ctx,
		`select max(generated_at) from timeline_snapshots where user_id = $1::uuid`,
		userID).Scan(&generated); err != nil {
		return status, err
	}
	if generated != nil {
		status.LastGenerated = generated.UTC().Format(time.RFC3339)
	}
	return status, nil
}

// ---------- 签名 ----------

func (s *Service) signThumb(ctx context.Context, userID string, thumb *Thumb, process string) {
	if thumb == nil || thumb.Video || thumb.Path == "" {
		return
	}
	key := paths.KeyFor(thumb.Path, userID)
	url, err := s.storage.GetURL(ctx, key, storage.URLOptions{
		ExpiresSeconds: 900,
		Process:        process,
	})
	if err != nil {
		slog.Warn("timeline: 生成缩略图地址失败", "path", thumb.Path, "error", err)
		return
	}
	thumb.URL = url
}

// ---------- 月份工具 ----------

func monthStartOf(value time.Time, loc *time.Location) time.Time {
	local := value.In(loc)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
}

func parseMonth(value string, loc *time.Location) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, loc); err == nil {
			return time.Date(parsed.Year(), parsed.Month(), 1, 0, 0, 0, 0, loc), true
		}
	}
	return time.Time{}, false
}

func monthEnd(month time.Time) time.Time {
	return month.AddDate(0, 1, 0)
}
