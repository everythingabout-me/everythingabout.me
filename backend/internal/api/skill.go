package api

import (
	_ "embed"
	"net/http"
)

// The drive skill is the install-time half of the handoff /s/<code> serves, and it is public
// because it carries no credential and must be installable before any connection exists.
//
// It states no token, scope, expiry or transfer class. Those change after a link is minted,
// and /s/<token> already renders them from the live row, so a copy here would be a second
// answer that goes stale precisely where a reader would not think to check. What the live
// page cannot say is how to find a connection at all, and which rules hold for all of them;
// that is the whole document.
//
// It is embedded, not read from disk, so a binary serves the copy it was built with.
//
//go:embed skills/eam-drive/SKILL.md
var driveSkillMarkdown []byte

// handleDriveSkill serves the installable skill. Caching is short and public: the document
// changes only with a deploy, and an agent that installs it gets the current copy.
func handleDriveSkill(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, err := w.Write(driveSkillMarkdown)
	return err
}
