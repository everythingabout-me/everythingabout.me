package main

import (
	"bufio"
	"bytes"
	"context"
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
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/testpg"
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
	scratch := testpg.CreateDatabase(t, "eam_server_test_")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	isolatedURL := scratch.URL

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
	db, err := pgx.Connect(ctx, isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())

	// schema_migrations is the contract; the closing line only reports it. The assertions
	// below match a count fragment ("applied N ") instead of the singular/plural wording
	// ("migration(s)"), so rewording the summary cannot fail a correct run, and compare the
	// recorded rows themselves so a second pass that silently re-executed or re-recorded a
	// file fails even if it prints the right message.
	type migrationRecord struct {
		name, method, appliedAt string
	}
	recorded := func() []migrationRecord {
		t.Helper()
		rows, err := db.Query(ctx, `select name, method, applied_at::text from schema_migrations order by name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []migrationRecord
		for rows.Next() {
			var record migrationRecord
			if err := rows.Scan(&record.name, &record.method, &record.appliedAt); err != nil {
				t.Fatal(err)
			}
			result = append(result, record)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}

	output := run("migrate")
	if !strings.Contains(output, fmt.Sprintf("applied %d ", len(migrations))) {
		t.Fatalf("first migration pass did not report applying all %d files:\n%s", len(migrations), output)
	}
	first := recorded()
	if len(first) != len(migrations) {
		t.Fatalf("first migration pass recorded %d files in %s, want %d:\n%s", len(first), versionTable, len(migrations), output)
	}
	seen := make(map[string]migrationRecord, len(first))
	for _, record := range first {
		seen[record.name] = record
	}
	for _, migration := range migrations {
		name := filepath.Base(migration)
		record, ok := seen[name]
		if !ok {
			t.Fatalf("first migration pass applied %s but did not record it in %s:\n%s", name, versionTable, output)
		}
		if record.method != "executed" {
			t.Fatalf("first migration pass recorded %s as method=%q, want executed: the database started empty", name, record.method)
		}
	}
	t.Logf("full migration pass 1: applied %d files and recorded each as executed", len(migrations))

	output = run("migrate")
	if !strings.Contains(output, "nothing to apply") || !strings.Contains(output, "already recorded") {
		t.Fatalf("second migration pass did not report that every file is already recorded:\n%s", output)
	}
	if second := recorded(); !reflect.DeepEqual(first, second) {
		t.Fatalf("second migration pass changed %s; a recorded file must not be re-executed or re-recorded:\nbefore=%v\nafter=%v", versionTable, first, second)
	}
	t.Logf("full migration pass 2: applied nothing; the %d recorded files are unchanged", len(migrations))
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
	output = run("reconcile", "--owner", ownerID, "--dry-run")
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
	serverExited := false
	defer func() {
		if !serverExited {
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
	if body := request("GET", "/api/files/list?path=%2FNotes", "", http.StatusOK); !strings.Contains(body, " original + report.txt") {
		t.Fatalf("imported file absent from HTTP list: %s", body)
	}
	for _, mutation := range []struct{ path, body string }{
		{"/api/fs/mkdir", `{"path":"blocked"}`},
		{"/api/fs/rename", `{"path":"Notes","name":"renamed"}`},
		{"/api/fs/delete", `{"paths":["Notes"]}`},
		{"/api/fs/upload/sign", `{"path":"blocked.txt","size":1}`},
		{"/api/fs/upload/confirm", `{"path":"blocked.txt"}`},
	} {
		request("POST", mutation.path, mutation.body, http.StatusGone)
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
	t.Log("real local server: health 200; password session, account and imported listing 200; five retired legacy mutations 410; index unchanged; original native filename, binary bytes and empty folder preserved")

	// A backup runs in a goroutine that shutdown deliberately does not wait for.
	// The row is the only trace it leaves, so a stopped process must turn a
	// 'running' row into an explicit interruption instead of leaving it running.
	var backupConnection string
	if err := db.QueryRow(ctx, `select id::text from storage_connections limit 1`).Scan(&backupConnection); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `insert into backup_runs(owner_id, connection_id, snapshot, state) values($1::uuid,$2::uuid,'2020-01-01','running')`, ownerID, backupConnection); err != nil {
		t.Fatal(err)
	}

	// A request that is still reading its body when SIGTERM arrives must be allowed
	// to finish, and the pool must still be open when it does: /api/auth/code reads
	// the JSON body first and only then writes the verification-code row. If shutdown
	// closed the pool when ListenAndServe returned — the behaviour this test exists to
	// prevent — the process would exit with the request in flight and never answer.
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	slowBody := `{"email":"startup@example.test"}`
	half := len(slowBody) / 2
	if _, err := fmt.Fprintf(slow, "POST /api/auth/code HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", addr, base, len(slowBody), slowBody[:half]); err != nil {
		t.Fatal(err)
	}
	// Let the handler reach ReadJSON and block on the missing bytes before signalling.
	time.Sleep(200 * time.Millisecond)
	if err := server.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-exited:
		serverExited = true
		t.Fatalf("server exited with a request still in flight: %v", err)
	default:
	}
	if _, err := io.WriteString(slow, slowBody[half:]); err != nil {
		t.Fatalf("finish the in-flight request: %v", err)
	}
	if err := slow.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(slow), nil)
	if err != nil {
		t.Fatalf("read the response for the request that was in flight during shutdown: %v", err)
	}
	data, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("in-flight request did not finish before the pool closed: status=%d body=%s", response.StatusCode, data)
	}
	select {
	case err := <-exited:
		serverExited = true
		if err != nil {
			t.Errorf("server shutdown: %v", err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("server did not exit after the in-flight request finished")
	}
	var backupState, backupError string
	if err := db.QueryRow(ctx, `select state, coalesce(error,'') from backup_runs where owner_id=$1::uuid and snapshot='2020-01-01'`, ownerID).Scan(&backupState, &backupError); err != nil {
		t.Fatal(err)
	}
	if backupState != "failed" || !strings.Contains(backupError, "stopped before this backup finished") {
		t.Fatalf("interrupted backup was not recorded: state=%s error=%q", backupState, backupError)
	}
	t.Log("SIGTERM: the request still reading its body finished with 200 against a live pool, the interrupted backup was recorded, then the process exited")
}
