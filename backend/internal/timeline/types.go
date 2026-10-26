// Package timeline 把文件整理成一条内容流。
//
// 界面上不出现"事件""时间线""片段"这些概念：对用户它就是一条按时间往下
// 滚的内容流。内部仍然需要一个单位（这里叫 Moment），因为它可以跨天、
// 可以融合多个来源，而且是密度计算的最小对象。
//
// 流水线刻意分成两步：
//  1. scan / backfill：把 nodes 里的文件变成 timeline_assets（时间、类型、是否叙事）；
//  2. month：把一个月里的 assets 聚成 moments，按预算挑选，写成 timeline_snapshots。
//
// 分析、分组、评分、命名都走接口（Analyzer / Grouper / Scorer / Narrator），
// 现在只有启发式实现；以后接多模态或大模型时替换实现即可，上层不用改。
package timeline

import (
	"context"
	"time"
)

const (
	// pipelineVersion 变了会触发重新分析。改分类/评分逻辑时 +1。
	pipelineVersion = 2

	// 一条内容流里，一个月的展示预算（单位 ≈ 72px）。
	// 预算上限不是目标：没有值得看的就不放。
	defaultMonthBudget = 24
	// 单日最多几个 Moment、多少展示成本，防止一天挤爆一个月。
	maxPerDayMoments = 3
	maxPerDayCost    = 12
	// 参与挑选的最低价值。
	minScore = 0.40
)

// FileInput 是 Analyzer 的输入：只依赖索引里已有的字段，
// 不要求读对象字节；需要读字节的（健康 CSV）由 AmbientExtractor 负责。
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

// Facts 是分析结果。将来的 EXIF / GPS / 画面标签 / 审美分都往这里加。
type Facts struct {
	EventTime      time.Time
	TimeSource     string
	TimeConfidence float64
	MediaType      string // photo | video | audio | doc | data | other
	Narrative      bool   // 是否参与内容流
	Ambient        bool   // 是否作为环境数据（健康 CSV 等）
	Screenshot     bool
	Quality        float64
	// Meta 是扩展位：多模态分析结果（标签、场景、人脸数……）放这里，
	// 现在只写一些便于调试的启发式依据。
	Meta map[string]any
}

// Analyzer 决定一个文件的时间、类型和初步价值信号。
//
// 接入多模态时实现这个接口：读图片/视频、跑模型、写回 Facts 和 Meta。
// 注意它会被批量调用，实现里要自己做并发控制和限流。
type Analyzer interface {
	Name() string
	Analyze(ctx context.Context, file FileInput) (Facts, error)
}

// Asset 是整理后的文件事实。month 阶段只读这个。
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

// Moment 是内容流的原子单元：一次会话、一段旅行、一个系列。
// 它不一定只属于一天，也不一定只有一种来源。
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

// Grouper 把一个月里的 assets 聚成候选 Moment。
// 现在的实现是"按天 + 时间间隔切会话"；以后可以用场景/地点/人物模型。
type Grouper interface {
	Name() string
	Group(ctx context.Context, assets []Asset) ([]Moment, error)
}

// Scorer 给 Moment 打价值分。多模态审美、LLM 判断都从这里接。
type Scorer interface {
	Name() string
	Score(ctx context.Context, moment *Moment) error
}

// Narrator 负责标题和副标题。现在用路径/时段模板，以后换 LLM。
type Narrator interface {
	Name() string
	Narrate(ctx context.Context, moment *Moment) error
}

// AmbientExtractor 从数据文件里抽日粒度序列（睡眠、心率、步数……）。
//
// 接入更复杂的解析器（Apple Health 全量导出、手环私有格式）时实现这个接口，
// 注册到 Engine.Ambient 里即可；解析失败不影响文件本身。
type AmbientExtractor interface {
	Name() string
	Match(path string, mime string) bool
	Extract(ctx context.Context, file FileInput, data []byte) ([]SeriesSample, error)
}

// SeriesSample 是一条日粒度的观测值。
type SeriesSample struct {
	Metric string
	Label  string
	Unit   string
	Date   time.Time
	Value  float64
}

// Month 是内容流的一页。读路径只读 timeline_snapshots.payload。
type Month struct {
	Month       string `json:"month"`
	FileCount   int    `json:"file_count"`
	MomentCount int    `json:"moment_count"`
	Coverage    string `json:"coverage"`
	GeneratedAt string `json:"generated_at,omitempty"`
	Items       []Item `json:"items"`
}

// Item 是渲染一张卡所需的最小信息。Type 决定前端用哪种卡片。
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

// Thumb 是一个可展示的封面。URL 在读取时预签名（10 分钟），快照里不存。
type Thumb struct {
	Path       string `json:"path"`
	URL        string `json:"url,omitempty"`
	Name       string `json:"name,omitempty"`
	Video      bool   `json:"video,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// Series 是数据带里的一条迷你曲线。
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

// Status 给前端判断"还有多少在整理"。
type Status struct {
	Enabled       bool   `json:"enabled"`
	Pending       int    `json:"pending"`
	IndexedFiles  int    `json:"indexed_files"`
	EarliestMonth string `json:"earliest_month,omitempty"`
	LatestMonth   string `json:"latest_month,omitempty"`
	LastGenerated string `json:"last_generated,omitempty"`
}

// Feed 是一次分页读取的结果。
type Feed struct {
	Periods []Month `json:"periods"`
	Next    string  `json:"next,omitempty"`
	Status  Status  `json:"status"`
}
