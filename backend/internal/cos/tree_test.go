package cos

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/objecttree/treetest"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

func TestNativeTreeConformance(t *testing.T) {
	treetest.Run(t, func(server *httptest.Server) (storage.Factory, error) {
		endpoint, err := url.Parse(server.URL)
		if err != nil {
			return nil, err
		}
		original := server.Config.Handler
		server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Authorization"), "q-sign-algorithm=sha1") {
				t.Error("COS SDK request lacks native authentication")
			}
			original.ServeHTTP(w, r)
		})
		native := sdk.NewClient(&sdk.BaseURL{BucketURL: endpoint}, &http.Client{Transport: &sdk.AuthorizationTransport{SecretID: "test-id", SecretKey: "test-key", Transport: server.Client().Transport}})
		client := &Client{bucket: "test-bucket", client: native}
		return client.TreeFactory()
	})
}
func TestTreeFactoryRejectsMissingClient(t *testing.T) {
	if _, err := NewTreeFactory(nil); !storage.IsCode(err, "invalid_config") {
		t.Fatal(err)
	}
}
