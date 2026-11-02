package filecontract

import (
	"net/url"
	"testing"
)

func TestInlineContentURLPreservesNativePathAndConfersNoGrant(t *testing.T) {
	for _, path := range []string{"Photos/ +%?#.png ", "Photos/literal%2F.png", "Photos/日本語.png", "Photos/cafe\u0301.png"} {
		u, err := url.Parse(InlineContentURL(path))
		if err != nil || u.IsAbs() || u.Host != "" || u.Path != Base+"/content" || u.Query().Get("path") != "/"+path || u.Query().Get("disposition") != "inline" || len(u.Query()) != 2 {
			t.Fatalf("path %q escaped authorized read: %v %v", path, u, err)
		}
	}
	for _, invalid := range []string{"", "/absolute.png", "a/../private", "a//b", "a\\b", "a\n.png"} {
		if got := InlineContentURL(invalid); got != "" {
			t.Fatalf("invalid indexed path %q produced URL %q", invalid, got)
		}
	}
}
