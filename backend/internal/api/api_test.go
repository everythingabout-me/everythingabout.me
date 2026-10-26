package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/jobs"
	"everythingabout.me/backend/internal/nodes"
)

// 确认 /files/{path...} 的几件事：斜杠保留、%20 解码、HEAD 优先于 GET。
func TestFilesRoute(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "GET:%s", r.PathValue("path"))
	})
	mux.HandleFunc("HEAD /files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handler", "head")
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name       string
		method     string
		target     string
		wantBody   string
		wantHeader string
	}{
		{"多级路径", http.MethodGet, "/files/Photos/2024/a.jpg", "GET:Photos/2024/a.jpg", ""},
		{"百分号编码", http.MethodGet, "/files/A%20B.txt", "GET:A B.txt", ""},
		{"中文", http.MethodGet, "/files/%E7%BE%BD%E6%AF%9B%E7%90%83/a.mp4", "GET:羽毛球/a.mp4", ""},
		{"HEAD 走自己的 handler", http.MethodHead, "/files/a.jpg", "", "head"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.target, nil)
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)

			if tc.wantHeader != "" {
				if got := recorder.Header().Get("X-Handler"); got != tc.wantHeader {
					t.Fatalf("handler = %q, want %q", got, tc.wantHeader)
				}
				return
			}
			if got := recorder.Body.String(); got != tc.wantBody {
				t.Fatalf("body = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

func TestIsAPIPath(t *testing.T) {
	yes := []string{
		"/capabilities", "/list", "/me", "/mkdir", "/move", "/copy", "/search",
		"/files", "/files/a/b", "/upload", "/upload/complete", "/upload/u_1",
		"/move/m_1", "/copy/c_1", "/copy/c_1/cancel",
	}
	no := []string{"/", "/api/fs/list", "/filesx", "/login", "/capabilitiesx"}

	for _, path := range yes {
		if !isAPIPath(path) {
			t.Errorf("%s 应该被当成 API 路径", path)
		}
	}
	for _, path := range no {
		if isAPIPath(path) {
			t.Errorf("%s 不应该被当成 API 路径", path)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	row := nodes.Row{Node: nodes.Node{ID: "7f3a9c2b-0000-0000-0000-000000000000", Name: "a.txt"}}
	encoded := encodeCursor(&row)
	decoded, err := decodeCursor(encoded)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if decoded.ID != row.ID || decoded.IsDir != row.IsDir || decoded.Name != row.Name {
		t.Fatalf("往返不一致: %+v", decoded)
	}

	if _, err := decodeCursor("not-base64!!"); err == nil {
		t.Fatal("非法 cursor 应该报错")
	}
	if cursor, err := decodeCursor(""); err != nil || cursor != nil {
		t.Fatal("空 cursor 应该返回 (nil, nil)")
	}
}

func TestNewUploadID(t *testing.T) {
	id, err := newUploadID()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(id) != 2+24 || id[:2] != "u_" {
		t.Fatalf("形状不对: %q", id)
	}
	other, err := newUploadID()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if id == other {
		t.Fatal("两次生成的 uploadId 相同")
	}
}

func TestScopePrefix(t *testing.T) {
	limited := &auth.Principal{Token: &auth.Token{Prefix: "Apps/x", CanRead: true, CanWrite: true}}
	full := &auth.Principal{}

	cases := []struct {
		name      string
		principal *auth.Principal
		request   string
		want      string
		wantErr   bool
	}{
		{"令牌没给 path，用它自己的前缀", limited, "", "Apps/x", false},
		{"令牌给了前缀内的 path", limited, "Apps/x/sub", "Apps/x/sub", false},
		{"令牌给了前缀外的 path", limited, "Apps/y", "", true},
		{"全权限空 path", full, "", "", false},
		{"全权限任意 path", full, "any/path", "any/path", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scopePrefix(tc.principal, tc.request)
			if tc.wantErr {
				if err == nil {
					t.Fatal("应该报错")
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParsePrefer(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		requested bool
		wait      time.Duration
	}{
		{"没有头", "", false, 0},
		{"只要求异步", "respond-async", true, 0},
		{"平铺 wait", "respond-async, wait=10", true, 10 * time.Second},
		{"分号 wait", "respond-async; wait=3", true, 3 * time.Second},
		{"wait 0", "respond-async,wait=0", true, 0},
		{"wait 超过上限", "respond-async, wait=999", true, maxAsyncWait},
		{"wait 不是数字", "respond-async, wait=abc", true, 0},
		{"别的偏好不认", "return=minimal", false, 0},
		{"大小写不敏感", "Respond-Async, Wait=5", true, 5 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePrefer(tc.header)
			if got.Requested != tc.requested || got.Wait != tc.wait {
				t.Fatalf("parsePrefer(%q) = %+v, want requested=%v wait=%v",
					tc.header, got, tc.requested, tc.wait)
			}
		})
	}
}

func TestPreferBudget(t *testing.T) {
	if got := (preferAsync{}).Budget(); got != defaultSyncBudget {
		t.Fatalf("没带 Prefer 应该等 %v，实际 %v", defaultSyncBudget, got)
	}
	if got := (preferAsync{Requested: true}).Budget(); got != 0 {
		t.Fatalf("respond-async 不带 wait 应该立刻回 202，实际等 %v", got)
	}
	if got := (preferAsync{Requested: true, Wait: 5 * time.Second}).Budget(); got != 5*time.Second {
		t.Fatalf("wait=5 应该等 5s，实际 %v", got)
	}
}

func TestSourceAndTarget(t *testing.T) {
	full := &auth.Principal{}
	scoped := &auth.Principal{Token: &auth.Token{Prefix: "Apps/x", CanRead: true, CanWrite: true}}

	cases := []struct {
		name    string
		p       *auth.Principal
		from    string
		to      string
		wantErr bool
	}{
		{"正常", full, "a.md", "b/a.md", false},
		{"根目录不行", full, "", "b", true},
		{"路径没变", full, "a.md", "a.md", true},
		{"目标在源里面", full, "Docs", "Docs/sub", true},
		{"令牌外", scoped, "Apps/y/a.md", "Apps/y/b.md", true},
		{"令牌内", scoped, "Apps/x/a.md", "Apps/x/b.md", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := sourceAndTarget(tc.from, tc.to, tc.p)
			if tc.wantErr && err == nil {
				t.Fatal("应该报错")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不该报错: %v", err)
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	s := &Server{cfg: &config.Config{
		QuotaBytes:         1024,
		MultipartThreshold: 5 << 30,
		UploadPartSize:     64 << 20,
	}}
	recorder := httptest.NewRecorder()
	if err := s.handleAPICapabilities(recorder, httptest.NewRequest(http.MethodGet, "/capabilities", nil)); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	for _, key := range []string{
		"maxPutBytes", "maxUploadBytes", "multipartThreshold", "partSize",
		"maxParts", "list", "rateLimit", "quotaBytes",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("缺少字段 %s", key)
		}
	}
	if got["quotaBytes"].(float64) != 1024 {
		t.Errorf("quotaBytes = %v", got["quotaBytes"])
	}
	if async, ok := got["async"].(map[string]any); !ok || async["value"] != "respond-async" {
		t.Errorf("async 字段不对: %v", got["async"])
	}
}

func TestWriteJobResult(t *testing.T) {
	cases := []struct {
		name     string
		job      *jobs.Job
		status   int
		location bool
	}{
		{"还没做完", &jobs.Job{ID: "m_1", Status: "running"}, http.StatusAccepted, true},
		{"做完了", &jobs.Job{ID: "m_1", Status: "done"}, http.StatusOK, true},
		{"取消了", &jobs.Job{ID: "m_1", Status: "cancelled"}, http.StatusOK, true},
		{"失败了", &jobs.Job{ID: "m_1", Status: "failed", Error: "boom"}, http.StatusInternalServerError, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeJobResult(recorder, "move", tc.job)

			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.status)
			}
			location := recorder.Header().Get("Location")
			if tc.location && location != "/move/m_1" {
				t.Fatalf("Location = %q", location)
			}
			if tc.status == http.StatusAccepted && recorder.Header().Get("Retry-After") == "" {
				t.Fatal("202 应该带 Retry-After")
			}
		})
	}
}

// Handler 里 ServeMux 的路由冲突会 panic，这个测试就是那道闸。
func TestHandlerRegistersRoutes(t *testing.T) {
	if handler := (&Server{}).Handler(); handler == nil {
		t.Fatal("Handler 返回 nil")
	}
}
