package cos

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

func TestCaptureBaselineExactKeysAndPagination(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Query().Get("prefix") != "drive/owner/" {
			t.Error("capture attempted an unexpected request")
		}
		w.Header().Set("Content-Type", "application/xml")
		if calls == 1 {
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextMarker>drive/owner/ a+%23?# </NextMarker><Contents><Key>drive/owner/ a+%23?# </Key><Size>7</Size><ETag>"opaque"</ETag><LastModified>2026-10-30T00:00:00Z</LastModified></Contents></ListBucketResult>`)
			return
		}
		if r.URL.Query().Get("marker") != "drive/owner/ a+%23?# " {
			t.Error("marker spelling changed")
		}
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>drive/owner/Empty/</Key><Size>0</Size><LastModified>2026-10-30T00:00:00Z</LastModified></Contents></ListBucketResult>`)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	c := &Client{client: sdk.NewClient(&sdk.BaseURL{BucketURL: u}, server.Client())}
	entries, err := c.CaptureBaseline(context.Background(), "drive/owner/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Key != "drive/owner/ a+%23?# " || calls != 2 {
		t.Fatalf("unexpected capture: %+v, calls=%d", entries, calls)
	}
}

func TestCaptureBaselineRejectsIncompleteOrInvalidPages(t *testing.T) {
	cases := []string{
		`<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`,
		`<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>drive/other/private</Key><Size>7</Size><LastModified>2026-10-30T00:00:00Z</LastModified></Contents></ListBucketResult>`,
		`<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>drive/owner/a</Key><Size>7</Size></Contents></ListBucketResult>`,
		`<ListBucketResult><IsTruncated>true</IsTruncated><NextMarker>drive/owner/a</NextMarker><Contents><Key>drive/owner/a</Key><Size>7</Size><LastModified>2026-10-30T00:00:00Z</LastModified></Contents></ListBucketResult>`,
	}
	for i, body := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			c := &Client{client: sdk.NewClient(&sdk.BaseURL{BucketURL: u}, server.Client())}
			entries, err := c.CaptureBaseline(context.Background(), "drive/owner/")
			if err == nil || entries != nil {
				t.Fatalf("invalid listing accepted: %+v, %v", entries, err)
			}
		})
	}
}
