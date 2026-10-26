package timeline

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- 默认实现：只看索引元数据，不下载原图 ----------

// HeuristicAnalyzer 是 P0 的分析器。它只读文件路径、大小、mime 和修改时间。
//
// 它明确不做的事（以后由多模态 Analyzer 接）：EXIF 拍摄时间、GPS、画面内容、
// 清晰度/审美、人脸、视频关键帧。接口保持不变，所以可以逐个替换。
type HeuristicAnalyzer struct {
	Location *time.Location
}

func (HeuristicAnalyzer) Name() string { return "heuristic" }

func (a HeuristicAnalyzer) Analyze(_ context.Context, file FileInput) (Facts, error) {
	event := file.SourceModified
	source := "mtime"
	confidence := 0.65
	if event.IsZero() {
		event = file.UpdatedAt
		source = "ingest"
		confidence = 0.3
	}
	if event.IsZero() {
		event = file.CreatedAt
		source = "ingest"
		confidence = 0.2
	}

	// 路径里带日期（Photos/2025/09/14、2025-9-14 第三届…）通常比
	// "今天上传"更接近真实发生的时间。差得远才采用，避免覆盖正常拍摄时间。
	loc := a.Location
	if loc == nil {
		loc = time.UTC
	}
	if candidate, ok := dateFromPath(file.Path, loc); ok &&
		(candidate.Before(time.Now().AddDate(1, 0, 0))) &&
		(event.IsZero() || absDuration(event.Sub(candidate)) > 48*time.Hour) {
		event = candidate
		source = "filename"
		confidence = 0.55
	}

	media := mediaTypeOf(file.Name, file.Mime)
	screenshot := looksLikeScreenshot(file.Path, file.Name)
	narrative := (media == "photo" || media == "video") && !screenshot && !strings.HasPrefix(file.Name, ".")
	ambient := media == "data" && looksLikeAmbient(file.Path)

	quality := 0.5
	if file.Size > 2<<20 {
		quality += 0.05
	}
	if media == "video" {
		quality += 0.03
	}

	return Facts{
		EventTime:      event,
		TimeSource:     source,
		TimeConfidence: confidence,
		MediaType:      media,
		Narrative:      narrative,
		Ambient:        ambient,
		Screenshot:     screenshot,
		Quality:        clamp01(quality),
		Meta: map[string]any{
			"classifier": "heuristic",
			"ext":        strings.ToLower(path.Ext(file.Name)),
		},
	}, nil
}

// SessionGrouper 按「同一天 + 相邻间隔 < 25 分钟」切会话。
//
// 它刻意不按文件数截断：500 张照片仍然是一个 Moment 的成员，
// 只是展示时只取 9 张代表。这样"全部照片"永远找得到。
type SessionGrouper struct {
	Location *time.Location
}

func (SessionGrouper) Name() string { return "session-by-day" }

// sessionGap 是一次会话内允许的最大空档。拍照时吃饭、坐车都可能停顿。
const sessionGap = 25 * time.Minute

