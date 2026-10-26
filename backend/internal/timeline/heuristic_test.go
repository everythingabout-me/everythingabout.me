package timeline

import (
	"context"
	"testing"
	"time"
)

func shanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestDateFromPath(t *testing.T) {
	loc := shanghai(t)
	cases := []struct {
		path string
		ok   bool
		want string
	}{
		{"Photos/2025/09/14/a.jpg", true, "2025-09-14"},
		{"运动样例/2003-7-8 样例邀请赛/clip-7f3a.mov", true, "2003-07-08"},
		{"20250914/a.jpg", true, "2025-09-14"},
		{"Vlog/2025/06/2025-06-01.mp4", true, "2025-06-01"},
		{"100APPLE/IMG_1234.JPG", false, ""},
		{"Photos/2025/13/40/a.jpg", false, ""},
		{"旅行的照片/a.jpg", false, ""},
	}
	for _, tc := range cases {
		got, ok := dateFromPath(tc.path, loc)
		if ok != tc.ok {
			t.Fatalf("%s: ok=%v want %v", tc.path, ok, tc.ok)
		}
		if ok && got.Format("2006-01-02") != tc.want {
			t.Fatalf("%s: got %s want %s", tc.path, got.Format("2006-01-02"), tc.want)
		}
	}
}

func TestFolderLabelStripsDate(t *testing.T) {
	cases := map[string]string{
		"运动样例/2003-7-8 样例邀请赛 场馆甲": "样例邀请赛 场馆甲",
		"Photos/2025/06": "",
		"旅行/2024/05/敦煌":  "敦煌",
		"DCIM/100APPLE":  "",
	}
	for folder, want := range cases {
		if got := folderLabel(folder); got != want {
			t.Fatalf("%s: got %q want %q", folder, got, want)
		}
	}
}

func TestSessionGrouper(t *testing.T) {
	loc := shanghai(t)
	day := time.Date(2025, 6, 15, 0, 0, 0, 0, loc)
	mk := func(minute int, id string) Asset {
		at := day.Add(time.Duration(minute) * time.Minute)
		return Asset{
			NodeID:    id,
			Path:      "Photos/2025/06/15/" + id + ".jpg",
			Name:      id + ".jpg",
			Parent:    "Photos/2025/06/15",
			Size:      1000,
			MediaType: "photo",
			EventTime: at,
			LocalDate: day,
			Narrative: true,
		}
	}

	assets := []Asset{mk(9*60, "a"), mk(9*60+5, "b"), mk(9*60+10, "c"), mk(11*60, "d")}
	grouper := SessionGrouper{Location: loc}
	moments, err := grouper.Group(context.Background(), assets)
	if err != nil {
		t.Fatal(err)
	}
	if len(moments) != 2 {
		t.Fatalf("got %d moments, want 2", len(moments))
	}
	if moments[0].ItemCount != 3 || moments[1].ItemCount != 1 {
		t.Fatalf("unexpected counts: %d, %d", moments[0].ItemCount, moments[1].ItemCount)
	}
	if len(moments[0].ItemIDs) != 3 {
		t.Fatalf("item ids lost: %d", len(moments[0].ItemIDs))
	}
}

func TestGrouperKeepsLargeSession(t *testing.T) {
	loc := shanghai(t)
	day := time.Date(2025, 6, 15, 0, 0, 0, 0, loc)
	assets := make([]Asset, 0, 500)
	for i := 0; i < 500; i++ {
		at := day.Add(9*time.Hour + time.Duration(i)*time.Second)
		assets = append(assets, Asset{
			NodeID:    "n" + time.Duration(i).String(),
			Path:      "Photos/a.jpg",
			Name:      "a.jpg",
			MediaType: "photo",
			EventTime: at,
			LocalDate: day,
			Narrative: true,
		})
	}
	grouper := SessionGrouper{Location: loc}
	moments, err := grouper.Group(context.Background(), assets)
	if err != nil {
		t.Fatal(err)
	}
	if len(moments) != 1 || moments[0].ItemCount != 500 {
		t.Fatalf("got %d moments, count %d", len(moments), moments[0].ItemCount)
	}
}

