// Package events 是时间线的读写模型。
//
// 一条事件 = 一段时间 + 一张卡（type + payload）+ 一个可选的详情链接。
// 设计见 docs/design/events.md，这里只记录实现层面的取舍：
//
//   - 卡片类型固定四种：photo / note / metric / html。type 决定 payload 的形状，
//     校验在 Validate 里，渲染在前端。没有"任意 HTML 卡片"，html 是其中一类，
//     而且前端用 sandbox iframe 渲染（见 web/src/pages/StreamPage.tsx）。
//   - 没有用户编辑。事件由生产者创建；生产者重推同一个 external_id 就是整体替换。
//     用户只能删除——删除写 state='deleted'（墓碑），而不是真删：生产者重跑、或者
//     应用重推同一个 external_id 时，要能认出"这条被用户删了"，否则会把它复活。
//     payload 会被清空，只留去重键。
//   - 时间线拥有事件，应用只是投稿：token 撤销、应用删除都不带走内容。
//   - 应用调 DELETE → state='detached'（内容保留，外链清空）
//   - 用户删除 → state='deleted'（内容清空，防止复活）
//   - 媒体只能引用盘里的文件（payload 里写路径，落库时解析成 node_id）。
//     不做外链图片：防盗链、失效、审核、SSRF 都是问题。
package events

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

// 卡片类型。
const (
	TypePhoto  = "photo"
	TypeNote   = "note"
	TypeMetric = "metric"
	TypeHTML   = "html"
)

// 来源。
const (
	OriginNative = "native"
	OriginAuto   = "auto"
	OriginApp    = "app"
)

// 状态。deleted 是墓碑：不出现在读路径，但保住去重键。
const (
	StatePublished = "published"
	StateDetached  = "detached"
	StateDeleted   = "deleted"
)

// 限额。改这里要同步 docs/design/events.md §6.4 和 GET /capabilities。
const (
	MaxTitle    = 80
	MaxBody     = 500
	MaxMeta     = 120
	MaxMetrics  = 3
	MaxMedia    = 4
	MaxHTMLLen  = 32 << 10
	MinHTMLSize = 40
	MaxHTMLSize = 640
	MaxURLLen   = 2000
)

// Metric 是一个数字槽位。
type Metric struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

// Media 是读路径上的媒体：path 是盘里的路径，url 是响应时预签名的地址。
type Media struct {
	Path  string `json:"path"`
	URL   string `json:"url,omitempty"`
	Name  string `json:"name,omitempty"`
	Video bool   `json:"video,omitempty"`
}

// Payload 是读路径上返回的卡片内容。写路径的 payload 结构见 PayloadIn。
type Payload struct {
	Body    string   `json:"body,omitempty"`
	Meta    string   `json:"meta,omitempty"`
	Media   []Media  `json:"media,omitempty"`
	Metrics []Metric `json:"metrics,omitempty"`
	HTML    string   `json:"html,omitempty"`
	Height  int      `json:"height,omitempty"`
}

// PayloadIn 是写路径的 payload（也是落库的形状）。和读路径唯一的区别是媒体存路径。
type PayloadIn struct {
	Body    string   `json:"body,omitempty"`
	Meta    string   `json:"meta,omitempty"`
	Media   []string `json:"media,omitempty"`
	Metrics []Metric `json:"metrics,omitempty"`
	HTML    string   `json:"html,omitempty"`
	Height  int      `json:"height,omitempty"`
}

// Source 是归因。native / auto 不显示名称；app 显示 token 名。
type Source struct {
	Kind         string `json:"kind"`
	Name         string `json:"name,omitempty"`
	Disconnected bool   `json:"disconnected,omitempty"`
}

// Event 是对外的事件形状。
type Event struct {
	ID        string  `json:"id"`
	Type      string  `json:"type"`
	Title     string  `json:"title"`
	StartsAt  string  `json:"starts_at"`
	EndsAt    string  `json:"ends_at,omitempty"`
	LocalDate string  `json:"local_date"`
	Precision string  `json:"precision"`
	DetailURL string  `json:"detail_url,omitempty"`
	Source    Source  `json:"source"`
	Payload   Payload `json:"payload"`
}

