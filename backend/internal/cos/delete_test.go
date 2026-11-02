package cos

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

func TestRemoveDoesNotReplayUncertainNativeRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<Error><Code>InternalError</Code><Message>unknown result</Message></Error>`)
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	regular := sdk.NewClient(&sdk.BaseURL{BucketURL: base}, server.Client())
	once := sdk.NewClient(&sdk.BaseURL{BucketURL: base}, server.Client())
	once.Conf.RetryOpt.Count = 1
	client := &Client{client: regular, publishClient: once}
	if err = client.Remove(context.Background(), "drive/owner/a.txt"); err == nil {
		t.Fatal("uncertain delete returned success")
	}
	if calls.Load() != 1 {
		t.Fatalf("native delete attempts=%d, want 1", calls.Load())
	}
}