func TestVideoSeriesCollapses(t *testing.T) {
	loc := shanghai(t)
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, loc)
	makeVideo := func(day int) Asset {
		date := start.AddDate(0, 0, day)
		return Asset{
			NodeID:    "v" + time.Duration(day).String(),
			Path:      "Vlog/2025/06/" + date.Format("2006-01-02") + ".mp4",
			Name:      date.Format("2006-01-02") + ".mp4",
			Parent:    "Vlog/2025/06",
			MediaType: "video",
			EventTime: date.Add(20 * time.Hour),
			LocalDate: date,
			Narrative: true,
		}
	}

	var daily []Asset
	for day := 0; day < 30; day++ {
		daily = append(daily, makeVideo(day))
	}
	moments, err := (SessionGrouper{Location: loc}).Group(context.Background(), daily)
	if err != nil {
		t.Fatal(err)
	}
	if len(moments) != 1 || moments[0].Kind != "series" || moments[0].ItemCount != 30 {
		t.Fatalf("series not collapsed: %d moments", len(moments))
	}
	if moments[0].DedupeKey == "" || len(moments[0].ItemIDs) != 30 {
		t.Fatalf("series lost items")
	}

	// 6 天 6 个视频不够成系列，仍然按天。
	var sparse []Asset
	for day := 0; day < 6; day++ {
		sparse = append(sparse, makeVideo(day))
	}
	moments, err = (SessionGrouper{Location: loc}).Group(context.Background(), sparse)
	if err != nil {
		t.Fatal(err)
	}
	if len(moments) != 6 {
		t.Fatalf("sparse videos = %d moments, want 6", len(moments))
	}
}

func TestSelectRespectsBudgetAndDayCap(t *testing.T) {
	loc := shanghai(t)
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, loc)
	makeMoment := func(day, index int, score float64) Moment {
		date := start.AddDate(0, 0, day)
		return Moment{
			DedupeKey: date.Format("2006-01-02") + "-" + string(rune('a'+index)),
			StartsAt:  date.Add(10 * time.Hour),
			LocalDate: date,
			Score:     score,
			Cost:      4,
			Layout:    "mosaic",
			ItemCount: 5,
		}
	}

	// 30 天每天 1 条，预算 24 → 最多 6 条，且要分散。
	var spread []Moment
	for day := 0; day < 30; day++ {
		spread = append(spread, makeMoment(day, 0, 0.6))
	}
	selected := selectMoments(spread, 24, 0)
	if len(selected) > 6 {
		t.Fatalf("selected %d, budget allows 6", len(selected))
	}
	for i := 1; i < len(selected); i++ {
		gap := selected[i].LocalDate.Sub(selected[i-1].LocalDate).Hours() / 24
		if gap < 1 {
			t.Fatalf("selected items not spread: gap %.0f", gap)
		}
	}

	// 同一天 10 条，单日上限 3 条 / 12 单位。
	var sameDay []Moment
	for i := 0; i < 10; i++ {
		sameDay = append(sameDay, makeMoment(0, i, 0.9))
	}
	selected = selectMoments(sameDay, 24, 0)
	if len(selected) != 3 {
		t.Fatalf("same day selected %d, want 3", len(selected))
	}
	total := 0
	for _, item := range selected {
		total += item.Cost
	}
	if total > maxPerDayCost {
		t.Fatalf("same day cost %d > %d", total, maxPerDayCost)
	}
}

func TestSelectThreshold(t *testing.T) {
	loc := shanghai(t)
	var candidates []Moment
	for i := 0; i < 3; i++ {
		date := time.Date(2025, 6, 1+i, 0, 0, 0, 0, loc)
		candidates = append(candidates, Moment{
			DedupeKey: string(rune('a' + i)),
			StartsAt:  date,
			LocalDate: date,
			Score:     0.1,
			Cost:      2,
		})
	}
	if selected := selectMoments(candidates, 24, 0); len(selected) != 0 {
		t.Fatalf("low value items should be filtered, got %d", len(selected))
	}
}

func TestAmbientCSVWide(t *testing.T) {
	data := []byte("date,resting_hr,sleep_hours,steps\n" +
		"2025-06-01,54,7.2,6840\n" +
		"2025-06-02,55,6.8,7200\n" +
		"2025-06-03,53,7.5,6100\n")
	samples, err := (CSVAmbientExtractor{}).Extract(context.Background(), FileInput{}, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 9 {
		t.Fatalf("got %d samples, want 9", len(samples))
	}
	units := map[string]string{}
	for _, sample := range samples {
		units[sample.Metric] = sample.Unit
	}
	if units["resting_hr"] != "bpm" || units["sleep_hours"] != "h" || units["steps"] != "步" {
		t.Fatalf("unexpected units: %#v", units)
	}
}

func TestAmbientCSVLong(t *testing.T) {
	data := []byte("date,metric,value,unit\n" +
		"2025-06-01,resting_hr,54,bpm\n" +
		"2025-06-02,resting_hr,55,bpm\n" +
		"2025-06-03,resting_hr,53,bpm\n")
	samples, err := (CSVAmbientExtractor{}).Extract(context.Background(), FileInput{}, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("got %d samples, want 3", len(samples))
	}
	if samples[0].Unit != "bpm" {
		t.Fatalf("unit = %q", samples[0].Unit)
	}
}
