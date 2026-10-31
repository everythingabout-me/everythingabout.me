package fileops

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestPostgresCheckpointAndCleanupSafety(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("unregistered action cannot starve staged cleanup", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		generic := mustClaim(t, p, mustBegin(t, p, genericRequest(r, a, "older-copy", 0)), a)
		if err := p.FinishWithoutEffect(ctx, generic.Lease, Failed, nil); err != nil {
			t.Fatal(err)
		}
		req := genericRequest(r, a, "later-upload", 0)
		req.Kind = "upload_commit"
		req.Intent, _ = json.Marshal(storage.WriteIntent{Target: "later-upload", Size: 0, Conditions: storage.Conditions{Absent: true}})
		upload := mustClaim(t, p, mustBegin(t, p, req), a)
		if err := p.FinishWithoutEffect(ctx, upload.Lease, Failed, nil); err != nil {
			t.Fatal(err)
		}
		candidates, err := p.CleanupCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].ID != upload.ID {
			t.Fatalf("cleanup selected unrelated older action: %+v %v", candidates, err)
		}
	})
	t.Run("empty unknown observation preserves allocation checkpoint", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		op := mustClaim(t, p, mustBegin(t, p, genericRequest(r, a, "unknown", 4)), a)
		checkpoint := []byte("durable native stage identity")
		if err := p.SaveCheckpoint(ctx, op.Lease, checkpoint); err != nil {
			t.Fatal(err)
		}
		if err := p.StartEffect(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		evidence := []byte("native inspection uncertain")
		if err := p.MarkUnknown(ctx, op.Lease, evidence); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkUnknown(ctx, op.Lease, nil); err != nil {
			t.Fatal(err)
		}
		got, err := p.Get(ctx, r, a, op.ID)
		if err != nil || !bytes.Equal(got.Checkpoint, checkpoint) || !bytes.Equal(got.RecoveryEvidence, evidence) || got.State != Unknown {
			t.Fatalf("unknown erased native provenance: %+v %v", got, err)
		}
		var count int
		if err = pool.QueryRow(ctx, `select count(*) from file_operation_checkpoints where operation_id=$1`, op.ID).Scan(&count); err != nil || count != 3 {
			t.Fatalf("observation history=%d %v", count, err)
		}
		requireCode(t, p.FinishWithoutEffect(ctx, op.Lease, Cancelled, nil), "recovery_required")
	})
}
