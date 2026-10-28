package mail

import "testing"

func TestLocaleFromAcceptLanguage(t *testing.T) {
	cases := map[string]string{
		"":                           "en",
		"en-US,en;q=0.9":             "en",
		"zh-CN,zh;q=0.9,en;q=0.8":    "zh",
		"zh-TW,zh;q=0.9,en-US;q=0.8": "zh",
		"en;q=0.2,zh;q=0.9":          "zh",
		"zh;q=0.2,en;q=0.9":          "en",
		"fr-FR,fr;q=0.9,en;q=0.8":    "en",
		"fr-FR,fr;q=0.9,de;q=0.8":    "en",
		"*":                          "en",
		"zh-CN;q=0":                  "en",
		"en-GB,zh-Hans-CN;q=0.7":     "en",
		" ZH-cn , en ; q=0.5":        "zh",
	}
	for header, want := range cases {
		if got := localeFromAcceptLanguage(header); got != want {
			t.Errorf("localeFromAcceptLanguage(%q) = %q, want %q", header, got, want)
		}
	}
}
