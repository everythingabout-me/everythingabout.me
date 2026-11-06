package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/webui"
)

// TestFilesBrowserAcceptance uses the built SPA and an actual Chromium process.
// It never loads config/environment files or connects to production storage.
func TestFilesBrowserAcceptance(t *testing.T) {
	module := os.Getenv("EAM_TEST_PLAYWRIGHT_MODULE")
	if module == "" {
		t.Skip("EAM_TEST_PLAYWRIGHT_MODULE unset; actual browser acceptance not run")
	}
	f := newFileWorkerFixture(t)
	root, _ := f.owner(t)
	// The interrupted upload uses one full part plus one byte. Local chunks coexist
	// with an assembled stage, and every session reserves its own bytes twice.
	// Automatic restart allocates before the abandoned session's asynchronous
	// cleanup, so budget both sessions plus the files retained by later checks.
	logicalLimit := 4 * filesUploadPartSize
	physicalLimit := 4 * logicalLimit
	if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=$2,physical_limit=$3 where owner_id=$1`, root.OwnerID, logicalLimit, physicalLimit); err != nil {
		t.Fatal(err)
	}
	cookie, _, err := f.auth.CreateSession(f.ctx, root.OwnerID, "disposable browser acceptance", "")
	if err != nil {
		t.Fatal(err)
	}
	bearer, _ := f.token(t, root.OwnerID, "Scope", true, true)
	webDir, err := filepath.Abs("../../../web/dist")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(webDir, "index.html")); err != nil {
		t.Fatal("build web before browser acceptance:", err)
	}
	server := New(f.cfg, f.db, f.backend.Legacy(), webui.New(webDir))
	server.SetStorageFactory(f.backend)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	f.cfg.FileCookieOrigins = []string{httpServer.URL}
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	server.StartFileOperations(ctx)
	script, err := filepath.Abs("../../../web/tests/browser-acceptance.mjs")
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"url": httpServer.URL, "cookie": cookie, "bearer": bearer, "module": module, "chrome": os.Getenv("EAM_TEST_CHROME")})
	cmd := exec.CommandContext(f.ctx, "node", script)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser acceptance: %v\n%s", err, output)
	}
	t.Log(string(output))
}
