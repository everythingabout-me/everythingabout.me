package events

import (
	"strings"
	"testing"
	"time"
)

func baseInput() AppInput {
	return AppInput{
		ExternalID: "run-1",
		StartsAt:   time.Date(2025, 8, 3, 7, 12, 0, 0, time.UTC),
		TZ:         "Asia/Shanghai",
		Precision:  "minute",
		Title:      "晨跑 · 北岸",
		Type:       TypeMetric,
		Payload: PayloadIn{
			Metrics: []Metric{{Label: "距离", Value: "12.4", Unit: "km"}},
		},
	}
}

// 校验是唯一的写入口：网页、流水线、三方应用都从这里过。
// 这些用例就是 docs/design/events.md §3 / §6.4 的限额表。
func TestValidateApp(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*AppInput)
		ok     bool
	}{
		{"valid metric", func(*AppInput) {}, true},
		{"empty title", func(in *AppInput) { in.Title = "" }, false},
		{"long title", func(in *AppInput) { in.Title = strings.Repeat("字", MaxTitle+1) }, false},
		{"photo without media", func(in *AppInput) { in.Type = TypePhoto }, false},
		{"photo with 5 media", func(in *AppInput) {
			in.Type = TypePhoto
			in.Payload.Metrics = nil
			in.Payload.Media = []string{"a", "b", "c", "d", "e"}
		}, false},
		{"photo with 4 media", func(in *AppInput) {
			in.Type = TypePhoto
			in.Payload.Metrics = nil
			in.Payload.Media = []string{"a", "b", "c", "d"}
		}, true},
		{"note without body", func(in *AppInput) { in.Type = TypeNote }, false},
		{"note with body", func(in *AppInput) {
			in.Type = TypeNote
			in.Payload.Metrics = nil
			in.Payload.Body = "搬家。"
		}, true},
		{"note with media", func(in *AppInput) {
			in.Type = TypeNote
			in.Payload.Metrics = nil
			in.Payload.Body = "搬家。"
			in.Payload.Media = []string{"a"}
		}, false},
		{"metric with 4", func(in *AppInput) {
			in.Payload.Metrics = []Metric{{Label: "a", Value: "1"}, {Label: "b", Value: "2"}, {Label: "c", Value: "3"}, {Label: "d", Value: "4"}}
		}, false},
		{"html without height", func(in *AppInput) {
			in.Type = TypeHTML
			in.Payload.Metrics = nil
			in.Payload.HTML = "<div>x</div>"
			in.Payload.Height = 0
		}, true},
		{"html too tall", func(in *AppInput) {
			in.Type = TypeHTML
			in.Payload.Metrics = nil
			in.Payload.HTML = "<div>x</div>"
			in.Payload.Height = MaxHTMLSize + 1
		}, false},
		{"html too big", func(in *AppInput) {
			in.Type = TypeHTML
			in.Payload.Metrics = nil
			in.Payload.HTML = strings.Repeat("x", MaxHTMLLen+1)
		}, false},
		{"http detail url", func(in *AppInput) { in.DetailURL = "http://example.com" }, false},
		{"https detail url", func(in *AppInput) { in.DetailURL = "https://example.com/a" }, true},
		{"ends before starts", func(in *AppInput) {
			earlier := in.StartsAt.Add(-time.Hour)
			in.EndsAt = &earlier
		}, false},
		{"bad precision", func(in *AppInput) { in.Precision = "week" }, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.mutate(&in)
			err := ValidateApp(&in)
			if tc.ok && err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	cursor := encodeCursor("2025-08-03T07:12:00Z", "0f1c2d3e-0000-0000-0000-000000000000")
	at, id, err := decodeCursor(cursor)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if at != "2025-08-03T07:12:00Z" || id != "0f1c2d3e-0000-0000-0000-000000000000" {
		t.Fatalf("round trip mismatch: %q %q", at, id)
	}
	if _, _, err := decodeCursor("not-a-cursor"); err == nil {
		t.Fatal("expected an error for a bad cursor")
	}
}