// AutoInput 是文件流水线投稿的一条事件。
type AutoInput struct {
	ProducerKey string
	StartsAt    time.Time
	EndsAt      *time.Time
	TZ          string
	Precision   string
	Title       string
	Body        string
	Meta        string
	Media       []AutoMedia
}

// AutoMedia 是流水线已经解析好的媒体。
type AutoMedia struct {
	NodeID string
	Path   string
	Video  bool
}

// AppInput 是三方应用投稿的一条事件。媒体是路径，落库时解析成 node。
type AppInput struct {
	ExternalID string
	DetailURL  string
	StartsAt   time.Time
	EndsAt     *time.Time
	TZ         string
	Precision  string
	Title      string
	Type       string
	Payload    PayloadIn
}

// Service 是事件的读写入口。
type Service struct {
	db      *store.DB
	storage storage.Storage
	nodes   *nodes.Service
}

func New(db *store.DB, objectStorage storage.Storage, nodeService *nodes.Service) *Service {
	return &Service{db: db, storage: objectStorage, nodes: nodeService}
}

// ---------- 读 ----------

// List 按时间倒序返回一页事件。cursor 是上一页最后一条的 opaque 游标。
func (s *Service) List(ctx context.Context, userID, cursor string, limit int) ([]Event, string, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}

	args := []any{userID}
	where := `e.user_id = $1::uuid and e.state = 'published'`
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", httpx.BadRequest("invalid cursor")
		}
		args = append(args, at, id)
		where += fmt.Sprintf(" and (e.starts_at, e.id) < ($%d::timestamptz, $%d::uuid)", len(args)-1, len(args))
	}
	args = append(args, limit+1)

	rows, err := s.db.Pool().Query(ctx,
		`select e.id::text, e.type, e.title, e.payload, coalesce(e.detail_url, ''),
		        e.starts_at, e.ends_at, e.local_date, e.precision, e.origin,
		        coalesce(t.name, ''), t.revoked_at is not null
		   from events e
		   left join api_tokens t on t.id = e.token_id
		  where `+where+`
		  order by e.starts_at desc, e.id desc
		  limit $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	list, err := scanEvents(rows)
	if err != nil {
		return nil, "", err
	}

	var next string
	if len(list) > limit {
		last := list[limit-1]
		list = list[:limit]
		next = encodeCursor(last.StartsAt, last.ID)
	}
	if err := s.attachMedia(ctx, userID, list); err != nil {
		return nil, "", err
	}
	return list, next, nil
}

// Get 读一条事件。只返回本人的、未被删除的。
func (s *Service) Get(ctx context.Context, userID, id string) (*Event, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select e.id::text, e.type, e.title, e.payload, coalesce(e.detail_url, ''),
		        e.starts_at, e.ends_at, e.local_date, e.precision, e.origin,
		        coalesce(t.name, ''), t.revoked_at is not null
		   from events e
		   left join api_tokens t on t.id = e.token_id
		  where e.user_id = $1::uuid and e.id = $2::uuid and e.state = 'published'`,
		userID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, httpx.NotFound("event not found")
	}
	if err := s.attachMedia(ctx, userID, list); err != nil {
		return nil, err
	}
	return &list[0], nil
}

