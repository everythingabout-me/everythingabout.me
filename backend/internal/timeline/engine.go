package timeline

import (
	"time"

	"everythingabout.me/backend/internal/config"
)

// Engine 把可替换的四个环节串起来。默认全是启发式实现；
// 接多模态 / 大模型时只需要在构造后替换字段：
//
//	engine.Analyzer = multimodal.New(...)
//	engine.Scorer   = llmScorer.New(...)
//
// 上层（worker、快照、接口）不感知实现。
type Engine struct {
	Analyzer Analyzer
	Grouper  Grouper
	Scorer   Scorer
	Narrator Narrator
	Ambient  []AmbientExtractor
	Location *time.Location
	Budget   int
}

// NewEngine 用配置文件构造默认引擎。
func NewEngine(cfg config.Timeline) *Engine {
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil || location == nil {
		location = time.UTC
	}
	budget := cfg.MonthBudget
	if budget <= 0 {
		budget = defaultMonthBudget
	}
	return &Engine{
		Analyzer: HeuristicAnalyzer{Location: location},
		Grouper:  SessionGrouper{Location: location},
		Scorer:   HeuristicScorer{Location: location},
		Narrator: TemplateNarrator{Location: location},
		Ambient:  []AmbientExtractor{CSVAmbientExtractor{}},
		Location: location,
		Budget:   budget,
	}
}

// localDate 把事件时间落到"用户所在时区的那一天"。
func (e *Engine) localDate(event time.Time) time.Time {
	loc := e.Location
	if loc == nil {
		loc = time.UTC
	}
	local := event.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

func (e *Engine) monthStart(value time.Time) time.Time {
	local := value.In(e.Location)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, e.Location)
}
