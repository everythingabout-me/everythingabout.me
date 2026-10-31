package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/auth"
)

// Run the real command entry point in a separate process so signal handling,
// config loading, database startup and ListenAndServe are exercised unchanged.
func TestLocalStorageServerCommand(t *testing.T) {
	if os.Getenv("EAM_SERVER_TEST_PROCESS") != "1" {
		t.Skip("subprocess helper")
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	t.Fatal("missing command arguments")
}

// Explicit opt-in: the URL must permit creating a disposable database. Nothing
// loads .env or inherits application/provider credentials from the parent process.
func TestLocalStorageServerStartupPostgres(t *testing.T) {
	databaseURL := os.Getenv("EAM_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("EAM_TEST_DATABASE_URL is unset; real server startup integration was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("eam_server_test_%x", random[:])
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "drop database "+quoted+" with (force)"); err != nil {
			t.Errorf("remove disposable database: %v", err)
		}
	}()
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("EAM_TEST_DATABASE_URL must be a postgres URL")
	}
	parsed.Path = "/" + name
	parsed.RawPath = ""
	query := parsed.Query()
	query.Del("search_path")
	parsed.RawQuery = query.Encode()
	isolatedURL := parsed.String()

	root := t.TempDir()
	content, system := filepath.Join(root, "content"), filepath.Join(root, "system")
	ownerID := "11111111-1111-1111-1111-111111111111"
	ownerRoot := filepath.Join(content, ownerID)
	for _, dir := range []string{filepath.Join(ownerRoot, "Notes"), filepath.Join(ownerRoot, "empty"), filepath.Join(system, ownerID)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	original := []byte("original bytes\x00\xff\r\nno application decoding\n")
	nativeFile := filepath.Join(ownerRoot, "Notes", " original + report.txt")
	if err := os.WriteFile(nativeFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	sqlDir, err := filepath.Abs("../../../sql")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"EAM_SERVER_TEST_PROCESS=1", "EAM_ENV_FILE=" + filepath.Join(root, "nonexistent.env"),
		"EAM_SQL_DIR=" + sqlDir, "DATABASE_URL=" + isolatedURL,
		"APP_ENV=development", "APP_SECRET=local-startup-test-secret",
		"STORAGE_BACKEND=local", "LOCAL_CONTENT_ROOT=" + content, "LOCAL_SYSTEM_ROOT=" + system,
		"ADDR=" + addr, "WEB_DIR=" + filepath.Join(root, "no-web-build"),
		"COOKIE_SECURE=false", "TIMELINE_ENABLED=true",
	}
	command := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestLocalStorageServerCommand$", "--"}, args...)...)
		cmd.Env = env
		cmd.Dir = root
		return cmd
	}
	run := func(args ...string) string {
		t.Helper()
		output, err := command(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	migrations, err := filepath.Glob(filepath.Join(sqlDir, "*.sql"))
	if err != nil || len(migrations) == 0 {
		t.Fatalf("discover real migrations: %v", err)
	}
	for i := 1; i <= 2; i++ {
		output := run("migrate")
		if !strings.Contains(output, fmt.Sprintf("applied %d migrations", len(migrations))) {
			t.Fatalf("migration pass %d: %s", i, output)
		}
		t.Logf("full migration pass %d succeeded (%d files)", i, len(migrations))
	}
	db, err := pgx.Connect(ctx, isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	password := "local-startup-password"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `insert into users(id,email,password_hash) values($1::uuid,$2,$3)`, ownerID, "startup@example.test", hash); err != nil {
		t.Fatal(err)
	}
	snapshot := func() []string {
		t.Helper()
		rows, err := db.Query(ctx, `select row_to_json(n)::text from nodes n order by owner_id,path`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatal(err)
			}
			result = append(result, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := snapshot()
	output := run("reconcile", "--owner", ownerID, "--dry-run")
	if !strings.Contains(output, "scanned 3 paths, inserted 3") || !reflect.DeepEqual(before, snapshot()) {
		t.Fatalf("dry-run changed index or missed native files: %s", output)
	}
	t.Log(strings.TrimSpace(output))
	output = run("reconcile", "--owner", ownerID)
	if !strings.Contains(output, "scanned 3 paths, inserted 3") {
		t.Fatalf("native import: %s", output)
	}
	t.Log(strings.TrimSpace(output))
	before = snapshot()
	output = run("reconcile", "--owner", ownerID)
	if !strings.Contains(output, "inserted 0, updated 0, unchanged 3") || !reflect.DeepEqual(before, snapshot()) {
		t.Fatalf("repeat import changed index: %s", output)
	}

	logPath := filepath.Join(root, "server.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	server := command("server")
	server.Stdout, server.Stderr = log, log
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- server.Wait() }()
	defer func() {
		_ = server.Process.Signal(os.Interrupt)
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(12 * time.Second):
			_ = server.Process.Kill()
			<-exited
			t.Error("server did not shut down gracefully")
		}
		if data, err := os.ReadFile(logPath); err == nil {
			t.Logf("actual server process log:\n%s", data)
		}
	}()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	base := "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, err := client.Get(base + "/api/health")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"status":"ok"`)) {
				t.Fatalf("health: status=%d body=%s error=%v", response.StatusCode, body, readErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("real server never became healthy: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	request := func(method, path, body string, status int) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", base)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status {
			t.Fatalf("%s %s: status=%d want=%d body=%s error=%v", method, path, response.StatusCode, status, data, err)
		}
		return string(data)
	}
	request("GET", "/api/account", "", http.StatusUnauthorized)
	request("POST", "/api/auth/login", `{"email":"startup@example.test","password":"`+password+`"}`, http.StatusOK)
	parsedBase, _ := url.Parse(base)
	if len(jar.Cookies(parsedBase)) == 0 {
		t.Fatal("password login did not issue a session cookie")
	}
	for _, path := range []string{"/api/auth/me", "/api/account"} {
		if body := request("GET", path, "", http.StatusOK); !strings.Contains(body, ownerID) {
			t.Fatalf("%s lost authenticated owner: %s", path, body)
		}
	}
	if body := request("GET", "/api/fs/list?path=Notes", "", http.StatusOK); !strings.Contains(body, " original + report.txt") {
		t.Fatalf("imported file absent from HTTP list: %s", body)
	}
	for _, mutation := range []struct{ path, body string }{
		{"/api/fs/mkdir", `{"path":"blocked"}`},
		{"/api/fs/rename", `{"path":"Notes","name":"renamed"}`},
		{"/api/fs/delete", `{"paths":["Notes"]}`},
		{"/api/fs/upload/sign", `{"path":"blocked.txt","size":1}`},
		{"/api/fs/upload/confirm", `{"path":"blocked.txt"}`},
	} {
		request("POST", mutation.path, mutation.body, http.StatusNotImplemented)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("startup, reads or rejected legacy mutation changed the file index")
	}
	data, err := os.ReadFile(nativeFile)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("ordinary native read changed original bytes: %x %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(ownerRoot, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("native empty folder was lost: %v", err)
	}
	entries, err := os.ReadDir(ownerRoot)
	if err != nil || len(entries) != 2 {
		t.Fatalf("native root changed: %v %v", entries, err)
	}
	t.Log("real local server: health 200; password session, account and imported listing 200; five legacy mutations 501; index unchanged; original native filename, binary bytes and empty folder preserved")
}