// ListByToken 给应用对账：返回它自己写过的事件的外部 id。
func (s *Service) ListByToken(ctx context.Context, userID, tokenID string, from, to *time.Time) ([]Event, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select e.id::text, e.type, e.title, e.payload, coalesce(e.detail_url, ''),
		        e.starts_at, e.ends_at, e.local_date, e.precision, e.origin,
		        coalesce(t.name, ''), t.revoked_at is not null
		   from events e
		   left join api_tokens t on t.id = e.token_id
		  where e.user_id = $1::uuid and e.token_id = $2::uuid and e.state <> 'deleted'
		    and ($3::timestamptz is null or e.starts_at >= $3::timestamptz)
		    and ($4::timestamptz is null or e.starts_at < $4::timestamptz)
		  order by e.starts_at desc
		  limit 500`,
		userID, tokenID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Payload = Payload{} // 对账只要 id / external_id
	}
	return list, nil
}

func scanEvents(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]Event, error) {
	// 空页也要返回 []，不能是 nil：JSON 里 null 会让前端把列表当对象用。
	list := make([]Event, 0)
	for rows.Next() {
		var (
			ev           Event
			payload      []byte
			starts       time.Time
			ends         *time.Time
			localDate    time.Time
			token        string
			disconnected bool
		)
		if err := rows.Scan(&ev.ID, &ev.Type, &ev.Title, &payload, &ev.DetailURL,
			&starts, &ends, &localDate, &ev.Precision, &ev.Source.Kind, &token, &disconnected); err != nil {
			return nil, err
		}
		var stored PayloadIn
		if err := json.Unmarshal(payload, &stored); err != nil {
			return nil, fmt.Errorf("events: bad payload for %s: %w", ev.ID, err)
		}
		ev.StartsAt = starts.UTC().Format(time.RFC3339)
		ev.LocalDate = localDate.Format("2006-01-02")
		if ends != nil {
			ev.EndsAt = ends.UTC().Format(time.RFC3339)
		}
		if ev.Source.Kind == OriginApp {
			ev.Source.Name = token
			ev.Source.Disconnected = disconnected
		}
		ev.Payload = Payload{
			Body:    stored.Body,
			Meta:    stored.Meta,
			Metrics: stored.Metrics,
			HTML:    stored.HTML,
			Height:  stored.Height,
		}
		for _, p := range stored.Media {
			ev.Payload.Media = append(ev.Payload.Media, Media{Path: p})
		}
		list = append(list, ev)
	}
	return list, rows.Err()
}

// attachMedia 补齐媒体的 URL / 视频标记。一次查完一页，避免 N+1。
func (s *Service) attachMedia(ctx context.Context, userID string, list []Event) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, 0, len(list))
	for _, ev := range list {
		ids = append(ids, ev.ID)
	}

	rows, err := s.db.Pool().Query(ctx,
		`select em.event_id::text, em.idx, n.path, coalesce(n.name, ''), coalesce(n.mime, '')
		   from event_media em
		   join nodes n on n.id = em.node_id
		  where em.event_id = any($1::uuid[])
		  order by em.event_id, em.idx`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()

	type mediaRow struct {
		idx  int
		item Media
	}
	byEvent := map[string][]mediaRow{}
	for rows.Next() {
		var (
			eventID string
			idx     int
			path    string
			name    string
			mime    string
		)
		if err := rows.Scan(&eventID, &idx, &path, &name, &mime); err != nil {
			return err
		}
		byEvent[eventID] = append(byEvent[eventID], mediaRow{
			idx:  idx,
			item: Media{Path: path, Name: name, Video: strings.HasPrefix(mime, "video/")},
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range list {
		event := &list[i]
		media := byEvent[event.ID]
		if len(media) == 0 {
			// 文件删了：卡片退化成没有图，不报错。
			event.Payload.Media = nil
			continue
		}
		out := make([]Media, 0, len(media))
		for _, row := range media {
			item := row.item
			if !item.Video {
				key := paths.KeyFor(item.Path, userID)
				signed, err := s.storage.GetURL(ctx, key, storage.URLOptions{
					ExpiresSeconds: 900,
					Process:        "imageMogr2/thumbnail/1600x>",
				})
				if err != nil {
					slog.Warn("events: failed to sign media", "path", item.Path, "error", err)
				} else {
					item.URL = signed
				}
			}
			out = append(out, item)
		}
		event.Payload.Media = out
	}
	return nil
}

// ---------- 写：流水线 ----------

// PublishAuto 把一个月里选中的片段写成 origin='auto' 的事件。
// 同一个 producer_key 重跑只更新，不新增；用户删过的（墓碑）不会被复活。
func (s *Service) PublishAuto(ctx context.Context, userID string, items []AutoInput) error {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, item := range items {
		if item.ProducerKey == "" || item.Title == "" {
			continue
		}
		loc, err := time.LoadLocation(item.TZ)
		if err != nil {
			loc = time.Local
		}
		localDate := item.StartsAt.In(loc).Format("2006-01-02")
		payload := PayloadIn{Body: item.Body, Meta: item.Meta}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}

		var eventID string
		err = tx.QueryRow(ctx,
			`insert into events (user_id, origin, producer_key, starts_at, ends_at, tz, local_date,
			                     precision, type, title, payload, state)
			 values ($1::uuid, 'auto', $2, $3, $4, $5, $6::date, $7, 'photo', $8, $9::jsonb, 'published')
			 on conflict (user_id, producer_key) where producer_key is not null
			 do update set starts_at = excluded.starts_at, ends_at = excluded.ends_at,
			               tz = excluded.tz, local_date = excluded.local_date,
			               precision = excluded.precision, title = excluded.title,
			               payload = excluded.payload, updated_at = now()
			 where events.state = 'published'
			 returning id::text`,
			userID, item.ProducerKey, item.StartsAt, item.EndsAt, item.TZ, localDate,
			item.Precision, item.Title, raw).Scan(&eventID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// 墓碑：用户删过，不要再出现。
				continue
			}
			return err
		}

		if _, err := tx.Exec(ctx, `delete from event_media where event_id = $1::uuid`, eventID); err != nil {
			return err
		}
		for idx, media := range item.Media {
			if media.NodeID == "" {
				continue
			}
			if _, err := tx.Exec(ctx,
				`insert into event_media (event_id, idx, node_id) values ($1::uuid, $2, $3::uuid)`,
				eventID, idx, media.NodeID); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// ---------- 写：三方应用 ----------

// CreateApp 幂等创建 / 更新一条应用事件。
// created=false 表示覆盖了已有事件；410 表示这条被用户删了。
func (s *Service) CreateApp(ctx context.Context, userID, tokenID, prefix string, in AppInput) (*Event, bool, error) {
	if in.ExternalID == "" {
		return nil, false, httpx.BadRequest("external_id is required")
	}
	if err := ValidateApp(&in); err != nil {
		return nil, false, err
	}
	loc, err := time.LoadLocation(in.TZ)
	if err != nil {
		return nil, false, httpx.BadRequest("invalid tz")
	}
	localDate := in.StartsAt.In(loc).Format("2006-01-02")

	mediaIDs := make([]string, 0, len(in.Payload.Media))
	for _, path := range in.Payload.Media {
		if !paths.IsInside(path, prefix) {
			return nil, false, httpx.Forbidden("media must stay inside " + prefix)
		}
		row, err := s.nodes.Live(ctx, path, userID)
		if err != nil {
			return nil, false, err
		}
		if row == nil || row.IsDir {
			return nil, false, httpx.BadRequest("media not found: " + path)
		}
		mediaIDs = append(mediaIDs, row.ID)
	}

	raw, err := json.Marshal(in.Payload)
	if err != nil {
		return nil, false, err
	}

	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		eventID string
		created bool
	)
	err = tx.QueryRow(ctx,
		`insert into events (user_id, origin, token_id, external_id, starts_at, ends_at, tz,
		                     local_date, precision, type, title, detail_url, payload, state)
		 values ($1::uuid, 'app', $2::uuid, $3, $4, $5, $6, $7::date, $8, $9, $10, $11, $12::jsonb, 'published')
		 on conflict (user_id, token_id, external_id) where external_id is not null
		 do update set starts_at = excluded.starts_at, ends_at = excluded.ends_at, tz = excluded.tz,
		               local_date = excluded.local_date, precision = excluded.precision,
		               type = excluded.type, title = excluded.title, detail_url = excluded.detail_url,
		               payload = excluded.payload, state = 'published', updated_at = now()
		 where events.state <> 'deleted'
		 returning id::text, (xmax = 0)`,
		userID, tokenID, in.ExternalID, in.StartsAt, in.EndsAt, in.TZ, localDate,
		in.Precision, in.Type, in.Title, nullIfEmpty(in.DetailURL), raw).Scan(&eventID, &created)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, httpx.NewError(410, "event was deleted by the user")
		}
		return nil, false, err
	}

	if _, err := tx.Exec(ctx, `delete from event_media where event_id = $1::uuid`, eventID); err != nil {
		return nil, false, err
	}
	for idx, nodeID := range mediaIDs {
		if _, err := tx.Exec(ctx,
			`insert into event_media (event_id, idx, node_id) values ($1::uuid, $2, $3::uuid)`,
			eventID, idx, nodeID); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	ev, err := s.Get(ctx, userID, eventID)
	return ev, created, err
}

// DetachApp 是应用侧删除：保留内容，清掉外链。
func (s *Service) DetachApp(ctx context.Context, userID, tokenID, eventID string) error {
	tag, err := s.db.Pool().Exec(ctx,
		`update events set state = 'detached', detail_url = null, updated_at = now()
		  where user_id = $1::uuid and token_id = $2::uuid and id = $3::uuid
		    and state in ('published', 'detached')`, userID, tokenID, eventID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("event not found")
	}
	return nil
}

// DeleteOwned 是用户在网页上删除：墓碑 + 清空内容，防止流水线或应用复活。
func (s *Service) DeleteOwned(ctx context.Context, userID, eventID string) error {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`update events set state = 'deleted', title = '', payload = '{}'::jsonb,
		        detail_url = null, updated_at = now()
		  where user_id = $1::uuid and id = $2::uuid and state <> 'deleted'`, userID, eventID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("event not found")
	}
	if _, err := tx.Exec(ctx, `delete from event_media where event_id = $1::uuid`, eventID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------- 校验 ----------

// ValidateApp 校验三方投稿。所有限制集中在这里，改限额只改这一个地方。
// 指针：html 卡片没给高度时，这里补默认值，让落库的 payload 是完整的。
func ValidateApp(in *AppInput) error {
	if in.Title == "" || len([]rune(in.Title)) > MaxTitle {
		return httpx.BadRequest("title must be 1-80 characters")
	}
	if in.DetailURL != "" {
		u, err := url.Parse(in.DetailURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(in.DetailURL) > MaxURLLen {
			return httpx.BadRequest("detail_url must be an https URL")
		}
	}
	switch in.Precision {
	case "", "day", "minute":
	default:
		return httpx.BadRequest("precision must be day or minute")
	}
	if in.EndsAt != nil && in.EndsAt.Before(in.StartsAt) {
		return httpx.BadRequest("ends_at must be after starts_at")
	}

	p := &in.Payload
	if len([]rune(p.Body)) > MaxBody {
		return httpx.BadRequest("body is too long")
	}
	if len([]rune(p.Meta)) > MaxMeta {
		return httpx.BadRequest("meta is too long")
	}

	switch in.Type {
	case TypePhoto:
		if len(p.Media) == 0 || len(p.Media) > MaxMedia {
			return httpx.BadRequest("photo needs 1-4 media")
		}
		if len(p.Metrics) > 0 || p.HTML != "" {
			return httpx.BadRequest("photo does not take metrics or html")
		}
	case TypeNote:
		if strings.TrimSpace(p.Body) == "" {
			return httpx.BadRequest("note needs a body")
		}
		if len(p.Media) > 0 || len(p.Metrics) > 0 || p.HTML != "" {
			return httpx.BadRequest("note takes body only")
		}
	case TypeMetric:
		if len(p.Metrics) == 0 || len(p.Metrics) > MaxMetrics {
			return httpx.BadRequest("metric needs 1-3 metrics")
		}
		if len(p.Media) > 0 || p.HTML != "" {
			return httpx.BadRequest("metric does not take media or html")
		}
		for _, m := range p.Metrics {
			if m.Label == "" || m.Value == "" {
				return httpx.BadRequest("each metric needs a label and a value")
			}
		}
	case TypeHTML:
		if strings.TrimSpace(p.HTML) == "" || len(p.HTML) > MaxHTMLLen {
			return httpx.BadRequest("html card needs html under 32KB")
		}
		if p.Height == 0 {
			p.Height = 240
		}
		if p.Height < MinHTMLSize || p.Height > MaxHTMLSize {
			return httpx.BadRequest(fmt.Sprintf("html height must be %d-%d", MinHTMLSize, MaxHTMLSize))
		}
		if len(p.Media) > 0 || len(p.Metrics) > 0 {
			return httpx.BadRequest("html card carries its own content")
		}
	default:
		return httpx.BadRequest("type must be photo, note, metric or html")
	}
	return nil
}

// ---------- 游标 ----------

func encodeCursor(startsAt, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(startsAt + "|" + id))
}

func decodeCursor(cursor string) (string, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("bad cursor")
	}
	return parts[0], parts[1], nil
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
