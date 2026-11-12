package timeline

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/events"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

// New builds the stream service. With Enabled=false the API stays readable, but the
// organizer never starts.
//
// eventPublisher is the write path for events (internal/events). The pipeline never
// writes those tables itself: validation, deduplication and media resolution live in
// that package; this one only decides which segments are published.
func New(db *store.DB, objectStorage storage.Storage, cfg config.Timeline, eventPublisher *events.Service) *Service {
	return &Service{
		db:      db,
		storage: objectStorage,
		engine:  NewEngine(cfg),
		cfg:     cfg,
		events:  eventPublisher,
	}
}

// Notify is called after a legacy write path changes files. It queues only a scan,
// which handles files modified in the last three days. The organizer's real trigger
// is the durable change feed (see changes.go): the replacement file API settles a
// change fact in the index transaction and never calls this. Missing history is
// queued as backfill at startup and after a feed gap, so an upload never triggers a
// whole-library "did we miss a file" query.
func (s *Service) Notify(userID string) {
	if !s.cfg.Enabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.enqueueScan(ctx, userID); err != nil {
		slog.Warn("timeline: failed to enqueue scan", "user", userID, "error", err)
	}
}

// Status reports how much organizing is still in flight.
func (s *Service) Status(ctx context.Context, userID string) (Status, error) {
	return s.status(ctx, userID)
}

// Feed reads a few months back from before. Only months with content are returned;
// the gaps in between are the frontend's to express.
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
		// Not organized yet: return a month header plus the file count, and the frontend
		// shows a skeleton / in-progress state.
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

	s.hydrateMonth(snapshot)
	return snapshot, nil
}

func (s *Service) hydrateMonth(month *Month) {
	for i := range month.Items {
		item := &month.Items[i]
		hydrateThumb(item.Cover)
		for j := range item.Grid {
			hydrateThumb(&item.Grid[j])
		}
	}
}

// Rebuild is the explicit full rebuild for users and developers: drop this user's
// analysis results, jump the change checkpoint to the feed head (the backfill below
// covers everything up to it by scanning the tree) and start over. Without the jump
// the retained feed would be replayed on top of the full scan. The normal path never
// needs this; it costs O(file count), and it is also the recovery path when the feed
// no longer retains the checkpoint.
func (s *Service) Rebuild(ctx context.Context, userID string) error {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Void any running backfill so it cannot park an old cursor halfway through.
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
	// The scan below owns history up to this position. A concurrent changes job may
	// still save an older position, so the checkpoint only ever moves forward.
	if _, err := tx.Exec(ctx,
		`insert into timeline_change_positions (owner_id, position, updated_at)
		 select owner_id, head_position, now() from file_change_heads where owner_id = $1::uuid
		 on conflict (owner_id) do update
		    set position = greatest(timeline_change_positions.position, excluded.position),
		        updated_at = now()`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`insert into timeline_jobs (user_id, kind) values ($1::uuid, 'backfill')
		 on conflict do nothing`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------- month snapshot build (called by the worker) ----------

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

	// The selected segments become events — the only thing the stream actually reads
	// (since v0.4). Re-running the same photos creates no duplicates: producer_key is
	// unique, so a conflict only updates.
	if err := s.events.PublishAuto(ctx, userID, autoEvents(selected, assets, s.engine.Location)); err != nil {
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
	// The month snapshot is v0.3 legacy. The frontend reads events now; the write stays
	// for the moment and can go once nothing reads it (see sql/013_events.sql).
	slog.Debug("timeline: month rebuilt",
		"user", userID, "month", month.Format("2006-01"),
		"files", fileCount, "candidates", len(candidates), "selected", len(selected))
	return nil
}

// autoEvents maps the selected segments into event submissions.
//
// Tradeoffs:
//   - At most 4 media (the photo type's cap). The 9-cell grid belongs to a future
//     type; do not loosen the constraint for one layout.
//   - The title is the one the Narrator produced; the subtitle goes into meta (a
//     rendered date plus a count).
//   - The body stays empty: heuristics cannot write a sentence, and nothing beats no
//     filler.
func autoEvents(selected []Moment, assets []Asset, loc *time.Location) []events.AutoInput {
	byPath := make(map[string]Asset, len(assets))
	byID := make(map[string]Asset, len(assets))
	for _, asset := range assets {
		byPath[asset.Path] = asset
		byID[asset.NodeID] = asset
	}
	out := make([]events.AutoInput, 0, len(selected))
	for _, moment := range selected {
		item := momentItem(moment, byID)
		thumbs := item.Grid
		if len(thumbs) == 0 && item.Cover != nil {
			thumbs = []Thumb{*item.Cover}
		}
		media := make([]events.AutoMedia, 0, len(thumbs))
		for _, thumb := range thumbs {
			asset, ok := byPath[thumb.Path]
			if !ok {
				continue
			}
			media = append(media, events.AutoMedia{
				NodeID: asset.NodeID,
				Path:   asset.Path,
				Video:  asset.MediaType == "video",
			})
			if len(media) == 4 {
				break
			}
		}
		if len(media) == 0 {
			continue
		}
		endsAt := moment.EndsAt
		out = append(out, events.AutoInput{
			ProducerKey: moment.DedupeKey,
			StartsAt:    moment.StartsAt,
			EndsAt:      &endsAt,
			TZ:          loc.String(),
			Precision:   "minute",
			Title:       moment.Title,
			Meta:        moment.Subtitle,
			Media:       media,
		})
	}
	return out
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

		// A multi-day moment also marks the days in between as covered, so no extra gap
		// row gets inserted.
		for day := moment.LocalDate; ; day = day.AddDate(0, 0, 1) {
			covered[day.Format("2006-01-02")] = true
			if !day.Before(momentEndDay(*moment)) {
				break
			}
		}
	}

	// Days with files that no segment covers accumulate into one collapsed gap row.
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

	// Cover first; the rest sort photos ahead of videos so the grid's first screen is
	// not all video placeholders.
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
