package filecontract

import "net/url"

// InlineContentURL converts an indexed root-relative path into an origin-relative
// read URL. It carries no credential or provider grant; the content endpoint must
// authenticate and authorize every request. Unsafe media retain attachment handling.
func InlineContentURL(relative string) string {
	p, err := FromRelative(relative)
	if err != nil || p == "/" {
		return ""
	}
	return Base + "/content?" + url.Values{"path": {string(p)}, "disposition": {"inline"}}.Encode()
}
