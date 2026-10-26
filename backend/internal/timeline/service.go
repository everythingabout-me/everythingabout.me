package timeline

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

// New 构造内容流服务。Enabled=false 时接口仍可读，但不会启动整理器。
func New(db *store.DB, objectStorage storage.Storage, cfg config.Timeline) *Service {
	return &Service{
		db:      db,
		storage: objectStorage,
		engine:  NewEngine(cfg),
		cfg:     cfg,
	}
}

// Notify 在文件变动后调用。只排一个 scan：它处理最近三天变化的文件。
// 缺失历史数据由启动时和每 20 分钟的 sweeper 排 backfill，避免每次上传都做一次
// "整个库有没有漏文件"的重查询。
func (s *Service) Notify(userID string) {
	if !s.cfg.Enabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.enqueueScan(ctx, userID); err != nil {
		slog.Warn("timeline: 排 scan 失败", "user", userID, "error", err)
	}
}

// Status 返回"还有多少在整理"。
func (s *Service) Status(ctx context.Context, userID string) (Status, error) {
	return s.status(ctx, userID)
}

// Feed 读取从 before 往前的若干个月。只返回"有内容"的月份，中间的空档交给前端表达。
func (s *Service) Feed(ctx context.Context, userID string, before string, months int) (*Feed, error) {
	status, err := s.status(ctx, userID)
	if err != nil {
		return nil, err
	}
	feed := &Feed{Status: status}

	loc := s.engine.Location
	start := monthStartOf(time.Now(), loc)
	if parsed, ok := parseMonth(before, loc); ok {
		start = parsed
	}
	if months <= 0 {
		months = 2
	}
	if months > 6 {
		months = 6
	}

	rows, err := s.db.Pool().Query(ctx,
		`with months as (
		   select distinct date_trunc('month', local_date)::date as month
		     from timeline_assets where user_id = $1::uuid
		   union
		   select month from timeline_snapshots where user_id = $1::uuid
		 )
		 select month from months where month <= $2 order by month desc limit $3`,
		userID, start, months+1)
	if err != nil {
		return nil, err
	}
	var active []time.Time
	for rows.Next() {
		var month time.Time
		if err := rows.Scan(&month); err != nil {
			rows.Close()
			return nil, err
		}
		active = append(active, month)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(active) > months {
		feed.Next = active[months].Format("2006-01")
		active = active[:months]
	}

	for _, month := range active {
		period, err := s.month(ctx, userID, month)
		if err != nil {
			return nil, err
		}
		if period != nil {
			feed.Periods = append(feed.Periods, *period)
		}
	}
	if feed.Periods == nil {
		feed.Periods = []Month{}
	}
	return feed, nil
}

func (s *Service) month(ctx context.Context, userID string, month time.Time) (*Month, error) {
	snapshot, err := s.readSnapshot(ctx, userID, month)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		// 还没轮到整理：先给一个月标题 + 文件数，前端显示骨架/整理中。
		count, _, err := s.monthStats(ctx, userID, month, monthEnd(month))
		if err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, nil
		}
		period := &Month{
			Month:     month.Format("2006-01"),
			FileCount: count,
			Coverage:  "indexing",
			Items: []Item{{
				Type:      "gap",
				From:      month.Format("2006-01-02"),
				To:        monthEnd(month).AddDate(0, 0, -1).Format("2006-01-02"),
				ItemCount: count,
				Label:     "正在整理",
			}},
		}
		return period, nil
	}

	s.signMonth(ctx, userID, snapshot)
	return snapshot, nil
}

func (s *Service) signMonth(ctx context.Context, userID string, month *Month) {
	for i := range month.Items {
		item := &month.Items[i]
		process := "imageMogr2/thumbnail/640x>"
		if item.Layout == "hero" || item.Layout == "single" {
			process = "imageMogr2/thumbnail/1440x>"
		}
		if item.Cover != nil {
			s.signThumb(ctx, userID, item.Cover, process)
		}
		for j := range item.Grid {
			s.signThumb(ctx, userID, &item.Grid[j], process)
		}
	}
}

