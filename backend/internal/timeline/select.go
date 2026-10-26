package timeline

import (
	"sort"
	"time"
)

// selectMoments 在一个月的候选里挑出要展示的那些。
//
// 规则不是"每天 N 条"，而是：
//   - 总预算固定（默认 24 单位），给得起的才出现；
//   - 单日最多 3 条 / 12 单位，防止一天独占；
//   - 先按价值从高到低，再用一个递减的"最小间隔"补位：
//     高价值允许挨着，普通内容尽量散开。
//
// 这样旅行月会自然变密、平淡月会自然变稀，而整年的滚动节奏保持稳定。
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

	// 间隔从 2 天放宽到 0 天，尽量把预算用满。
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

// tooClose 判断候选和已选内容是否在 minGap 天内。
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

// gapItem 生成一条压缩的空白行；只有跨度够长、确实有文件时才值得出现。
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
