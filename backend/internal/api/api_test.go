package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"everythingabout.me/backend/internal/auth"
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
	yes := []string{"/list", "/me", "/move", "/files", "/files/a/b", "/upload", "/upload/complete"}
	no := []string{"/", "/api/fs/list", "/filesx", "/login"}

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