// Rebuild 是给用户/开发用的强制重算：丢掉这个用户的分析结果，从头再来。
// 正常路径不需要它；它的代价是 O(文件数)。
func (s *Service) Rebuild(ctx context.Context, userID string) error {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 正在跑的回填直接作废，避免它把旧游标停在半路。
	if _, err := tx.Exec(ctx,
		`update timeline_jobs
		    set status = 'done', lease_until = null, error = 'superseded by rebuild'
		  where user_id = $1::uuid and kind = 'backfill' and status in ('pending', 'running')`,
		userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`delete from timeline_assets where user_id = $1::uuid`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`insert into timeline_jobs (user_id, kind) values ($1::uuid, 'backfill')
		 on conflict do nothing`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------- 月快照构建（worker 调用） ----------

func (s *Service) rebuildMonth(ctx context.Context, userID string, month time.Time) error {
	start := month
	end := monthEnd(month)

	assets, err := s.monthAssets(ctx, userID, start, end)
	if err != nil {
		return err
	}
	if len(assets) == 0 {
		if err := s.deleteMonthMoments(ctx, userID, start, end); err != nil {
			return err
		}
		return s.deleteSnapshot(ctx, userID, month)
	}

	candidates, err := s.engine.Grouper.Group(ctx, assets)
	if err != nil {
		return err
	}
	for i := range candidates {
		if err := s.engine.Scorer.Score(ctx, &candidates[i]); err != nil {
			return err
		}
		if err := s.engine.Narrator.Narrate(ctx, &candidates[i]); err != nil {
			return err
		}
	}

	series, err := s.loadSeries(ctx, userID, start, end)
	if err != nil {
		return err
	}
	ambientCost := 0
	if len(series) > 0 {
		ambientCost = 2
	}

	selected := selectMoments(candidates, s.engine.Budget, ambientCost)
	if err := s.persistMoments(ctx, userID, start, end, selected); err != nil {
		return err
	}

	fileCount, sourceRev, err := s.monthStats(ctx, userID, start, end)
	if err != nil {
		return err
	}
	payload := s.buildPayload(month, assets, selected, series, fileCount)
	if err := s.saveSnapshot(ctx, userID, month, payload, fileCount, sourceRev); err != nil {
		return err
	}
	slog.Debug("timeline: 月份已整理",
		"user", userID, "month", month.Format("2006-01"),
		"files", fileCount, "candidates", len(candidates), "selected", len(selected))
	return nil
}

func (s *Service) buildPayload(
	month time.Time,
	assets []Asset,
	selected []Moment,
	series []Series,
	fileCount int,
) Month {
	assetByID := make(map[string]Asset, len(assets))
	dayCounts := make(map[string]int)
	for _, asset := range assets {
		assetByID[asset.NodeID] = asset
		dayCounts[asset.LocalDate.Format("2006-01-02")]++
	}

	items := make([]Item, 0, len(selected)+3)
	if len(series) > 0 {
		items = append(items, Item{Type: "ambient", Series: series})
	}

	covered := make(map[string]bool)
	for i := range selected {
		moment := &selected[i]
		item := momentItem(*moment, assetByID)
		items = append(items, item)

		// 跨天事件把中间的日期也标成"有内容"，避免再插一条空白。
		for day := moment.LocalDate; ; day = day.AddDate(0, 0, 1) {
			covered[day.Format("2006-01-02")] = true
			if !day.Before(momentEndDay(*moment)) {
				break
			}
		}
	}

	// 没被任何片段覆盖、且确实有文件的日子，攒成一条压缩空白行。
	monthFirst := month
	monthLast := monthEnd(month).AddDate(0, 0, -1)
	runStart := time.Time{}
	runCount := 0
	flushGap := func(end time.Time) {
		if runStart.IsZero() || runCount == 0 {
			runStart = time.Time{}
			runCount = 0
			return
		}
		days := int(end.Sub(runStart).Hours()/24) + 1
		if days >= 7 || len(selected) == 0 {
			if gap := gapItem(runStart, end, runCount); gap != nil {
				items = append(items, *gap)
			} else if len(selected) == 0 {
				items = append(items, Item{
					Type:      "gap",
					From:      runStart.Format("2006-01-02"),
					To:        end.Format("2006-01-02"),
					ItemCount: runCount,
					Label:     "这个月",
				})
			}
		}
		runStart = time.Time{}
		runCount = 0
	}

	for day := monthFirst; !day.After(monthLast); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		count := dayCounts[key]
		if covered[key] || count == 0 {
			flushGap(day.AddDate(0, 0, -1))
			continue
		}
		if runStart.IsZero() {
			runStart = day
		}
		runCount += count
	}
	flushGap(monthLast)

	sort.SliceStable(items, func(i, j int) bool {
		return itemStart(items[i]).Before(itemStart(items[j]))
	})

	momentCount := 0
	for _, item := range items {
		if item.Type == "moment" {
			momentCount++
		}
	}
	return Month{
		Month:       month.Format("2006-01"),
		FileCount:   fileCount,
		MomentCount: momentCount,
		Coverage:    "ready",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Items:       items,
	}
}

func momentItem(moment Moment, assets map[string]Asset) Item {
	item := Item{
		Type:      "moment",
		ID:        moment.ID,
		Layout:    moment.Layout,
		Title:     moment.Title,
		Subtitle:  moment.Subtitle,
		Folder:    moment.Folder,
		StartsAt:  moment.StartsAt.UTC().Format(time.RFC3339),
		EndsAt:    moment.EndsAt.UTC().Format(time.RFC3339),
		ItemCount: moment.ItemCount,
		Score:     moment.Score,
	}

	// 封面优先；其余按"照片优先、视频靠后"排，避免九宫格第一屏全是视频占位。
	ordered := make([]Asset, 0, len(moment.ItemIDs))
	if cover, ok := assets[moment.CoverNodeID]; ok {
		ordered = append(ordered, cover)
	}
	for _, id := range moment.ItemIDs {
		if id == moment.CoverNodeID {
			continue
		}
		if asset, ok := assets[id]; ok {
			ordered = append(ordered, asset)
		}
	}
	if len(ordered) > 2 {
		sort.SliceStable(ordered[1:], func(i, j int) bool {
			a, b := ordered[1+i], ordered[1+j]
			if (a.MediaType == "photo") != (b.MediaType == "photo") {
				return a.MediaType == "photo"
			}
			return a.EventTime.Before(b.EventTime)
		})
	}

	limit := 9
	switch moment.Layout {
	case "single", "hero":
		limit = 1
	case "series":
		limit = 6
	}
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	thumbs := make([]Thumb, 0, len(ordered))
	for _, asset := range ordered {
		thumbs = append(thumbs, Thumb{
			Path:  asset.Path,
			Name:  asset.Name,
			Video: asset.MediaType == "video",
		})
	}
	if len(thumbs) > 0 {
		item.Cover = &thumbs[0]
	}
	if moment.Layout == "mosaic" || moment.Layout == "series" {
		item.Grid = thumbs
	}
	if more := moment.ItemCount - len(thumbs); more > 0 {
		item.More = more
	}
	return item
}

func momentEndDay(moment Moment) time.Time {
	end := moment.EndsAt.In(moment.LocalDate.Location())
	return time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, moment.LocalDate.Location())
}

func itemStart(item Item) time.Time {
	switch item.Type {
	case "moment":
		if parsed, err := time.Parse(time.RFC3339, item.StartsAt); err == nil {
			return parsed
		}
	case "gap":
		if parsed, err := time.Parse("2006-01-02", item.From); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
