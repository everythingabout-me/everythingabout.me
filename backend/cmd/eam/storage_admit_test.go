package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/testpg"
	"everythingabout.me/backend/internal/treescan"
)

func admissionFixture() ([]treescan.Record, []admitIndexEntry) {
	owner := "11111111-1111-1111-1111-111111111111"
	records := []treescan.Record{{Entry: storage.Entry{Path: " folder ", Kind: storage.Folder}}, {Entry: storage.Entry{Path: " folder / a+%2F?#.txt ", Kind: storage.File, Size: 7}}, {Entry: storage.Entry{Path: "empty", Kind: storage.Folder}}, {Entry: storage.Entry{Path: "zero", Kind: storage.File}}}
	index := make([]admitIndexEntry, 0, len(records))
	for _, r := range records {
		p := string(r.Entry.Path)
		folder := r.Entry.Kind == storage.Folder
		key := paths.KeyFor(p, owner)
		if folder {
			key = paths.DirKeyFor(p, owner)
		}
		index = append(index, admitIndexEntry{Path: p, Key: key, Parent: paths.ParentOf(p), Name: paths.NameOf(p), Folder: folder, Size: r.Entry.Size})
	}
	return records, index
}
func TestStorageAdmissionExactComparison(t *testing.T) {
	records, index := admissionFixture()
	before, _ := json.Marshal(records)
	logical, err := compareAdmissionEntries("11111111-1111-1111-1111-111111111111", records, index)
	if err != nil || logical != 7 {
		t.Fatalf("complete native baseline rejected: %d %v", logical, err)
	}
	after, _ := json.Marshal(records)
	if string(before) != string(after) {
		t.Fatal("comparison rewrote native metadata")
	}
	if !strings.Contains(string(after), " a+%2F?#.txt ") {
		t.Fatal("native spelling was lost")
	}
	// Existing logical root index metadata is optional; it is never a file.
	index = append(index, admitIndexEntry{Path: "", Key: "drive/11111111-1111-1111-1111-111111111111/", Folder: true})
	if _, err := compareAdmissionEntries("11111111-1111-1111-1111-111111111111", records, index); err != nil {
		t.Fatal(err)
	}
}
func TestStorageAdmissionRejectsIncompleteOrMismatchedIndex(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*[]treescan.Record, *[]admitIndexEntry)
	}{
		{"unindexed native", func(_ *[]treescan.Record, i *[]admitIndexEntry) { *i = (*i)[:len(*i)-1] }},
		{"index only", func(r *[]treescan.Record, _ *[]admitIndexEntry) { *r = (*r)[:len(*r)-1] }},
		{"size", func(_ *[]treescan.Record, i *[]admitIndexEntry) { (*i)[1].Size++ }},
		{"kind", func(_ *[]treescan.Record, i *[]admitIndexEntry) { (*i)[1].Folder = true; (*i)[1].Key += "/" }},
		{"owner key", func(_ *[]treescan.Record, i *[]admitIndexEntry) { (*i)[1].Key = "drive/another/secret" }},
		{"trimmed spelling", func(_ *[]treescan.Record, i *[]admitIndexEntry) { (*i)[1].Path = strings.TrimSpace((*i)[1].Path) }},
		{"parent", func(_ *[]treescan.Record, i *[]admitIndexEntry) { (*i)[1].Parent = "" }},
		{"name", func(_ *[]treescan.Record, i *[]admitIndexEntry) { (*i)[1].Name = "alias" }},
		{"duplicate index", func(_ *[]treescan.Record, i *[]admitIndexEntry) { *i = append(*i, (*i)[0]) }},
		{"duplicate native", func(r *[]treescan.Record, _ *[]admitIndexEntry) { *r = append(*r, (*r)[0]) }},
		{"omitted ancestor", func(r *[]treescan.Record, i *[]admitIndexEntry) { *r = (*r)[1:]; *i = (*i)[1:] }},
		{"overflow", func(r *[]treescan.Record, i *[]admitIndexEntry) {
			(*r)[1].Entry.Size = 9007199254740991
			(*i)[1].Size = 9007199254740991
			(*r)[3].Entry.Size = 1
			(*i)[3].Size = 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, i := admissionFixture()
			tc.mutate(&r, &i)
			if _, err := compareAdmissionEntries("11111111-1111-1111-1111-111111111111", r, i); err == nil {
				t.Fatal("unsafe baseline was accepted")
			}
		})
	}
}
func TestStorageAdmitRejectsArgumentsBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{nil, {"--owner-id", "not-a-uuid", "--writers-quiesced", "--evidence", "missing"}, {"--owner-id", "11111111-1111-1111-1111-111111111111", "--evidence", "missing"}, {"--owner-id", "11111111-1111-1111-1111-111111111111", "--writers-quiesced"}, {"--owner-id", "11111111-1111-1111-1111-111111111111", "--writers-quiesced", "--evidence", "missing", "--physical-limit", "-1"}, {"--unexpected"}} {
		if err := runStorageAdmit(args); err == nil {
			t.Fatalf("unsafe arguments accepted: %v", args)
		}
	}
}
func TestAdmissionEvidenceIsBoundedNonemptyJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proof.json")
	for _, raw := range []string{"", "null", "{}", "[]", "true", `"proof"`, `{"proof":true} {}`, strings.Repeat(" ", maxAdmissionEvidenceBytes+1)} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readAdmissionEvidence(path); err == nil {
			t.Fatalf("invalid proof accepted: %d bytes", len(raw))
		}
	}
	if err := os.WriteFile(path, []byte(`{"quiescence":"operator-verified","backup":"available"}`), 0600); err != nil {
		t.Fatal(err)
	}
	proof, err := readAdmissionEvidence(path)
	if err != nil || !json.Valid(proof) {
		t.Fatalf("valid proof rejected %v", err)
	}
	if _, err := readAdmissionEvidence(t.TempDir()); err == nil {
		t.Fatal("directory proof accepted")
	}
}
func TestStorageAdmitPostgresCLI(t *testing.T) {
	scratch := testpg.CreateDatabase(t, "eam_admit_test_")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	isolated := scratch.URL
	var err error
	root := t.TempDir()
	content, system := filepath.Join(root, "content"), filepath.Join(root, "system")
	owner := "11111111-1111-1111-1111-111111111111"
	cp, sp := filepath.Join(content, owner), filepath.Join(system, owner)
	for _, p := range []string{filepath.Join(cp, " folder "), filepath.Join(cp, "empty"), sp} {
		if err = os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	native := filepath.Join(cp, " folder ", " a+%2F?#.txt ")
	original := []byte("0123456")
	if err = os.WriteFile(native, original, 0600); err != nil {
		t.Fatal(err)
	}
	proof := filepath.Join(root, "proof.json")
	if err = os.WriteFile(proof, []byte(`{"writers":"drained","native":"verified","backup":"retained"}`), 0600); err != nil {
		t.Fatal(err)
	}
	sqlDir, err := filepath.Abs("../../../sql")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"EAM_SERVER_TEST_PROCESS=1", "EAM_ENV_FILE=" + filepath.Join(root, "absent.env"), "EAM_SQL_DIR=" + sqlDir, "DATABASE_URL=" + isolated, "APP_ENV=development", "STORAGE_BACKEND=local", "FILE_COORDINATED_WRITES=true", "LOCAL_CONTENT_ROOT=" + content, "LOCAL_SYSTEM_ROOT=" + system, "USER_QUOTA_BYTES=5"}
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
	if _, err = db.Exec(ctx, `insert into users(id,email) values($1::uuid,'admit@example.test')`, owner); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"storage-admit", "--owner-id", owner, "--writers-quiesced", "--evidence", proof}
	if out, err := run(append(arguments, "--dry-run")...); err == nil || !strings.Contains(out, "not indexed") {
		t.Fatalf("unindexed content admitted: %v %s", err, out)
	}
	// Reconcile must remain read-only even while the global write flag is enabled.
	if out, err := run("reconcile", "--owner", owner); err != nil {
		t.Fatalf("index fixture: %v %s", err, out)
	}
	var before string
	if err = db.QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(n) order by path),'[]'::jsonb)::text from nodes n where owner_id=$1::uuid`, owner).Scan(&before); err != nil {
		t.Fatal(err)
	}
	assertUntouched := func() {
		t.Helper()
		var after string
		if err := db.QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(n) order by path),'[]'::jsonb)::text from nodes n where owner_id=$1::uuid`, owner).Scan(&after); err != nil || after != before {
			t.Fatalf("index changed: %v", err)
		}
		got, err := os.ReadFile(native)
		if err != nil || string(got) != string(original) {
			t.Fatalf("native bytes changed: %v", err)
		}
	}
	if out, err := run(append(arguments, "--dry-run")...); err != nil || !strings.Contains(out, "[dry-run] baseline verified") {
		t.Fatalf("dry-run: %v %s", err, out)
	}
	var budgets int
	if err = db.QueryRow(ctx, `select count(*) from file_budgets`).Scan(&budgets); err != nil || budgets != 0 {
		t.Fatal("dry-run wrote budget", err)
	}
	entries, err := os.ReadDir(sp)
	if err != nil || len(entries) != 0 {
		t.Fatal("dry-run allocated system state", err)
	}
	assertUntouched()
	// Reject unknown staging without deleting it.
	known := filepath.Join(sp, "retained-unknown")
	if err = os.WriteFile(known, []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(arguments...); err == nil || !strings.Contains(out, "empty system root") {
		t.Fatalf("nonempty system admitted: %v %s", err, out)
	}
	if got, _ := os.ReadFile(known); string(got) != "evidence" {
		t.Fatal("unknown staging was deleted")
	}
	// Remove only the exact disposable test fixture after checking its parent.
	if filepath.Dir(known) != sp {
		t.Fatal("unexpected fixture cleanup target")
	}
	if err = os.Remove(known); err != nil {
		t.Fatal(err)
	}
	if out, err := run(arguments...); err != nil || !strings.Contains(out, "budget initialized") {
		t.Fatalf("admission: %v %s", err, out)
	}
	var logical, physical, limit int64
	var evidence json.RawMessage
	if err = db.QueryRow(ctx, `select logical_bytes,physical_bytes,logical_limit,baseline->'Evidence' from file_budgets where owner_id=$1::uuid`, owner).Scan(&logical, &physical, &limit, &evidence); err != nil {
		t.Fatal(err)
	}
	if logical != 7 || physical != 7 || limit != 5 || !json.Valid(evidence) {
		t.Fatalf("bad baseline %d %d %d %s", logical, physical, limit, evidence)
	}
	assertUntouched()
	// Idempotent rerun does not reset usage or require an empty staging root later.
	if _, err = db.Exec(ctx, `update file_budgets set logical_bytes=9 where owner_id=$1::uuid`, owner); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(known, []byte("post-admission ledger"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(arguments...); err != nil || !strings.Contains(out, "already initialized") {
		t.Fatalf("repeat admission: %v %s", err, out)
	}
	if err = db.QueryRow(ctx, `select logical_bytes from file_budgets where owner_id=$1::uuid`, owner).Scan(&logical); err != nil || logical != 9 {
		t.Fatal("rerun reset established usage", err)
	}
	env = append(env, "STORAGE_GENERATION=2")
	if out, err := run(arguments...); err == nil || !strings.Contains(out, "different root binding") {
		t.Fatalf("root replacement admitted: %v %s", err, out)
	}
	assertUntouched()
}

func TestAdmissionSystemOwnerSymlinkRejected(t *testing.T) {
	content, system := t.TempDir(), t.TempDir()
	owner := "11111111-1111-1111-1111-111111111111"
	cp, sp := filepath.Join(content, owner), filepath.Join(system, owner)
	other := filepath.Join(system, "22222222-2222-2222-2222-222222222222")
	if err := os.Mkdir(cp, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, sp); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Storage: config.Storage{Local: config.Local{ContentRoot: content, SystemRoot: system}}}
	binding := storage.Config{OwnerID: owner, ConnectionID: "fixture", Generation: 1, ContentRoot: cp, SystemRoot: sp}
	if err := emptyAdmissionSystemRoot(context.Background(), cfg, binding); err == nil {
		t.Fatal("system owner symlink was followed")
	}
	info, err := os.Lstat(sp)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("rejected symlink was mutated")
	}
}

func TestAdmissionSystemRootMustStayEmptyAndAnchored(t *testing.T) {
	content, system := t.TempDir(), t.TempDir()
	owner := "11111111-1111-1111-1111-111111111111"
	cp, sp := filepath.Join(content, owner), filepath.Join(system, owner)
	if err := os.Mkdir(cp, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sp, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Storage: config.Storage{Local: config.Local{ContentRoot: content, SystemRoot: system}}}
	binding := storage.Config{OwnerID: owner, ConnectionID: "fixture", Generation: 1, ContentRoot: cp, SystemRoot: sp}
	if err := emptyAdmissionSystemRoot(context.Background(), cfg, binding); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(sp, "unknown-ledger")
	if err := os.WriteFile(unknown, []byte("retained evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := emptyAdmissionSystemRoot(context.Background(), cfg, binding); err == nil {
		t.Fatal("unknown temporary data admitted")
	}
	data, err := os.ReadFile(unknown)
	if err != nil || string(data) != "retained evidence" {
		t.Fatal("system evidence modified")
	}
}
