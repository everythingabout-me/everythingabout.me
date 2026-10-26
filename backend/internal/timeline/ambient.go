package timeline

import (
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
)

// CSVAmbientExtractor 处理最简单的健康/运动数据：
//
//	date,resting_hr,sleep_hours,steps
//	2025-06-01,54,7.2,6840
//
// 或长表：
//
//	date,metric,value,unit
//	2025-06-01,resting_hr,54,bpm
//
// 它不负责 Apple Health 的 export.xml、手环私有格式。那些接入时实现
// AmbientExtractor 接口再注册即可，timeline 的其它部分不用动。
type CSVAmbientExtractor struct{}

func (CSVAmbientExtractor) Name() string { return "csv" }

func (CSVAmbientExtractor) Match(itemPath, _ string) bool {
	if strings.ToLower(path.Ext(itemPath)) != ".csv" {
		return false
	}
	return looksLikeAmbient(itemPath)
}

func (CSVAmbientExtractor) Extract(
	_ context.Context,
	_ FileInput,
	data []byte,
) ([]SeriesSample, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if len(header) == 0 {
		return nil, nil
	}
	for i := range header {
		header[i] = strings.TrimSpace(strings.TrimPrefix(header[i], "\ufeff"))
	}

	dateIndex := findColumn(header, "date", "day", "time", "timestamp", "日期", "时间")
	if dateIndex < 0 {
		return nil, nil
	}
	metricIndex := findColumn(header, "metric", "name", "指标", "项目")
	valueIndex := findColumn(header, "value", "数值", "值")
	longForm := metricIndex >= 0 && valueIndex >= 0

	type seriesKey struct{ metric, label, unit string }
	type point struct {
		date  time.Time
		value float64
	}
	values := make(map[seriesKey]map[string]point)
	order := make([]seriesKey, 0, len(header))

	add := func(key seriesKey, date time.Time, value float64) {
		if _, ok := values[key]; !ok {
			values[key] = make(map[string]point)
			order = append(order, key)
		}
		day := date.Format("2006-01-02")
		values[key][day] = point{date: date, value: value}
	}

	rows := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 单行坏掉不影响其它行；CSV 里这种情况很常见。
			continue
		}
		rows++
		if rows > 20000 {
			break
		}
		if dateIndex >= len(record) {
			continue
		}
		date, ok := parseAmbientDate(record[dateIndex])
		if !ok {
			continue
		}

		if longForm {
			if metricIndex >= len(record) || valueIndex >= len(record) {
				continue
			}
			metric := cleanMetric(record[metricIndex])
			if metric == "" {
				continue
			}
			value, ok := parseAmbientNumber(record[valueIndex])
			if !ok {
				continue
			}
			unit := ""
			if unitIndex := findColumn(header, "unit", "单位"); unitIndex >= 0 && unitIndex < len(record) {
				unit = strings.TrimSpace(record[unitIndex])
			}
			key := seriesKey{metric: metric, label: prettyMetric(metric), unit: unit}
			add(key, date, value)
			continue
		}

		for i, cell := range record {
			if i == dateIndex || i >= len(header) {
				continue
			}
			value, ok := parseAmbientNumber(cell)
			if !ok {
				continue
			}
			metric := cleanMetric(header[i])
			if metric == "" {
				continue
			}
			label, unit := splitUnit(header[i])
			key := seriesKey{metric: metric, label: label, unit: unit}
			add(key, date, value)
		}
	}

	samples := make([]SeriesSample, 0, len(order)*31)
	for _, key := range order {
		points := values[key]
		if len(points) < 2 {
			continue
		}
		for _, p := range points {
			samples = append(samples, SeriesSample{
				Metric: key.metric,
				Label:  key.label,
				Unit:   key.unit,
				Date:   p.date,
				Value:  p.value,
			})
		}
	}
	return samples, nil
}

func findColumn(header []string, names ...string) int {
	for i, cell := range header {
		lower := strings.ToLower(strings.TrimSpace(cell))
		for _, name := range names {
			if lower == name || strings.HasPrefix(lower, name+" ") {
				return i
			}
		}
	}
	return -1
}

func cleanMetric(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "（")
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if lower == "date" || lower == "日期" || lower == "时间" || lower == "time" {
		return ""
	}
	return truncateRunes(value, 32)
}

// splitUnit 把 "resting_hr(bpm)" 拆成 "resting_hr" + "bpm"。
func splitUnit(header string) (label, unit string) {
	header = strings.TrimSpace(header)
	open := strings.LastIndexAny(header, "(（")
	close := strings.LastIndexAny(header, ")）")
	if open > 0 && close > open {
		return strings.TrimSpace(header[:open]), strings.TrimSpace(header[open+1 : close])
	}
	lower := strings.ToLower(header)
	if known, ok := knownUnits[lower]; ok {
		return header, known
	}
	return header, ""
}

var knownUnits = map[string]string{
	"steps":              "步",
	"step_count":         "步",
	"步数":                 "步",
	"resting_hr":         "bpm",
	"resting_heart_rate": "bpm",
	"heart_rate":         "bpm",
	"hr":                 "bpm",
	"心率":                 "bpm",
	"sleep_hours":        "h",
	"sleep":              "h",
	"睡眠":                 "h",
	"weight":             "kg",
	"weight_kg":          "kg",
	"体重":                 "kg",
	"distance_km":        "km",
	"distance":           "km",
	"calories":           "kcal",
	"energy":             "kcal",
}

func prettyMetric(metric string) string {
	label, _ := splitUnit(metric)
	label = strings.ReplaceAll(label, "_", " ")
	return truncateRunes(label, 24)
}

func parseAmbientDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"2006-01-02", "2006/01/02", "2006-1-2", "2006/1/2",
		"2006-01-02 15:04:05", "2006/01/02 15:04:05",
		"2006-01-02T15:04:05", time.RFC3339,
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func parseAmbientNumber(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "%")
	value = strings.ReplaceAll(value, ",", "")
	value = strings.ReplaceAll(value, " ", "")
	if value == "" || value == "-" || value == "—" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}
