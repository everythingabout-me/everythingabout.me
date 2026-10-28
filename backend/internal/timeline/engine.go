package timeline

import (
	"time"

	"everythingabout.me/backend/internal/config"
)

// Engine wires the four replaceable stages together. All of them default to
// heuristics; to plug in multimodal models or an LLM, swap the fields after
// construction:
//
//	engine.Analyzer = multimodal.New(...)
//	engine.Scorer   = llmScorer.New(...)
//
// Callers above (worker, snapshots, API) never see the implementation.
type Engine struct {
	Analyzer Analyzer
	Grouper  Grouper
	Scorer   Scorer
	Narrator Narrator
	Ambient  []AmbientExtractor
	Location *time.Location
	Budget   int
}

// NewEngine builds the default engine from config.
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

// localDate snaps an event time to the day it falls on in the user's timezone.
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
