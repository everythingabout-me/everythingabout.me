package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/paths"
	"github.com/jackc/pgx/v5"
)

// This is the actual CLI, official S3 SDK and a native HTTP fixture. Provider
// mutation methods are forbidden; the fixture cannot establish live COS guarantees.
func TestStorageAdmitObjectsPostgresCLI(t *testing.T) {
	address := os.Getenv("EAM_TEST_DATABASE_URL")
	if address == "" {
		t.Skip("EAM_TEST_DATABASE_URL unset; disposable object baseline integration not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("eam_object_admit_%x", id[:])
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := admin.Exec(cleanup, "drop database "+quoted+" with (force)"); err != nil {
			t.Error(err)
		}
	}()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	parsed.RawPath = ""
	isolated := parsed.String()
	root := t.TempDir()
	spool := filepath.Join(root, "spool")
	if err = os.Mkdir(spool, 0700); err != nil {
		t.Fatal(err)
	}
	owner := "11111111-1111-1111-1111-111111111111"
	prefix := "drive/" + owner + "/"
	sizes := map[string]int64{prefix + "Empty/": 0, prefix + " a+%2F?#.txt ": 7, prefix + "zero": 0}
	var mu sync.Mutex
	mutations := 0
	heads := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Error("official SDK signing missing")
		}
		w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 00:00:00 GMT")
		switch r.Method {
		case "GET":
			if r.URL.Query().Get("list-type") != "2" {
				t.Error("baseline attempted native file download")
				http.Error(w, "unexpected GET", 500)
				return
			}
			nativePrefix := r.URL.Query().Get("prefix")
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`)
			for _, key := range []string{prefix + " a+%2F?#.txt ", prefix + "Empty/", prefix + "zero"} {
				if strings.HasPrefix(key, nativePrefix) {
					fmt.Fprint(w, "<Contents><Key>")
					_ = xml.EscapeText(w, []byte(key))
					fmt.Fprintf(w, "</Key><Size>%d</Size><LastModified>2026-11-02T00:00:00Z</LastModified><ETag>opaque</ETag></Contents>", sizes[key])
				}
			}
			fmt.Fprint(w, `</ListBucketResult>`)
		case "HEAD":
			heads++
			key := strings.TrimPrefix(r.URL.Path, "/fixture-bucket/")
			size, ok := sizes[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(size))
			w.Header().Set("ETag", `"opaque"`)
			w.WriteHeader(200)
		default:
			mutations++
			t.Errorf("baseline attempted native mutation %s", r.Method)
			w.WriteHeader(500)
		}
	}))
	defer provider.Close()
	admission := objectmutation.Admission{ConnectionID: "objects", Endpoint: provider.URL, Bucket: "fixture-bucket", Region: "test-region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated-http-fixture", AtomicPublish: true, ConditionalCreate: true, SingleGateway: true}
	admissionFile := filepath.Join(root, "admission.json")
	data, _ := json.Marshal(admission)
	if err = os.WriteFile(admissionFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	proof := filepath.Join(root, "proof.json")
	proofDocument := `{"writers":"drained","retainedVersionInventory":"verified-fixture-history-owner-root"}`
	if err = os.WriteFile(proof, []byte(proofDocument), 0600); err != nil {
		t.Fatal(err)
	}
	sqlDir, err := filepath.Abs("../../../sql")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"EAM_SERVER_TEST_PROCESS=1", "EAM_ENV_FILE=" + filepath.Join(root, "missing.env"), "EAM_SQL_DIR=" + sqlDir, "DATABASE_URL=" + isolated, "STORAGE_BACKEND=s3", "STORAGE_CONNECTION_ID=objects", "FILE_COORDINATED_WRITES=true", "FILE_OBJECT_ADMISSION_FILE=" + admissionFile, "FILE_OBJECT_SPOOL_ROOT=" + spool, "S3_ENDPOINT=" + provider.URL, "S3_REGION=test-region", "S3_BUCKET=fixture-bucket", "S3_ACCESS_KEY_ID=fixture-id", "S3_SECRET_ACCESS_KEY=fixture-secret", "S3_PATH_STYLE=true", "USER_QUOTA_BYTES=5"}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestLocalStorageServerCommand$", "--"}, args...)...)
		cmd.Env = env
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for i := 0; i < 2; i++ {
		if out, err := run("migrate"); err != nil {
			t.Fatalf("migrations: %v %s", err, out)
		}
	}
	db, err := pgx.Connect(ctx, isolated)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	var database string
	if err = db.QueryRow(ctx, "select current_database()").Scan(&database); err != nil || database != name {
		t.Fatal("test database isolation failed", err)
	}
	if _, err = db.Exec(ctx, `insert into users(id,email) values($1::uuid,'objects@example.test')`, owner); err != nil {
		t.Fatal(err)
	}
	for key, size := range sizes {
		p := strings.TrimPrefix(key, prefix)
		folder := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		if _, err = db.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size) values($1::uuid,$2,$3,$4,$5,$6,$7)`, owner, key, p, paths.ParentOf(p), paths.NameOf(p), folder, size); err != nil {
			t.Fatal(err)
		}
	}
	var before string
	if err = db.QueryRow(ctx, `select jsonb_agg(to_jsonb(n) order by path)::text from nodes n where owner_id=$1::uuid`, owner).Scan(&before); err != nil {
		t.Fatal(err)
	}
	args := []string{"storage-admit", "--owner-id", owner, "--evidence", proof}
	if out, err := run(append(append([]string(nil), args...), "--physical-bytes", "20", "--dry-run")...); err == nil || !strings.Contains(out, "--writers-quiesced") {
		t.Fatalf("CLI admitted unquiesced writers: %v %s", err, out)
	}
	args = append(args, "--writers-quiesced")
	for _, extra := range [][]string{{"--dry-run"}, {"--physical-bytes", "6", "--dry-run"}} {
		if out, err := run(append(append([]string(nil), args...), extra...)...); err == nil {
			t.Fatalf("underdeclared physical baseline admitted: %s", out)
		}
	}
	if out, err := run(append(append([]string(nil), args...), "--physical-bytes", "20", "--dry-run")...); err != nil || !strings.Contains(out, "logical=7 physical=20") {
		t.Fatalf("object dry-run: %v %s", err, out)
	}
	var count int
	if err = db.QueryRow(ctx, "select count(*) from file_budgets").Scan(&count); err != nil || count != 0 {
		t.Fatal("dry-run wrote budgets", err)
	}
	entries, err := os.ReadDir(spool)
	if err != nil || len(entries) != 0 {
		t.Fatal("dry-run allocated gateway spool", err)
	}
	if out, err := run(append(append([]string(nil), args...), "--physical-bytes", "20")...); err != nil || !strings.Contains(out, "budget initialized") {
		t.Fatalf("object admission: %v %s", err, out)
	}
	var logical, physical int64
	var retains, drained bool
	var evidence json.RawMessage
	if err = db.QueryRow(ctx, `select logical_bytes,physical_bytes,(root_binding->>'RetainsVersions')::boolean,(baseline->>'LegacyWritersDrained')::boolean,baseline->'Evidence' from file_budgets where owner_id=$1::uuid`, owner).Scan(&logical, &physical, &retains, &drained, &evidence); err != nil {
		t.Fatal(err)
	}
	if logical != 7 || physical != 20 || retains || !drained || !strings.Contains(string(evidence), "retainedVersionInventory") {
		t.Fatalf("wrong object baseline logical=%d physical=%d retains=%t drained=%t evidence=%s", logical, physical, retains, drained, evidence)
	}
	if _, err = db.Exec(ctx, `update file_budgets set physical_bytes=25 where owner_id=$1::uuid`, owner); err != nil {
		t.Fatal(err)
	}
	if out, err := run(append(append([]string(nil), args...), "--physical-bytes", "20")...); err != nil || !strings.Contains(out, "already initialized") {
		t.Fatalf("existing object baseline rerun: %v %s", err, out)
	}
	if err = db.QueryRow(ctx, `select physical_bytes from file_budgets where owner_id=$1::uuid`, owner).Scan(&physical); err != nil || physical != 25 {
		t.Fatal("object rerun reset physical history", err)
	}
	var after string
	if err = db.QueryRow(ctx, `select jsonb_agg(to_jsonb(n) order by path)::text from nodes n where owner_id=$1::uuid`, owner).Scan(&after); err != nil || before != after {
		t.Fatal("admission changed index", err)
	}
	entries, err = os.ReadDir(spool)
	if err != nil || len(entries) != 0 {
		t.Fatal("admission allocated gateway spool", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if mutations != 0 || heads == 0 {
		t.Fatalf("native baseline requests mutations=%d heads=%d", mutations, heads)
	}
}
