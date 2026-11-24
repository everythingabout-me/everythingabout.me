package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/connections"
)

// A relay connection has no provider URL to hand a browser: its bytes go through this
// service, and the server-side measurement already describes that path. The answer has to
// say so, because a page that showed a red failure here would blame the store.
func TestBrowserProbeRefusesARelayConnection(t *testing.T) {
	server, _, ctx := newAccountSessionTestServer(t)
	owner := createAccount(t, ctx, server, "browser-probe@example.test", hashedPassword(t, "correct-horse-1"))
	cookie := openSession(t, ctx, server, owner)
	row := insertStorageRow(t, server, ctx, connections.Connection{
		Name: "nas", Backend: "s3", Role: connections.RoleBackup, OwnerID: owner,
		Config: connections.Spec{Product: "rustfs", S3: connections.S3Spec{
			Endpoint: "https://nas.example.test", Region: "us-east-1", Bucket: "backups",
			PathStyle: true, ContentPrefix: "drive/", SystemPrefix: "__eam_system__/",
		}},
		Secret: connections.Secret{S3: connections.S3Credential{AccessKeyID: "id", SecretAccessKey: "key"}},
	})

	recorder := storageRequest(t, server, http.MethodPost, "/api/storage/connections/"+row.ID+"/browser-probe", cookie, nil)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("relay connection answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var envelope storageEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"nas", "relayed"} {
		if !strings.Contains(envelope.Error, fragment) {
			t.Fatalf("the refusal does not name %q: %s", fragment, envelope.Error)
		}
	}
}

// The delete route can only remove an object this owner opened on this connection: an
// unknown run is not an error, it is an already-swept run.
func TestBrowserProbeDeleteForgetsUnknownRuns(t *testing.T) {
	server, _, ctx := newAccountSessionTestServer(t)
	owner := createAccount(t, ctx, server, "browser-probe-delete@example.test", hashedPassword(t, "correct-horse-1"))
	cookie := openSession(t, ctx, server, owner)
	row := insertStorageRow(t, server, ctx, connections.Connection{
		Name: "nas", Backend: "s3", Role: connections.RoleBackup, OwnerID: owner,
		Config: connections.Spec{Product: "rustfs", S3: connections.S3Spec{
			Endpoint: "https://nas.example.test", Region: "us-east-1", Bucket: "backups",
			PathStyle: true, ContentPrefix: "drive/", SystemPrefix: "__eam_system__/",
		}},
		Secret: connections.Secret{S3: connections.S3Credential{AccessKeyID: "id", SecretAccessKey: "key"}},
	})

	recorder := storageRequest(t, server, http.MethodDelete, "/api/storage/connections/"+row.ID+"/browser-probe/bp_0123456789abcdef", cookie, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unknown run answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var envelope storageEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var data struct {
		Removed bool `json:"removed"`
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil || data.Removed {
		t.Fatalf("an unknown run reported a removal: %s", envelope.Data)
	}
}
