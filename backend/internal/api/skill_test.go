package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/config"
)

// The drive skill is the one artifact an agent installs before it has any credential, so the
// test covers the three things that decide whether it works: the route reaches it rather than
// the SPA, the file parses as a skill, and the body says where the live half is. A document
// that installs but never loads, or loads and never points at /s/, is a dead end that looks fine
// in a browser.
func TestDriveSkillServesInstallableMarkdown(t *testing.T) {
	s := &Server{cfg: &config.Config{}, web: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the SPA answered /skill")
	})}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/skill", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /skill: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("GET /skill: content type %q, want text/markdown", ct)
	}

	body := rec.Body.String()
	name, description := skillFrontmatter(t, body)
	if name != "eam-drive" {
		t.Fatalf("frontmatter name %q, want eam-drive", name)
	}
	// A description is the only thing a catalog shows before the skill loads, so a stub that
	// names no trigger would never route.
	if len([]rune(description)) < 80 {
		t.Fatalf("frontmatter description is too short to route on: %q", description)
	}
	// The second run is seamless only because the first run saved a connection somewhere the next
	// session will look. A reader who is told to paste a link but not where to keep it costs the user
	// the same paste every time, which is the whole thing the skill exists to remove.
	for _, want := range []string{"/s/", "/skill", "/settings/connect", "XDG_CONFIG_HOME", "/eam/drive"} {
		if !strings.Contains(body, want) {
			t.Fatalf("skill body never mentions %s, so a reader cannot reach the live instructions or keep the connection", want)
		}
	}
}

// The skill is public and installable by anyone, so a token that leaked into it would be a
// credential published at a stable URL.
func TestDriveSkillCarriesNoCredential(t *testing.T) {
	secret := regexp.MustCompile(`eam_[a-z0-9]{6,}_[A-Za-z0-9]{16,}`)
	if match := secret.FindString(string(driveSkillMarkdown)); match != "" {
		t.Fatalf("skill document contains a token: %s", match)
	}
}

// skillFrontmatter returns the two required fields, failing if the document cannot be installed
// as a skill at all.
func skillFrontmatter(t *testing.T, doc string) (string, string) {
	t.Helper()
	rest, ok := strings.CutPrefix(doc, "---\n")
	if !ok {
		t.Fatal("skill document does not start with YAML frontmatter")
	}
	header, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("skill document never closes its frontmatter")
	}
	if strings.TrimSpace(body) == "" {
		t.Fatal("skill document has no body beyond the frontmatter")
	}
	var name, description string
	for _, line := range strings.Split(header, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "name":
			name = value
		case "description":
			description = value
		}
	}
	if name == "" || description == "" {
		t.Fatalf("frontmatter is missing name or description: %q", header)
	}
	return name, description
}