func (g SessionGrouper) Group(_ context.Context, assets []Asset) ([]Moment, error) {
	loc := g.Location
	if loc == nil {
		loc = time.UTC
	}

	// 先认"每日视频"：同一个目录里、跨 ≥7 天、≥7 个视频 → 一个系列。
	// 它不该按天占 30 张卡。这个判断以后可以换成模型，接口不变。
	seriesAssets := make(map[string][]Asset)
	for _, asset := range assets {
		if asset.Narrative && asset.MediaType == "video" && asset.Parent != "" {
			seriesAssets[asset.Parent] = append(seriesAssets[asset.Parent], asset)
		}
	}
	consumed := make(map[string]bool)
	moments := make([]Moment, 0, len(assets)/4)
	for folder, group := range seriesAssets {
		if !isVideoSeries(group) {
			continue
		}
		moments = append(moments, buildSeriesMoment(folder, group))
		for _, asset := range group {
			consumed[asset.NodeID] = true
		}
	}

	byDay := make(map[string][]Asset)
	for _, asset := range assets {
		if !asset.Narrative || consumed[asset.NodeID] {
			continue
		}
		key := asset.LocalDate.In(loc).Format("2006-01-02")
		byDay[key] = append(byDay[key], asset)
	}

	days := make([]string, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	sort.Strings(days)

	for _, day := range days {
		items := byDay[day]
		sort.Slice(items, func(i, j int) bool {
			return items[i].EventTime.Before(items[j].EventTime)
		})

		start := 0
		for i := 1; i <= len(items); i++ {
			if i < len(items) && items[i].EventTime.Sub(items[i-1].EventTime) < sessionGap {
				continue
			}
			moments = append(moments, buildMoment(items[start:i]))
			start = i
		}
	}
	return moments, nil
}

func isVideoSeries(assets []Asset) bool {
	if len(assets) < 7 {
		return false
	}
	days := make(map[string]struct{}, len(assets))
	first, last := assets[0].LocalDate, assets[0].LocalDate
	for _, asset := range assets {
		days[asset.LocalDate.Format("2006-01-02")] = struct{}{}
		if asset.LocalDate.Before(first) {
			first = asset.LocalDate
		}
		if asset.LocalDate.After(last) {
			last = asset.LocalDate
		}
	}
	return len(days) >= 7 && int(last.Sub(first).Hours()/24) <= 62
}

func buildSeriesMoment(folder string, items []Asset) Moment {
	sort.Slice(items, func(i, j int) bool {
		return items[i].EventTime.Before(items[j].EventTime)
	})
	cover := items[0]
	nodeIDs := make([]string, 0, len(items))
	for _, item := range items {
		nodeIDs = append(nodeIDs, item.NodeID)
	}
	sum := sha1.Sum([]byte("series|" + folder + "|" + items[0].LocalDate.Format("2006-01")))
	return Moment{
		DedupeKey:   hex.EncodeToString(sum[:]),
		Kind:        "series",
		StartsAt:    items[0].EventTime,
		EndsAt:      items[len(items)-1].EventTime,
		LocalDate:   items[0].LocalDate,
		CoverNodeID: cover.NodeID,
		CoverPath:   cover.Path,
		Folder:      folder,
		ItemIDs:     nodeIDs,
		ItemCount:   len(items),
		SourceKinds: []string{"video"},
		Video:       true,
	}
}

func buildMoment(items []Asset) Moment {
	first := items[0]
	last := items[len(items)-1]

	nodeIDs := make([]string, 0, len(items))
	kinds := make(map[string]struct{})
	cover := items[0]
	for _, item := range items {
		nodeIDs = append(nodeIDs, item.NodeID)
		kinds[item.MediaType] = struct{}{}
		if preferCover(item, cover) {
			cover = item
		}
	}

	sourceKinds := make([]string, 0, len(kinds))
	for kind := range kinds {
		sourceKinds = append(sourceKinds, kind)
	}
	sort.Strings(sourceKinds)

	sum := sha1.Sum([]byte("session|" + first.LocalDate.Format("2006-01-02") + "|" + first.NodeID))
	return Moment{
		DedupeKey:   hex.EncodeToString(sum[:]),
		Kind:        "session",
		StartsAt:    first.EventTime,
		EndsAt:      last.EventTime,
		LocalDate:   first.LocalDate,
		CoverNodeID: cover.NodeID,
		CoverPath:   cover.Path,
		Folder:      cover.Parent,
		ItemIDs:     nodeIDs,
		ItemCount:   len(items),
		SourceKinds: sourceKinds,
		Video:       cover.MediaType == "video",
	}
}

// preferCover 优先图片，其次体积更大的；视频只在前者没有时当封面。
func preferCover(candidate, current Asset) bool {
	if (candidate.MediaType == "photo") != (current.MediaType == "photo") {
		return candidate.MediaType == "photo"
	}
	return candidate.Size > current.Size
}

// HeuristicScorer 是 P0 的价值模型。分数只用于排序和预算取舍，
// 不追求绝对准确；reason 里保留每个信号，方便以后校准。
type HeuristicScorer struct {
	Location *time.Location
}

func (HeuristicScorer) Name() string { return "heuristic" }

func (s HeuristicScorer) Score(_ context.Context, moment *Moment) error {
	loc := s.Location
	if loc == nil {
		loc = time.UTC
	}
	reason := map[string]any{}

	// 基线：一张普通照片也值得被看见，但不一定能挤进预算。
	score := 0.42
	reason["base"] = 0.42

	// 规模：500 张照片的会话比 3 张的更像一件事，但增长要饱和。
	n := float64(moment.ItemCount)
	scale := math.Log10(n+1) / 2.7
	score += 0.16 * math.Min(1, scale)
	reason["scale"] = round3(0.16 * math.Min(1, scale))

	// 文件夹语义：旅行、婚礼、毕业这类名字本身就是强信号。
	if label := folderLabel(moment.Folder); label != "" {
		if eventWord(label) {
			score += 0.13
			reason["event_folder"] = 0.13
		} else {
			score += 0.03
			reason["named_folder"] = 0.03
		}
	}

	if moment.Video {
		score += 0.06
		reason["video"] = 0.06
	}
	if moment.ItemCount >= 20 {
		score += 0.03
		reason["many_items"] = 0.03
	}

	// 周末/晚上稍微加分，但权重很小，不能让"时间"决定内容。
	local := moment.StartsAt.In(loc)
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		score += 0.015
		reason["weekend"] = 0.015
	}
	if local.Hour() >= 18 {
		score += 0.01
		reason["evening"] = 0.01
	}

	// 确定性抖动：同分时避免永远选同一条，也不引入随机刷新。
	jitter := hashUnit(moment.DedupeKey) * 0.03
	score += jitter
	reason["tie_break"] = round3(jitter)

	moment.Score = clamp01(score)
	moment.Reason = reason
	return nil
}

