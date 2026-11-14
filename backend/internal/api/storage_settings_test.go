package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/connections"
)

// storageEnvelope is the {ok,data} shape these account routes use. The test decodes
// the data it needs instead of asserting the whole page, so a copy change does not
// break it and a field regression does.
type storageEnvelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

type storagePage struct {
	Connections []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Role         string `json:"role"`
		BackupFolder string `json:"backupFolder"`
		Runs         int    `json:"runs"`
	} `json:"connections"`
	BackupTarget string `json:"backupTarget"`
}

func storageRequest(t *testing.T, server *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func decodeStoragePage(t *testing.T, recorder *httptest.ResponseRecorder) storagePage {
	t.Helper()
	var envelope storageEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	if !envelope.OK {
		t.Fatalf("request failed: %s", envelope.Error)
	}
	var page storagePage
	if err := json.Unmarshal(envelope.Data, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func insertStorageRow(t *testing.T, server *Server, ctx context.Context, row connections.Connection) connections.Connection {
	t.Helper()
	row.Generation, row.Enabled = 1, true
	identity, err := row.ComputeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	row.Identity = identity
	if err := connections.NewPostgresStore(server.db).Insert(ctx, row); err != nil {
		t.Fatal(err)
	}
	if row.ID == "" {
		if err := server.db.Pool().QueryRow(ctx, `select id::text from storage_connections where identity=$1 and name=$2`, row.Identity, row.Name).Scan(&row.ID); err != nil {
			t.Fatal(err)
		}
	}
	return row
}

// The page has to tell a person which destination a backup will use. Before this,
// the first backup row silently won and an account with two destinations could not
// see or change the choice.
func TestStorageTargetAndFolderAreAccountSettings(t *testing.T) {
	server, _, ctx := newAccountSessionTestServer(t)
	owner := createAccount(t, ctx, server, "storage-settings@example.test", hashedPassword(t, "correct-horse-1"))
	cookie := openSession(t, ctx, server, owner)

	authority := insertStorageRow(t, server, ctx, connections.Connection{
		Name: "default", Backend: "cos", Role: connections.RoleAuthority,
		Config: connections.Spec{COS: connections.COSSpec{Bucket: "authority-bucket-125", Region: "ap-hongkong"}},
		Secret: connections.Secret{COS: connections.COSCredential{SecretID: "id", SecretKey: "key"}},
	})
	backup := insertStorageRow(t, server, ctx, connections.Connection{
		Name: "nas", Backend: "s3", Role: connections.RoleBackup, OwnerID: owner,
		Config: connections.Spec{CoordinatedWrites: true, S3: connections.S3Spec{
			Endpoint: "https://nas.example.test", Region: "us-east-1", Bucket: "backups",
			PathStyle: true, ContentPrefix: "drive/", SystemPrefix: "__eam_system__/",
		}},
		Secret: connections.Secret{S3: connections.S3Credential{AccessKeyID: "id", SecretAccessKey: "key"}},
	})

	page := decodeStoragePage(t, storageRequest(t, server, http.MethodGet, "/api/storage", cookie, nil))
	if page.BackupTarget != backup.ID {
		t.Fatalf("backupTarget = %q, want the only destination %q", page.BackupTarget, backup.ID)
	}

	recorder := storageRequest(t, server, http.MethodPatch, "/api/storage/connections/"+backup.ID, cookie, map[string]string{"backupFolder": "/backups/"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("set folder: %d %s", recorder.Code, recorder.Body.String())
	}
	page = decodeStoragePage(t, storageRequest(t, server, http.MethodGet, "/api/storage", cookie, nil))
	var folder string
	for _, connection := range page.Connections {
		if connection.ID == backup.ID {
			folder = connection.BackupFolder
		}
	}
	if folder != "backups" {
		t.Fatalf("stored folder = %q, want normalized %q", folder, "backups")
	}

	// The deployment's drive is not an account setting, and it is not a snapshot
	// target: writing a date into the tree being copied is not a backup.
	if recorder := storageRequest(t, server, http.MethodPatch, "/api/storage/connections/"+authority.ID, cookie, map[string]string{"backupFolder": "x"}); recorder.Code != http.StatusForbidden {
		t.Fatalf("editing the default drive answered %d, want 403", recorder.Code)
	}
	if recorder := storageRequest(t, server, http.MethodPut, "/api/storage/backup-target", cookie, map[string]string{"connectionId": authority.ID}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("choosing the default drive as a target answered %d, want 400", recorder.Code)
	}

	// "No destination" must stick even when it is the only attached drive: deleting
	// the policy row would make the page fall back to that drive again.
	if recorder := storageRequest(t, server, http.MethodPut, "/api/storage/backup-target", cookie, map[string]string{"connectionId": ""}); recorder.Code != http.StatusOK {
		t.Fatalf("clear the target: %d %s", recorder.Code, recorder.Body.String())
	}
	page = decodeStoragePage(t, storageRequest(t, server, http.MethodGet, "/api/storage", cookie, nil))
	if page.BackupTarget != "" {
		t.Fatalf("clearing the target fell back to %q", page.BackupTarget)
	}
	if recorder := storageRequest(t, server, http.MethodPut, "/api/storage/backup-target", cookie, map[string]string{"connectionId": backup.ID}); recorder.Code != http.StatusOK {
		t.Fatalf("choose the target again: %d %s", recorder.Code, recorder.Body.String())
	}
	page = decodeStoragePage(t, storageRequest(t, server, http.MethodGet, "/api/storage", cookie, nil))
	if page.BackupTarget != backup.ID {
		t.Fatalf("re-choosing gave back %q, want %q", page.BackupTarget, backup.ID)
	}

	// A recorded run is what makes the folder final: restore points name a date,
	// not a folder, so moving it would make every listed date unreachable.
	if _, err := server.db.Pool().Exec(ctx, `insert into backup_runs(owner_id, connection_id, snapshot, state) values($1::uuid,$2::uuid,'2026-11-14','complete')`, owner, backup.ID); err != nil {
		t.Fatal(err)
	}
	if recorder := storageRequest(t, server, http.MethodPatch, "/api/storage/connections/"+backup.ID, cookie, map[string]string{"backupFolder": "elsewhere"}); recorder.Code != http.StatusConflict {
		t.Fatalf("moving a folder that holds a run answered %d, want 409: %s", recorder.Code, recorder.Body.String())
	}

	// Another account sees the shared row but neither the destination nor its id.
	other := createAccount(t, ctx, server, "other-storage@example.test", hashedPassword(t, "correct-horse-2"))
	otherCookie := openSession(t, ctx, server, other)
	otherPage := decodeStoragePage(t, storageRequest(t, server, http.MethodGet, "/api/storage", otherCookie, nil))
	if otherPage.BackupTarget != "" {
		t.Fatalf("another account targets %q", otherPage.BackupTarget)
	}
	if recorder := storageRequest(t, server, http.MethodPatch, "/api/storage/connections/"+backup.ID, otherCookie, map[string]string{"backupFolder": "mine"}); recorder.Code != http.StatusNotFound {
		t.Fatalf("another account edited a destination: %d", recorder.Code)
	}
	if recorder := storageRequest(t, server, http.MethodPut, "/api/storage/backup-target", otherCookie, map[string]string{"connectionId": backup.ID}); recorder.Code != http.StatusNotFound {
		t.Fatalf("another account selected a destination: %d", recorder.Code)
	}
}
