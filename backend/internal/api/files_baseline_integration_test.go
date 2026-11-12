package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5"
)

// The automatic baseline exists for exactly one case: an owner with no native
// content and no index/accounting row. This opt-in suite drives the real HTTP,
// session/CSRF and Postgres path for that case and for the fail-closed neighbor.
func TestEmptyOwnerAutomaticBaseline(t *testing.T) {
	databaseURL := testpg.Require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var entropy [8]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	database := "eam_baseline_test_" + hex.EncodeToString(entropy[:])
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err = admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.Exec(context.Background(), "drop database "+quoted+" with (force)"); e != nil {
			t.Error(e)
		}
	}()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	db, err := store.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) < 20 {
		t.Fatalf("migrations unavailable: %d %v", len(migrations), err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, migration := range migrations {
			data, e := os.ReadFile(migration)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = db.Pool().Exec(ctx, string(data)); e != nil {
				t.Fatalf("migration %s pass %d: %v", filepath.Base(migration), pass+1, e)
			}
		}
	}
	cfg := &config.Config{
		AppSecret:         "empty-owner-baseline-test-secret",
		SessionTTLDays:    1,
		FileCookieOrigins: []string{"https://files.example"},
		Storage: config.Storage{Backend: "local", ConnectionID: "baseline-runtime", Generation: 1, CoordinatedWrites: true,
			Local: config.Local{ContentRoot: t.TempDir(), SystemRoot: t.TempDir()}},
	}
	backend, err := storagebackend.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	ownerDir := func(owner string, content bool) {
		t.Helper()
		for _, base := range []string{cfg.Storage.Local.ContentRoot, cfg.Storage.Local.SystemRoot} {
			if err = os.Mkdir(filepath.Join(base, owner), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if content {
			if err = os.WriteFile(filepath.Join(cfg.Storage.Local.ContentRoot, owner, "keep.txt"), []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	newOwner := func(name string) string {
		t.Helper()
		var id string
		if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values($1,'') returning id::text`, name+"@example.test").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	service := auth.New(db, cfg)
	cookieFor := func(owner string) *http.Cookie {
		t.Helper()
		value, _, e := service.CreateSession(ctx, owner, "empty-owner baseline fixture", "")
		if e != nil {
			t.Fatal(e)
		}
		return &http.Cookie{Name: auth.SessionCookie, Value: value}
	}
	server := New(cfg, db, backend.Legacy(), nil)
	server.SetStorageFactory(backend)
	handler := server.Handler()
	do := func(method, target string, cookie *http.Cookie, headers http.Header, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://files.example"+target, bytes.NewReader(body)).WithContext(ctx)
		if headers != nil {
			r.Header = headers.Clone()
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	me := func(cookie *http.Cookie) map[string]any {
		t.Helper()
		w := do("GET", "/api/files/me", cookie, nil, nil)
		if w.Code != 200 {
			t.Fatalf("GET /api/files/me = %d body=%s", w.Code, w.Body.String())
		}
		var page map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	mkdir := func(cookie *http.Cookie, path string) *httptest.ResponseRecorder {
		t.Helper()
		csrf, _ := me(cookie)["csrfToken"].(string)
		if csrf == "" {
			t.Fatal("session response omitted the CSRF token")
		}
		data, _ := json.Marshal(map[string]any{"kind": "folder", "path": path, "parents": false})
		return do("POST", "/api/files", cookie, http.Header{"Origin": {"https://files.example"}, "X-CSRF-Token": {csrf}, "Content-Type": {"application/json"}}, data)
	}
	budget := func(owner string) (logical, physical int64, found bool) {
		t.Helper()
		err = db.Pool().QueryRow(ctx, `select logical_bytes,physical_bytes from file_budgets where owner_id=$1`, owner).Scan(&logical, &physical)
		if err == pgx.ErrNoRows {
			return 0, 0, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return logical, physical, true
	}

	t.Run("first write admits an empty owner", func(t *testing.T) {
		owner := newOwner("fresh")
		ownerDir(owner, false)
		cookie := cookieFor(owner)
		if ready, _ := me(cookie)["write"].(bool); ready {
			t.Fatal("owner was writable before any admission")
		}
		if w := mkdir(cookie, "/Notes"); w.Code != 201 {
			t.Fatalf("first write = %d body=%s", w.Code, w.Body.String())
		}
		logical, physical, found := budget(owner)
		if !found || logical != 0 || physical != 0 {
			t.Fatalf("automatic baseline not zero: found=%v logical=%d physical=%d", found, logical, physical)
		}
		if ready, _ := me(cookie)["write"].(bool); !ready {
			t.Fatal("owner not writable after automatic admission")
		}
	})

	t.Run("sign-up admission avoids the first-write scan", func(t *testing.T) {
		owner := newOwner("signup")
		ownerDir(owner, false)
		server.filesProvisionNewOwner(ctx, owner)
		if _, _, found := budget(owner); !found {
			t.Fatal("sign-up admission wrote no baseline")
		}
		if ready, _ := me(cookieFor(owner))["write"].(bool); !ready {
			t.Fatal("sign-up owner is not writable")
		}
	})

	t.Run("native content still fails closed", func(t *testing.T) {
		owner := newOwner("has-content")
		ownerDir(owner, true)
		if w := mkdir(cookieFor(owner), "/Notes"); w.Code != 503 {
			t.Fatalf("non-empty owner write = %d body=%s", w.Code, w.Body.String())
		} else if !bytes.Contains(w.Body.Bytes(), []byte("baseline_required")) {
			t.Fatalf("unexpected Problem: %s", w.Body.String())
		}
		if _, _, found := budget(owner); found {
			t.Fatal("non-empty owner was given an automatic baseline")
		}
	})
}