// TemplateNarrator 用路径和时段生成标题。
//
// 以后换成 LLM 时，这里能拿到 Score 阶段的 reason、来源类型、时间、文件数，
// 输出仍然是 Title/Subtitle 两个字段，前端完全不用改。
type TemplateNarrator struct {
	Location *time.Location
}

func (TemplateNarrator) Name() string { return "template" }

func (n TemplateNarrator) Narrate(_ context.Context, moment *Moment) error {
	loc := n.Location
	if loc == nil {
		loc = time.UTC
	}

	label := folderLabel(moment.Folder)
	period := timeOfDay(moment.StartsAt.In(loc))
	if label == "" {
		switch {
		case period != "" && moment.Video:
			label = period + "的视频"
		case period != "":
			label = period + "的照片"
		case moment.Video:
			label = "视频"
		default:
			label = "照片"
		}
	}
	moment.Title = truncateRunes(label, 24)

	startLocal := moment.StartsAt.In(loc)
	endLocal := moment.EndsAt.In(loc)
	start := startLocal.Format("15:04")
	end := endLocal.Format("15:04")
	span := start
	if end != start {
		span = start + "–" + end
	}

	// 月份标题已经给了月份，但卡片本身必须能回答"这是哪天"。
	date := fmt.Sprintf("%d 月 %d 日", startLocal.Month(), startLocal.Day())
	if startLocal.Format("2006-01-02") != endLocal.Format("2006-01-02") {
		date = fmt.Sprintf("%d 月 %d 日 – %d 月 %d 日",
			startLocal.Month(), startLocal.Day(), endLocal.Month(), endLocal.Day())
	}

	// 系列：一个目录里的连续视频，只占一张卡。
	if moment.Kind == "series" {
		moment.Subtitle = fmt.Sprintf("%s · %d 个视频", date, moment.ItemCount)
		moment.Layout = "series"
		moment.Cost = 3
		return nil
	}

	count := moment.ItemCount
	switch {
	case moment.Video && count == 1:
		moment.Subtitle = fmt.Sprintf("%s · %s · 视频", date, span)
		moment.Layout = "hero"
		moment.Cost = 6
	case count == 1:
		moment.Subtitle = fmt.Sprintf("%s · %s", date, span)
		moment.Layout = "single"
		moment.Cost = 2
	default:
		moment.Subtitle = fmt.Sprintf("%s · %s · %d 张", date, span, count)
		moment.Layout = "mosaic"
		moment.Cost = 4
	}

	if moment.ItemCount > 1 && moment.ItemCount <= 3 {
		moment.Layout = "mosaic"
	}
	return nil
}

// ---------- 小工具 ----------

func mediaTypeOf(name, mime string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".heic", ".heif", ".webp", ".gif", ".bmp",
		".tif", ".tiff", ".dng", ".cr2", ".nef", ".arw", ".raf", ".live":
		return "photo"
	case ".mp4", ".mov", ".m4v", ".avi", ".mkv", ".webm", ".3gp", ".mpeg", ".mpg", ".ts", ".flv", ".wmv":
		return "video"
	case ".mp3", ".m4a", ".wav", ".flac", ".aac", ".ogg", ".opus":
		return "audio"
	case ".csv", ".json", ".xml", ".fit", ".tcx", ".gpx", ".yaml", ".yml":
		return "data"
	}
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "photo"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "text/csv"), strings.Contains(mime, "json"):
		return "data"
	}
	return "doc"
}

