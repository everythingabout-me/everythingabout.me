package timeline

import (
	"sort"
	"time"
)

// selectMoments picks what to show from one month's candidates.
//
// The rule is not "N per day" but:
//   - a fixed total budget (24 display units by default); only what fits appears;
//   - at most 3 per day / 12 units, so one day cannot hog the month;
//   - highest value first, then a shrinking "minimum gap" backfills the rest:
//     high-value moments may sit close together, ordinary ones spread out.
//
// A travel month thus grows denser and a quiet month sparser, while the
// year-round rhythm stays stable.
func selectMoments(candidates []Moment, budget, ambientCost int) []Moment {
	if budget <= 0 {
		return nil
	}
	remaining := budget
	if ambientCost > 0 && ambientCost < remaining {
		remaining -= ambientCost
	}

	sorted := make([]Moment, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Score >= minScore {
			sorted = append(sorted, candidate)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Score != sorted[j].Score {
			return sorted[i].Score > sorted[j].Score
		}
		return sorted[i].StartsAt.Before(sorted[j].StartsAt)
	})

	selected := make([]Moment, 0, len(sorted))
	taken := make(map[string]bool, len(sorted))
	perDay := make(map[string]int)
	perDayCost := make(map[string]int)
	spent := 0

	// Relax the gap from 2 days to 0 so the budget gets used up.
	for _, minGap := range []int{2, 1, 0} {
		for _, candidate := range sorted {
			if taken[candidate.DedupeKey] {
				continue
			}
			day := candidate.LocalDate.Format("2006-01-02")
			if perDay[day] >= maxPerDayMoments || perDayCost[day]+candidate.Cost > maxPerDayCost {
				continue
			}
			if spent+candidate.Cost > remaining {
				continue
			}
			if minGap > 0 && candidate.Score < 0.75 && tooClose(candidate, selected, minGap) {
				continue
			}
			selected = append(selected, candidate)
			taken[candidate.DedupeKey] = true
			perDay[day]++
			perDayCost[day] += candidate.Cost
			spent += candidate.Cost
		}
	}

	sort.Slice(selected, func(i, j int) bool {
		return selected[i].StartsAt.Before(selected[j].StartsAt)
	})
	return selected
}

// tooClose reports whether the candidate sits within minGap days of anything selected.
func tooClose(candidate Moment, selected []Moment, minGap int) bool {
	for _, item := range selected {
		days := int(candidate.LocalDate.Sub(item.LocalDate).Hours() / 24)
		if days < 0 {
			days = -days
		}
		if days < minGap {
			return true
		}
	}
	return false
}

// gapItem builds a collapsed blank row; it only earns a slot when the span is long
// enough and files actually exist.
func gapItem(from, to time.Time, fileCount int) *Item {
	if fileCount <= 0 {
		return nil
	}
	days := int(to.Sub(from).Hours()/24) + 1
	if days < 7 {
		return nil
	}
	return &Item{
		Type:      "gap",
		From:      from.Format("2006-01-02"),
		To:        to.Format("2006-01-02"),
		ItemCount: fileCount,
		Label:     "这段时间",
	}
}
