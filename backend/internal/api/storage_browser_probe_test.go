package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/connections"
)

// A relay connection has no provider URL to hand a browser, so its speed test runs through
// this service — same origin, same cookie, the same leg order a real upload takes on that
// product. Refusing here would leave exactly the connections whose speed a person can fix
// with no test at all.
func TestBrowserProbeRelaysARelayConnection(t *testing.T) {
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
	if recorder.Code != http.StatusOK {
		t.Fatalf("relay connection answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var envelope storageEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var data struct {
		RunID  string `json:"runId"`
		Direct bool   `json:"direct"`
		Upload struct {
			URL         string            `json:"url"`
			Method      string            `json:"method"`
			Headers     map[string]string `json:"headers"`
			Credentials string            `json:"credentials"`
		} `json:"upload"`
		Download struct {
			URL         string `json:"url"`
			Credentials string `json:"credentials"`
		} `json:"download"`
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Direct {
		t.Fatal("a relay connection was handed a direct provider transfer")
	}
	if data.Upload.Method != http.MethodPut || data.Upload.Credentials != "include" || data.Download.Credentials != "include" {
		t.Fatalf("the relayed legs are not a cookie-authenticated PUT and GET: %+v", data)
	}
	want := "/api/storage/connections/" + row.ID + "/browser-probe/" + data.RunID + "/object"
	if data.Upload.URL != want || data.Download.URL != want {
		t.Fatalf("the relayed legs do not name this run's object: %q, %q, want %q", data.Upload.URL, data.Download.URL, want)
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

// The relayed legs refuse a run this account did not open: the object path names a run id,
// and an id alone must never be authority to read a provider object.
func TestBrowserProbeRelayLegsRefuseAnUnknownRun(t *testing.T) {
	server, _, ctx := newAccountSessionTestServer(t)
	owner := createAccount(t, ctx, server, "browser-probe-relay@example.test", hashedPassword(t, "correct-horse-1"))
	cookie := openSession(t, ctx, server, owner)
	row := insertStorageRow(t, server, ctx, connections.Connection{
		Name: "nas", Backend: "s3", Role: connections.RoleBackup, OwnerID: owner,
		Config: connections.Spec{Product: "rustfs", S3: connections.S3Spec{
			Endpoint: "https://nas.example.test", Region: "us-east-1", Bucket: "backups",
			PathStyle: true, ContentPrefix: "drive/", SystemPrefix: "__eam_system__/",
		}},
		Secret: connections.Secret{S3: connections.S3Credential{AccessKeyID: "id", SecretAccessKey: "key"}},
	})

	for _, method := range []string{http.MethodPut, http.MethodGet} {
		recorder := storageRequest(t, server, method, "/api/storage/connections/"+row.ID+"/browser-probe/bp_0123456789abcdef/object", cookie, nil)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s of an unopened run answered %d: %s", method, recorder.Code, recorder.Body.String())
		}
	}
}

// A relayed upload is exactly the measured length, because a shorter or longer body would
// publish a rate for work that never happened.
func TestBrowserProbeRelayPutRefusesAWrongLength(t *testing.T) {
	server, _, ctx := newAccountSessionTestServer(t)
	owner := createAccount(t, ctx, server, "browser-probe-size@example.test", hashedPassword(t, "correct-horse-1"))
	cookie := openSession(t, ctx, server, owner)
	row := insertStorageRow(t, server, ctx, connections.Connection{
		Name: "nas", Backend: "s3", Role: connections.RoleBackup, OwnerID: owner,
		Config: connections.Spec{Product: "rustfs", S3: connections.S3Spec{
			Endpoint: "https://nas.example.test", Region: "us-east-1", Bucket: "backups",
			PathStyle: true, ContentPrefix: "drive/", SystemPrefix: "__eam_system__/",
		}},
		Secret: connections.Secret{S3: connections.S3Credential{AccessKeyID: "id", SecretAccessKey: "key"}},
	})

	// The run has to exist for the length check to be the refusal under test.
	start := storageRequest(t, server, http.MethodPost, "/api/storage/connections/"+row.ID+"/browser-probe", cookie, nil)
	if start.Code != http.StatusOK {
		t.Fatalf("start answered %d: %s", start.Code, start.Body.String())
	}
	var envelope storageEnvelope
	if err := json.Unmarshal(start.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var grant struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(envelope.Data, &grant); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader("too short")
	recorder := storageRequest(t, server, http.MethodPut, "/api/storage/connections/"+row.ID+"/browser-probe/"+grant.RunID+"/object", cookie, body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a short body answered %d: %s", recorder.Code, recorder.Body.String())
	}
}