func looksLikeScreenshot(itemPath, name string) bool {
	lower := strings.ToLower(itemPath + "/" + name)
	for _, marker := range []string{
		"screenshot", "screen shot", "screen_shot", "截屏", "截图", "屏幕截图", "螢幕截圖",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func looksLikeAmbient(itemPath string) bool {
	lower := strings.ToLower(itemPath)
	for _, marker := range []string{
		"health", "fitness", "sleep", "steps", "weight", "heart", "workout",
		"健康", "睡眠", "步数", "体重", "心率", "血压", "运动",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// genericSegments 是路径里没有叙事价值的目录名。
var genericSegments = map[string]bool{
	"photos": true, "photo": true, "pictures": true, "picture": true,
	"camera": true, "dcim": true, "images": true, "image": true, "img": true,
	"videos": true, "video": true, "movies": true, "movie": true,
	"照片": true, "图片": true, "相片": true, "视频": true, "相机": true,
	"截屏": true, "截图": true, "screenshots": true, "screenshot": true,
	"downloads": true, "download": true, "下载": true, "微信": true, "wechat": true,
	"backup": true, "backups": true, "备份": true, "sync": true, "同步": true,
	"我的图片": true, "camera roll": true, "camera-roll": true,
}

// folderLabel 从父目录里找最近一个"像名字"的目录。
func folderLabel(folder string) string {
	segments := strings.Split(folder, "/")
	for i := len(segments) - 1; i >= 0; i-- {
		segment := strings.TrimSpace(segments[i])
		if segment == "" {
			continue
		}
		segment = stripDatePrefix(segment)
		if segment == "" {
			continue
		}
		lower := strings.ToLower(segment)
		if genericSegments[lower] {
			continue
		}
		if isDateish(segment) || isCameraSegment(segment) {
			continue
		}
		if len([]rune(segment)) > 24 {
			continue
		}
		return segment
	}
	return ""
}

func isDateish(segment string) bool {
	clean := strings.NewReplacer("-", "", "_", "", "/", "", ".", "").Replace(segment)
	if len(clean) < 2 {
		return false
	}
	for _, r := range clean {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isCameraSegment(segment string) bool {
	lower := strings.ToLower(segment)
	if strings.HasPrefix(lower, "10") && strings.HasSuffix(lower, "apple") {
		return true
	}
	for _, marker := range []string{"100canon", "100nikon", "100msd", "roll", "dsc", "pano"} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

func eventWord(label string) bool {
	lower := strings.ToLower(label)
	for _, word := range []string{
		"旅行", "旅游", "婚礼", "生日", "毕业", "聚会", "露营", "徒步", "爬山",
		"海边", "演唱会", "展览", "出差", "回家", "过年", "春节", "圣诞", "纪念",
		"trip", "travel", "wedding", "birthday", "party", "holiday", "camp", "hike",
	} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

var (
	// 前后都要卡住数字，否则 "2025/06/2025-06-01" 会被读成 2025/06/20。
	dateWithSep = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})[-_/.](\d{1,2})[-_/.](\d{1,2})(?:[^0-9]|$)`)
	dateCompact = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})(\d{2})(\d{2})(?:[^0-9]|$)`)
	datePrefix  = regexp.MustCompile(`^(?:(?:19|20)\d{2}[-_/.]\d{1,2}[-_/.]\d{1,2}|(?:19|20)\d{6})[\s_-]*`)
)

// dateFromPath 从路径里找一个完整日期（年月日），返回当天中午。
// 只认 19xx / 20xx，且会校验月日合法，避免把 "100APPLE/1234" 当成日期。
func dateFromPath(itemPath string, loc *time.Location) (time.Time, bool) {
	match := dateWithSep.FindStringSubmatch(itemPath)
	if len(match) != 4 {
		match = dateCompact.FindStringSubmatch(itemPath)
	}
	if len(match) != 4 {
		return time.Time{}, false
	}
	year, err1 := strconv.Atoi(match[1])
	month, err2 := strconv.Atoi(match[2])
	day, err3 := strconv.Atoi(match[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return time.Time{}, false
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, false
	}
	value := time.Date(year, time.Month(month), day, 12, 0, 0, 0, loc)
	if value.Year() != year || int(value.Month()) != month || value.Day() != day {
		return time.Time{}, false
	}
	return value, true
}

func stripDatePrefix(value string) string {
	cleaned := datePrefix.ReplaceAllString(value, "")
	cleaned = strings.TrimSpace(cleaned)
	cleaned = strings.Trim(cleaned, "-_.")
	return strings.TrimSpace(cleaned)
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func timeOfDay(local time.Time) string {
	switch hour := local.Hour(); {
	case hour >= 5 && hour < 11:
		return "早上"
	case hour >= 11 && hour < 13:
		return "中午"
	case hour >= 13 && hour < 18:
		return "下午"
	case hour >= 18 && hour < 23:
		return "晚上"
	default:
		return "深夜"
	}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func round3(value float64) float64 {
	return math.Round(value*1000) / 1000
}

func hashUnit(value string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(value))
	return float64(hasher.Sum32()%1000) / 1000
}
