package mail

import (
	"net/http"
	"strconv"
	"strings"
)

// RequestLocale derives the email language from the browser's Accept-Language header.
//
// Sign-in, sign-up and email-change all start in the browser, so that header is the only signal we have;
// there is no per-user locale stored yet. The web app itself resolves zh / en the same way, so the email
// matches the page the user is looking at. English is the fallback.
func RequestLocale(r *http.Request) string {
	return localeFromAcceptLanguage(r.Header.Get("Accept-Language"))
}

// localeFromAcceptLanguage returns "zh" or "en" from an Accept-Language value.
//
// It honours q-values (browsers do not always send entries in preference order) and ignores
// everything that is not a Chinese or English tag. Region subtags do not matter: zh-CN, zh-TW and zh-Hans
// all mean Chinese here, since the translated copy is written in Simplified Chinese.
func localeFromAcceptLanguage(header string) string {
	best, bestQ := "", -1.0
	for _, entry := range strings.Split(header, ",") {
		tag, quality := entry, 1.0
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			for _, param := range strings.Split(tag[i+1:], ";") {
				key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || strings.ToLower(strings.TrimSpace(key)) != "q" {
					continue
				}
				if q, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					quality = q
				}
			}
			tag = tag[:i]
		}
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || tag == "*" || quality <= 0 {
			continue
		}

		var locale string
		switch {
		case strings.HasPrefix(tag, "zh"):
			locale = "zh"
		case strings.HasPrefix(tag, "en"):
			locale = "en"
		default:
			continue
		}
		if quality > bestQ {
			best, bestQ = locale, quality
		}
	}
	if best == "" {
		return "en"
	}
	return best
}
