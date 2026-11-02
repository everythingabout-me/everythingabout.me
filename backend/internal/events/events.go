// Package events is the stream's read/write model.
//
// One event = one time range + one card (type + payload) + an optional detail link.
// The design lives in docs/design/events.md; only implementation tradeoffs are
// recorded here:
//
//   - Exactly four card types: photo / note / metric / html. type fixes the payload
//     shape; validation lives in Validate, rendering in the frontend. There is no
//     "arbitrary HTML card" — html is one of the four, and the frontend renders it in a
//     sandbox iframe (see web/src/pages/StreamPage.tsx).
//   - No user editing. Producers create events; re-pushing the same external_id
//     replaces the whole thing. Users can only delete, and deletion writes
//     state='deleted' (a tombstone) instead of removing the row: when a producer
//     re-runs, or an app re-pushes the same external_id, we still have to recognize
//     "the user deleted this one", or it comes back to life. The payload is cleared,
//     leaving only the dedupe key.
//   - The stream owns events; apps only submit. Revoking a token or deleting an app
//     never takes content away.
//   - App calls DELETE → state='detached' (content kept, external link cleared)
//   - User deletes → state='deleted' (content cleared, so it cannot be revived)
//   - Media may only reference files on the drive (the payload holds paths, resolved
//     to node_id on write). No hotlinked images: hotlink protection, link rot,
//     moderation and SSRF are all problems.
package events

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/store"
)

// Card types.
const (
	TypePhoto  = "photo"
	TypeNote   = "note"
	TypeMetric = "metric"
	TypeHTML   = "html"
)

// Origins.
const (
	OriginNative = "native"
	OriginAuto   = "auto"
	OriginApp    = "app"
)

// States. deleted is the tombstone: gone from the read path, but the dedupe key
// survives.
const (
	StatePublished = "published"
	StateDetached  = "detached"
	StateDeleted   = "deleted"
)

// Limits. Changing these means updating docs/design/events.md §6.4 and
// GET /capabilities.
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

// Metric is one numeric slot.
type Metric struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

// Media uses an authorized same-origin content URL. A URL confers no authority;
// every byte request must authenticate through the unified file boundary.
type Media struct {
	Path  string `json:"path"`
	URL   string `json:"url,omitempty"`
	Name  string `json:"name,omitempty"`
	Video bool   `json:"video,omitempty"`
}

// Payload is the card content returned on the read path. For the write-path shape,
// see PayloadIn.
type Payload struct {
	Body    string   `json:"body,omitempty"`
	Meta    string   `json:"meta,omitempty"`
	Media   []Media  `json:"media,omitempty"`
	Metrics []Metric `json:"metrics,omitempty"`
	HTML    string   `json:"html,omitempty"`
	Height  int      `json:"height,omitempty"`
}

// PayloadIn is the write-path payload (also the stored shape). The one difference from
// the read path is that media is stored as paths.
type PayloadIn struct {
	Body    string   `json:"body,omitempty"`
	Meta    string   `json:"meta,omitempty"`
	Media   []string `json:"media,omitempty"`
	Metrics []Metric `json:"metrics,omitempty"`
	HTML    string   `json:"html,omitempty"`
	Height  int      `json:"height,omitempty"`
}

// Source is attribution. native / auto show no name; app shows the token name.
type Source struct {
	Kind         string `json:"kind"`
	Name         string `json:"name,omitempty"`
	Disconnected bool   `json:"disconnected,omitempty"`
}

// Event is the public event shape.
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

// AutoInput is one event submitted by the file pipeline.
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

// AutoMedia is media the pipeline has already resolved.
type AutoMedia struct {
	NodeID string
	Path   string
	Video  bool
}

// AppInput is one event submitted by a third-party app. Media arrives as paths and is
// resolved to nodes on write.
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

// Service is the read/write entry point for events.
type Service struct {
	db    *store.DB
	nodes *nodes.Service
}

func New(db *store.DB, nodeService *nodes.Service) *Service {
	return &Service{db: db, nodes: nodeService}
}

// ---------- reads ----------

// List returns one page of events, newest first. cursor is the opaque cursor of the
// last event on the previous page.
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

// Get reads one event. Only the owner's own events that were not deleted are
// returned.
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

// ListByToken supports app-side reconciliation: it returns the external ids of the
// events this app wrote.
func (s *Service) ListByToken(ctx context.Context, userID, tokenID string, from, to *time.Time) ([]Event, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select e.id::text, e.type, e.title, e.payload, coalesce(e.detail_url, ''),
		        e.starts_at, e.ends_at, e.local_date, e.precision, e.origin,
		        coalesce(t.name, ''), t.revoked_at is not null
		   from events e
		   left join api_tokens t on t.id = e.token_id
		  where e.user_id = $1::uuid and e.token_id = (select id from api_tokens where user_id=$1::uuid and token_id=$2) and e.state <> 'deleted'
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
		list[i].Payload = Payload{} // reconciliation only needs id / external_id
	}
	return list, nil
}

func scanEvents(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]Event, error) {
	// An empty page must still be [] rather than nil: JSON null makes the frontend treat
	// the list as an object.
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

// attachMedia fills in media URLs and video flags. One query per page, no N+1.
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
		  where em.event_id = any($1::uuid[]) and n.owner_id = $2::uuid
		  order by em.event_id, em.idx`, ids, userID)
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
			// The file was deleted: the card degrades to no image instead of erroring.
			event.Payload.Media = nil
			continue
		}
		out := make([]Media, 0, len(media))
		for _, row := range media {
			item := row.item
			item.URL = fc.InlineContentURL(item.Path)
			out = append(out, item)
		}
		event.Payload.Media = out
	}
	return nil
}

// ---------- writes: pipeline ----------

// PublishAuto writes the segments selected for one month as origin='auto' events.
// The same producer_key only updates instead of inserting, and a user tombstone is
// never resurrected.
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
				// Tombstone: the user deleted it, so leave it gone.
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

// ---------- writes: third-party apps ----------

// CreateApp idempotently creates or updates one app event.
// created=false means an existing event was overwritten; 410 means the user deleted
// this one.
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
		 select $1::uuid, 'app', t.id, $3, $4, $5, $6, $7::date, $8, $9, $10, $11, $12::jsonb, 'published'
		   from api_tokens t where t.user_id=$1::uuid and t.token_id=$2
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

// DetachApp is the app-side delete: keep the content, clear the external link.
func (s *Service) DetachApp(ctx context.Context, userID, tokenID, eventID string) error {
	tag, err := s.db.Pool().Exec(ctx,
		`update events set state = 'detached', detail_url = null, updated_at = now()
		  where user_id = $1::uuid and token_id = (select id from api_tokens where user_id=$1::uuid and token_id=$2) and id = $3::uuid
		    and state in ('published', 'detached')`, userID, tokenID, eventID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("event not found")
	}
	return nil
}

// DeleteOwned is a user deleting from the web: tombstone plus cleared content, so
// neither the pipeline nor an app can revive it.
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

// ---------- validation ----------

// ValidateApp checks third-party submissions. Every limit lives here, so a limit change
// touches one place. Pointer receiver: when an html card omits its height, the default
// is filled in here so the stored payload is complete.
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

// ---------- cursors ----------

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
