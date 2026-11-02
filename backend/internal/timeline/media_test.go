package timeline

import (
	"net/url"
	"testing"
)

func TestHydrateMonthReplacesHistoricalProviderGrants(t *testing.T) {
	month := &Month{Items: []Item{{Cover: &Thumb{Path: "Photos/ +%2F?#.png", URL: "https://old-provider.example/cover?signature=old"}, Grid: []Thumb{{Path: "Movies/a.mp4", Video: true, URL: "https://old-provider.example/video"}, {Path: "", URL: "https://old-provider.example/unsafe"}}}}}
	// Nil storage deliberately proves hydration cannot issue a native URL grant.
	(&Service{}).hydrateMonth(month)
	for _, thumb := range []*Thumb{month.Items[0].Cover, &month.Items[0].Grid[0]} {
		u, err := url.Parse(thumb.URL)
		if err != nil || u.IsAbs() || u.Host != "" || u.Path != "/api/files/content" || u.Query().Get("path") != "/"+thumb.Path {
			t.Fatalf("timeline media escaped authorized content: %+v %v", thumb, err)
		}
	}
	if month.Items[0].Grid[1].URL != "" {
		t.Fatal("invalid path retained a historical provider URL")
	}
}
