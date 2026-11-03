package fileops

import (
	"context"
	"encoding/json"
	"everythingabout.me/backend/internal/storage"
	"testing"
)

func TestCreateOnlyJournalRejectsBroaderIntent(t *testing.T) {
	root := testRoot()
	root.CreateOnly = true
	actor := testActor(root)
	intent := storage.WriteIntent{Target: "new", Size: 4, Conditions: storage.Conditions{Absent: true}}
	encoded, _ := json.Marshal(intent)
	req := BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", Intent: encoded, Guards: []Guard{{Path: "new"}}, Reservation: ReservationRequest{NewBytes: 4, PhysicalBytes: 8}}
	if _, _, _, err := validateBegin(req); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"mkdir", "copy", "move", "delete", "thumbnail"} {
		bad := req
		bad.Kind = kind
		if _, _, _, err := validateBegin(bad); !IsCode(err, "unsupported") {
			t.Fatalf("%s admitted on create-only root: %v", kind, err)
		}
	}
	bad := req
	intent.Conditions = storage.Conditions{ExpectedVersion: []byte("native-old")}
	bad.Intent, _ = json.Marshal(intent)
	if _, _, _, err := validateBegin(bad); !IsCode(err, "unsupported") {
		t.Fatalf("replacement admitted: %v", err)
	}
	bad = req
	bad.Root.RetainsVersions = true
	if _, _, _, err := validateBegin(bad); !IsCode(err, "invalid_request") {
		t.Fatalf("contradictory root accepted: %v", err)
	}
}

func TestCreateOnlyPostgresBaselineDoesNotAssertWriterDrain(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	root := testRoot()
	root.OwnerID = newID()
	root.CreateOnly = true
	if _, err := pool.Exec(ctx, `insert into users(id,email,password_hash) values($1,$2,'fixture')`, root.OwnerID, root.OwnerID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	journal := NewPostgres(pool)
	base := Baseline{NativeComplete: true, IndexCompared: true, Evidence: json.RawMessage(`{"observation":"matched current native and indexed tree","externalWriters":"allowed"}`)}
	if err := journal.Initialize(ctx, root, base); err != nil {
		t.Fatalf("observed create-only baseline rejected: %v", err)
	}
	var stored Baseline
	var encoded []byte
	if err := pool.QueryRow(ctx, `select baseline from file_budgets where owner_id=$1::uuid`, root.OwnerID).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(encoded, &stored) != nil || stored.LegacyWritersDrained {
		t.Fatal("invented a writer-drain assertion")
	}
	broader := root
	broader.CreateOnly = false
	if err := journal.Initialize(ctx, broader, base); !IsCode(err, "baseline_required") {
		t.Fatalf("broad root admitted without drain: %v", err)
	}
	if _, err := journal.Usage(ctx, broader, testActor(root)); !IsCode(err, "root_conflict") {
		t.Fatalf("restricted budget reused by broader root: %v", err)
	}
}
