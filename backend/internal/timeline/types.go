// Package timeline turns files into one stream.
//
// The UI never says "event", "timeline" or "segment": to the user this is just a
// stream scrolling down through time. Internally we still need a unit (Moment
// here), because it can span days, merge several sources, and it is the smallest
// object that density is computed over.
//
// The pipeline deliberately has two steps:
//  1. scan / backfill: turn files in nodes into timeline_assets (time, type, narrative or not);
//  2. month: group one month's assets into moments, select by budget, write timeline_snapshots.
//
// Analysis, grouping, scoring and naming all go through interfaces
// (Analyzer / Grouper / Scorer / Narrator). Only heuristics exist today; plugging in
// multimodal models or an LLM later swaps the implementation, not the callers.
package timeline

import (
	"context"
	"time"
)

const (
	// Bumping pipelineVersion forces a re-analysis; increment it whenever
	// classification or scoring changes.
	pipelineVersion = 2

	// Per-month display budget within a stream (one unit ≈ 72px).
	// The cap is not a target: if nothing is worth showing, nothing appears.
	defaultMonthBudget = 24
	// Max moments and display cost per day, so one day cannot crowd out a month.
	maxPerDayMoments = 3
	maxPerDayCost    = 12
	// Minimum value a moment needs to enter selection.
	minScore = 0.40
)

// FileInput is the Analyzer's input: it relies only on fields the index already
// has and never requires reading object bytes; reading bytes (health CSVs) is the
// AmbientExtractor's job.
type FileInput struct {
	UserID         string
	NodeID         string
	Path           string
	Name           string
	Parent         string
	Size           int64
	Mime           string
	ETag           string
	SourceModified time.Time
	UpdatedAt      time.Time
	CreatedAt      time.Time
}

// Facts is the analysis result. Future EXIF / GPS / frame labels / aesthetics
// scores all land here.
type Facts struct {
	EventTime      time.Time
	TimeSource     string
	TimeConfidence float64
	MediaType      string // photo | video | audio | doc | data | other
	Narrative      bool   // whether it joins the stream
	Ambient        bool   // whether it is ambient data (health CSVs etc.)
	Screenshot     bool
	Quality        float64
	// Meta is the extension slot: multimodal results (labels, scenes, face count,
	// ...) go here; for now it only holds heuristic evidence handy for debugging.
	Meta map[string]any
}

// Analyzer decides a file's time, type and first-pass value signal.
//
// To add multimodality, implement this interface: read images/videos, run models,
// write back Facts and Meta. It is called in batches, so the implementation owns
// its own concurrency control and rate limiting.
type Analyzer interface {
	Name() string
	Analyze(ctx context.Context, file FileInput) (Facts, error)
}

// Asset is a file fact after analysis. The month stage reads only this.
type Asset struct {
	NodeID     string
	Path       string
	Name       string
	Parent     string
	Size       int64
	MediaType  string
	EventTime  time.Time
	LocalDate  time.Time
	Quality    float64
	Narrative  bool
	Ambient    bool
	Screenshot bool
}

// Moment is the stream's atomic unit: one session, one trip, one series.
// It need not belong to a single day, nor to a single source.
type Moment struct {
	ID          string
	DedupeKey   string
	Kind        string
	StartsAt    time.Time
	EndsAt      time.Time
	LocalDate   time.Time
	Title       string
	Subtitle    string
	CoverNodeID string
	ItemIDs     []string
	ItemCount   int
	SourceKinds []string
	Score       float64
	Cost        int
	Layout      string
	Reason      map[string]any
	CoverPath   string
	Folder      string
	Video       bool
	DurationMS  int64
}

// Grouper clusters one month's assets into candidate moments.
// Today it splits sessions by day plus time gap; scene / place / people models can
// replace it later.
type Grouper interface {
	Name() string
	Group(ctx context.Context, assets []Asset) ([]Moment, error)
}

// Scorer gives a moment its value score. Multimodal aesthetics and LLM judgment
// hook in here.
type Scorer interface {
	Name() string
	Score(ctx context.Context, moment *Moment) error
}

// Narrator owns the title and subtitle. Path and time-of-day templates today, an
// LLM later.
type Narrator interface {
	Name() string
	Narrate(ctx context.Context, moment *Moment) error
}

// AmbientExtractor pulls day-granularity series (sleep, heart rate, steps, ...) out
// of data files.
//
// For heavier parsers (full Apple Health exports, vendor-specific band formats),
// implement this interface and register it in Engine.Ambient; a parse failure never
// affects the file itself.
type AmbientExtractor interface {
	Name() string
	Match(path string, mime string) bool
	Extract(ctx context.Context, file FileInput, data []byte) ([]SeriesSample, error)
}

// SeriesSample is one day-granularity observation.
type SeriesSample struct {
	Metric string
	Label  string
	Unit   string
	Date   time.Time
	Value  float64
}

// Month is one page of the stream. The read path reads only timeline_snapshots.payload.
type Month struct {
	Month       string `json:"month"`
	FileCount   int    `json:"file_count"`
	MomentCount int    `json:"moment_count"`
	Coverage    string `json:"coverage"`
	GeneratedAt string `json:"generated_at,omitempty"`
	Items       []Item `json:"items"`
}

// Item is the minimal information needed to render one card. Type picks the
// frontend card.
type Item struct {
	Type      string   `json:"type"` // moment | gap | ambient
	ID        string   `json:"id,omitempty"`
	Layout    string   `json:"layout,omitempty"` // single | mosaic | hero | ambient
	Title     string   `json:"title,omitempty"`
	Subtitle  string   `json:"subtitle,omitempty"`
	Folder    string   `json:"folder,omitempty"`
	StartsAt  string   `json:"starts_at,omitempty"`
	EndsAt    string   `json:"ends_at,omitempty"`
	ItemCount int      `json:"item_count,omitempty"`
	More      int      `json:"more,omitempty"`
	Cover     *Thumb   `json:"cover,omitempty"`
	Grid      []Thumb  `json:"grid,omitempty"`
	Score     float64  `json:"score,omitempty"`
	Label     string   `json:"label,omitempty"`
	From      string   `json:"from,omitempty"`
	To        string   `json:"to,omitempty"`
	Series    []Series `json:"series,omitempty"`
}

// Thumb is a displayable cover. Its URL is presigned at read time (10 minutes) and
// never stored in a snapshot.
type Thumb struct {
	Path       string `json:"path"`
	URL        string `json:"url,omitempty"`
	Name       string `json:"name,omitempty"`
	Video      bool   `json:"video,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// Series is one mini curve in the data band.
type Series struct {
	Metric string    `json:"metric"`
	Label  string    `json:"label"`
	Unit   string    `json:"unit,omitempty"`
	Days   []int     `json:"days"`
	Values []float64 `json:"values"`
	Latest float64   `json:"latest"`
	Min    float64   `json:"min"`
	Max    float64   `json:"max"`
}

// Status tells the frontend how much organizing is still in flight.
type Status struct {
	Enabled       bool   `json:"enabled"`
	Pending       int    `json:"pending"`
	IndexedFiles  int    `json:"indexed_files"`
	EarliestMonth string `json:"earliest_month,omitempty"`
	LatestMonth   string `json:"latest_month,omitempty"`
	LastGenerated string `json:"last_generated,omitempty"`
}

// Feed is the result of one paginated read.
type Feed struct {
	Periods []Month `json:"periods"`
	Next    string  `json:"next,omitempty"`
	Status  Status  `json:"status"`
}
