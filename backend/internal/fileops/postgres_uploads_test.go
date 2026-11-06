package fileops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func uploadRequest(r Root, a Actor, target storage.Path, size int64, key string) (BeginRequest, UploadOptions) {
	client := storage.WriteIntent{Target: target, Size: size, ClientCondition: "*", MediaType: "application/octet-stream"}
	captured := client
	captured.Conditions = storage.Conditions{Absent: true}
	captured.ParentPath = "folder"
	captured.ParentVersion = []byte("captured-parent")
	intent, _ := json.Marshal(captured)
	return BeginRequest{Root: r, Actor: a, Kind: "upload_commit", IdempotencyKey: key, Intent: intent, Reservation: ReservationRequest{NewBytes: size, PhysicalBytes: 2*size + 16}}, UploadOptions{PartSize: 4, ClientIntent: client}
}
func mustUpload(t *testing.T, p *Postgres, r Root, a Actor, target storage.Path, size int64, key string) Upload {
	t.Helper()
	req, opt := uploadRequest(r, a, target, size, key)
	u, replay, err := p.CreateUpload(context.Background(), req, opt)
	if err != nil || replay {
		t.Fatalf("create replay=%v: %v", replay, err)
	}
	return u
}
func receipt(n int, size int64) PartReceipt {
	return PartReceipt{Number: n, Size: size, SHA256: fmt.Sprintf("%064x", n), Handle: fmt.Sprintf("private/chunk-%d", n)}
}
func mustReceipt(t *testing.T, p *Postgres, r Root, a Actor, u Upload, n int, size int64) PartReceipt {
	t.Helper()
	part, replay, err := p.AcceptUploadPart(context.Background(), r, a, u.ID, receipt(n, size))
	if err != nil || replay {
		t.Fatalf("receipt replay=%v: %v", replay, err)
	}
	return part
}

func TestUploadClientFingerprint(t *testing.T) {
	w := storage.WriteIntent{Target: "folder/file", Size: 8, ClientCondition: "*"}
	a, err := clientUploadFingerprint(w)
	if err != nil {
		t.Fatal(err)
	}
	w.Conditions = storage.Conditions{ExpectedVersion: []byte("new native evidence")}
	w.ParentPath = "new-parent"
	w.ParentVersion = []byte("new version")
	b, err := clientUploadFingerprint(w)
	if err != nil || a != b {
		t.Fatalf("native evidence changed client identity: %v", err)
	}
	w.ClientCondition = "different-client-revision"
	b, _ = clientUploadFingerprint(w)
	if a == b {
		t.Fatal("client condition omitted from replay identity")
	}
	for _, tt := range []struct {
		size, part int64
		count      int
	}{{0, 4, 0}, {1, 4, 1}, {4, 4, 1}, {5, 4, 2}, {8, 4, 2}} {
		got, err := uploadPartCount(tt.size, tt.part)
		if err != nil || got != tt.count {
			t.Fatalf("geometry %+v: %d %v", tt, got, err)
		}
	}
	if _, err := uploadPartCount(1, 0); err == nil {
		t.Fatal("zero part size accepted")
	}
	if _, err := uploadPartCount(1<<40, 1); err == nil {
		t.Fatal("unbounded part count accepted")
	}
}

func TestPostgresUploads(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("atomic admission and independent client replay keys", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 20, 200))
		u := mustUpload(t, p, r, a, "folder/one", 8, "same-key")
		if u.ID != u.Operation.ID || u.PartCount != 2 || u.State != UploadOpen || u.ExpiresAt.Sub(u.CreatedAt) != 24*time.Hour {
			t.Fatal(u)
		}
		_, err := p.Claim(ctx, r, a, u.ID, time.Minute)
		requireCode(t, err, "invalid_transition")
		req, opt := uploadRequest(r, a, "folder/one", 8, "same-key")
		var captured storage.WriteIntent
		_ = json.Unmarshal(req.Intent, &captured)
		captured.Conditions = storage.Conditions{ExpectedVersion: []byte("changed-evidence")}
		captured.ParentVersion = []byte("reobserved-parent")
		req.Intent, _ = json.Marshal(captured)
		req.Reservation.PhysicalBytes++
		got, replay, err := p.CreateUpload(ctx, req, opt)
		if err != nil || !replay || got.ID != u.ID {
			t.Fatalf("captured evidence replay: %s %v %v", got.ID, replay, err)
		}
		got, replay, err = p.ReplayUpload(ctx, r, a, "same-key", opt.ClientIntent)
		if err != nil || !replay || got.ID != u.ID {
			t.Fatalf("client replay: %v %v", replay, err)
		}
		changed := opt.ClientIntent
		changed.Size++
		_, _, err = p.ReplayUpload(ctx, r, a, "same-key", changed)
		requireCode(t, err, "idempotency_conflict")
		req, opt = uploadRequest(r, a, "folder/other", 1, "same-key")
		_, _, err = p.CreateUpload(ctx, req, opt)
		requireCode(t, err, "idempotency_conflict")
		req, opt = uploadRequest(r, a, "folder/one", 8, "new-key")
		_, _, err = p.CreateUpload(ctx, req, opt)
		requireCode(t, err, "operation_in_progress")
		other := mustUpload(t, p, r, a, "folder/two", 8, "other")
		req, _ = uploadRequest(r, a, "folder/simple", 1, "same-key")
		req.Guards = []Guard{{Path: "folder/simple"}}
		simple, _, err := p.Begin(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		var simpleIntent storage.WriteIntent
		_ = json.Unmarshal(req.Intent, &simpleIntent)
		replayed, found, err := p.ReplayWrite(ctx, r, a, "same-key", simpleIntent)
		if err != nil || !found || replayed.ID != simple.ID {
			t.Fatalf("simple/session key separation: %+v %v %v", replayed, found, err)
		}
		req, opt = uploadRequest(r, a, "folder/excess", 8, "excess")
		_, _, err = p.CreateUpload(ctx, req, opt)
		requireCode(t, err, "quota_exceeded")
		var count int
		if err = pool.QueryRow(ctx, `select count(*) from file_uploads where owner_id=$1`, r.OwnerID).Scan(&count); err != nil || count != 2 {
			t.Fatalf("failed admission left session: %d %v", count, err)
		}
		usage := mustUsage(t, p, r, a)
		if usage.LogicalReserved != 17 || usage.PhysicalReserved != 82 {
			t.Fatalf("replay reserved twice: %+v", usage)
		}
		// Retained open sessions never enter workers, even after the normal delay.
		_, err = pool.Exec(ctx, `update file_operations set snapshot=jsonb_set(snapshot,'{CreatedAt}',to_jsonb(clock_timestamp()-interval '1 hour')) where id in ($1,$2)`, u.ID, other.ID)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := p.Pending(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range pending {
			if op.ID == u.ID || op.ID == other.ID {
				t.Fatal("open upload entered recovery worker")
			}
		}
	})
	t.Run("immutable receipts watermark and freeze", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		u := mustUpload(t, p, r, a, "folder/receipts", 10, "")
		_, err := p.FreezeUpload(ctx, r, a, u.ID)
		requireCode(t, err, "upload_incomplete")
		one := mustReceipt(t, p, r, a, u, 2, 4)
		two := mustReceipt(t, p, r, a, u, 1, 4)
		if one.Sequence != 1 || two.Sequence != 2 {
			t.Fatal("receipts are not in acknowledgement order")
		}
		page, watermark, err := p.ListUploadParts(ctx, r, a, u.ID, 1, 0, 0)
		if err != nil || len(page) != 1 || page[0].Number != 2 || watermark != 2 {
			t.Fatalf("first page %+v %d %v", page, watermark, err)
		}
		three := mustReceipt(t, p, r, a, u, 3, 2)
		page, again, err := p.ListUploadParts(ctx, r, a, u.ID, 10, one.Sequence, watermark)
		if err != nil || again != watermark || len(page) != 1 || page[0].Number != 1 {
			t.Fatalf("watermark moved %+v %d %v", page, again, err)
		}
		replayPart := receipt(1, 4)
		replayPart.Handle = "different-isolated-attempt"
		got, replay, err := p.AcceptUploadPart(ctx, r, a, u.ID, replayPart)
		if err != nil || !replay || got.Handle != two.Handle || got.Sequence != two.Sequence {
			t.Fatalf("receipt replay %+v %v %v", got, replay, err)
		}
		conflict := receipt(1, 4)
		conflict.SHA256 = strings.Repeat("f", 64)
		_, _, err = p.AcceptUploadPart(ctx, r, a, u.ID, conflict)
		requireCode(t, err, "part_conflict")
		_, _, err = p.AcceptUploadPart(ctx, r, a, u.ID, receipt(3, 4))
		requireCode(t, err, "invalid_part")
		_, _, err = p.AcceptUploadPart(ctx, r, a, u.ID, receipt(4, 4))
		requireCode(t, err, "invalid_part")
		status, err := p.GetUpload(ctx, r, a, u.ID)
		if err != nil || status.ReceivedParts != 3 || status.ReceivedBytes != 10 {
			t.Fatalf("unique counters %+v %v", status, err)
		}
		frozen, err := p.FreezeUpload(ctx, r, a, u.ID)
		if err != nil || frozen.State != UploadCommitting {
			t.Fatalf("freeze %+v %v", frozen, err)
		}
		twice, err := p.FreezeUpload(ctx, r, a, u.ID)
		if err != nil || twice.ID != frozen.ID {
			t.Fatalf("freeze replay %+v %v", twice, err)
		}
		_, _, err = p.AcceptUploadPart(ctx, r, a, u.ID, three)
		requireCode(t, err, "commit_in_progress")
		_, err = p.AbortUpload(ctx, r, a, u.ID)
		requireCode(t, err, "commit_in_progress")
		op := mustClaim(t, p, frozen.Operation, a)
		var intent storage.WriteIntent
		_ = json.Unmarshal(op.Intent, &intent)
		intent.ID = op.ID
		if err = p.SaveStage(ctx, op.Lease, storage.Staged{Intent: intent, Handle: []byte("assembled-private-stage"), Size: 10}); err != nil {
			t.Fatal(err)
		}
		if err = p.StartEffect(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		if err = p.MarkUnknown(ctx, op.Lease, []byte("ambiguous effect")); err != nil {
			t.Fatal(err)
		}
		status, err = p.GetUpload(ctx, r, a, u.ID)
		if err != nil || status.State != UploadUnknown {
			t.Fatalf("unknown projection %+v %v", status, err)
		}
		result := storage.MutationResult{Entry: &storage.Entry{Path: intent.Target, Size: 10, Kind: storage.File}, Evidence: []byte("proven native terminal")}
		if err = p.SaveApplied(ctx, op.Lease, result); err != nil {
			t.Fatal(err)
		}
		_, err = p.Settle(ctx, op.Lease, Settlement{State: Succeeded, LogicalDelta: 10, PhysicalDelta: 10, KeepPhysical: 26, Result: json.RawMessage(`{"path":"/folder/receipts"}`)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		status, err = p.GetUpload(ctx, r, a, u.ID)
		if err != nil || status.State != UploadCommitted || len(status.Operation.Result) == 0 {
			t.Fatalf("terminal projection %+v %v", status, err)
		}
		_, err = p.AbortUpload(ctx, r, a, u.ID)
		requireCode(t, err, "already_committed")
	})
	t.Run("abort retains physical budget until actual cleanup", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		u := mustUpload(t, p, r, a, "folder/abort", 8, "retained")
		mustReceipt(t, p, r, a, u, 1, 4)
		if err := p.MarkUploadChunksCleaned(ctx, r, a, u.ID); !IsCode(err, "invalid_transition") {
			t.Fatalf("open cleanup: %v", err)
		}
		// A previously requested operation cancellation must not make open abort stuck.
		if _, err := p.RequestCancel(ctx, r, a, u.ID); err != nil {
			t.Fatal(err)
		}
		aborted, err := p.AbortUpload(ctx, r, a, u.ID)
		if err != nil || aborted.State != UploadAborted || aborted.Operation.State != Cancelled || aborted.Operation.CleanedAt != nil {
			t.Fatalf("abort %+v %v", aborted, err)
		}
		if aborted.Operation.FinishedAt == nil || aborted.Operation.RetainUntil.Sub(*aborted.Operation.FinishedAt) < 24*time.Hour {
			t.Fatal("terminal retention under 24 hours")
		}
		_, err = p.AbortUpload(ctx, r, a, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = p.AcceptUploadPart(ctx, r, a, u.ID, receipt(2, 4))
		requireCode(t, err, "upload_gone")
		_, err = p.FreezeUpload(ctx, r, a, u.ID)
		requireCode(t, err, "upload_gone")
		usage := mustUsage(t, p, r, a)
		if usage.LogicalReserved != 0 || usage.PhysicalReserved != 32 {
			t.Fatal(usage)
		}
		// The released target guard admits a fresh independent session.
		fresh := mustUpload(t, p, r, a, "folder/abort", 1, "")
		candidates, err := p.CleanupCandidates(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, op := range candidates {
			if op.ID == u.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("aborted upload absent from cleanup work")
		}
		if err = p.MarkUploadChunksCleaned(ctx, r, a, u.ID); err != nil {
			t.Fatal(err)
		}
		if got := mustUsage(t, p, r, a); got.PhysicalReserved != 50 {
			t.Fatal("chunk metadata released physical reservation")
		}
		if err = p.MarkCleaned(ctx, r, a, u.ID); err != nil {
			t.Fatal(err)
		}
		if err = p.MarkCleaned(ctx, r, a, u.ID); err != nil {
			t.Fatal(err)
		}
		usage = mustUsage(t, p, r, a)
		if usage.PhysicalReserved != fresh.Operation.Reservation.PhysicalBytes {
			t.Fatal(usage)
		}
		req, opt := uploadRequest(r, a, "folder/abort", 8, "retained")
		replayed, replay, err := p.CreateUpload(ctx, req, opt)
		if err != nil || !replay || replayed.ID != u.ID || replayed.State != UploadAborted {
			t.Fatalf("terminal replay %+v %v %v", replayed, replay, err)
		}
	})
	t.Run("status replay and list expire without waiting for sweep", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		for _, mode := range []string{"get", "replay", "list"} {
			u := mustUpload(t, p, r, a, storage.Path("folder/lazy-"+mode), 4, "lazy-"+mode)
			if _, err := pool.Exec(ctx, `update file_uploads set expires_at=clock_timestamp()-interval '1 second' where operation_id=$1`, u.ID); err != nil {
				t.Fatal(err)
			}
			var expired Upload
			var err error
			switch mode {
			case "get":
				expired, err = p.GetUpload(ctx, r, a, u.ID)
			case "replay":
				_, options := uploadRequest(r, a, storage.Path("folder/lazy-"+mode), 4, "lazy-"+mode)
				var found bool
				expired, found, err = p.ReplayUpload(ctx, r, a, "lazy-"+mode, options.ClientIntent)
				if !found {
					t.Fatal("expired replay identity lost")
				}
			case "list":
				var list []Upload
				list, err = p.ListUploads(ctx, r, a, 100, "")
				for _, candidate := range list {
					if candidate.ID == u.ID {
						expired = candidate
					}
				}
			}
			if err != nil || expired.State != UploadExpired || expired.Operation.State != Cancelled || expired.Operation.CleanedAt != nil {
				t.Fatalf("%s lazy expiry %+v %v", mode, expired, err)
			}
		}
		if usage := mustUsage(t, p, r, a); usage.LogicalReserved != 0 || usage.PhysicalReserved != 72 {
			t.Fatalf("lazy expiry released physical bytes %+v", usage)
		}
		if expired, err := p.ExpireUploads(ctx, 10); err != nil || len(expired) != 0 {
			t.Fatalf("already expired rows swept again: %+v %v", expired, err)
		}
	})
	t.Run("idle progress hard cap and bounded expiry", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		u := mustUpload(t, p, r, a, "folder/expiry", 8, "")
		_, err := pool.Exec(ctx, `update file_uploads set created_at=clock_timestamp()-interval '6 days 23 hours',expires_at=clock_timestamp()+interval '30 minutes' where operation_id=$1`, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		part := mustReceipt(t, p, r, a, u, 1, 4)
		progressed, err := p.GetUpload(ctx, r, a, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if progressed.ExpiresAt.Sub(progressed.CreatedAt) != 7*24*time.Hour {
			t.Fatalf("hard expiry cap missing: %+v", progressed)
		}
		_, replay, err := p.AcceptUploadPart(ctx, r, a, u.ID, part)
		if err != nil || !replay {
			t.Fatal(err)
		}
		repeated, err := p.GetUpload(ctx, r, a, u.ID)
		if err != nil || !repeated.ExpiresAt.Equal(progressed.ExpiresAt) {
			t.Fatal("replay extended expiry")
		}
		other := mustUpload(t, p, r, a, "folder/expiry-two", 1, "")
		_, err = pool.Exec(ctx, `update file_uploads set expires_at=clock_timestamp()-interval '1 second' where operation_id in ($1,$2)`, u.ID, other.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = p.AcceptUploadPart(ctx, r, a, u.ID, receipt(2, 4))
		requireCode(t, err, "upload_expired")
		expired, err := p.ExpireUploads(ctx, 1)
		if err != nil || len(expired) != 1 || expired[0].State != UploadExpired {
			t.Fatalf("expiry %+v %v", expired, err)
		}
		expired, err = p.ExpireUploads(ctx, 1)
		if err != nil || len(expired) != 1 {
			t.Fatalf("second expiry %+v %v", expired, err)
		}
		usage := mustUsage(t, p, r, a)
		if usage.LogicalReserved != 0 || usage.PhysicalReserved != 50 {
			t.Fatal(usage)
		}
		expired, err = p.ExpireUploads(ctx, 1)
		if err != nil || len(expired) != 0 {
			t.Fatalf("expiry repeated %+v %v", expired, err)
		}
	})
	t.Run("root principal scope and bounded session pagination", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		token := Actor{ID: "token:one", CanRead: true, CanWrite: true}
		u := mustUpload(t, p, r, token, "folder/scoped/item", 0, "")
		mustUpload(t, p, r, token, "folder/elsewhere/item", 0, "")
		narrow := token
		narrow.Scope = "folder/scoped"
		list, err := p.ListUploads(ctx, r, narrow, 1, "")
		if err != nil || len(list) != 1 || list[0].ID != u.ID {
			t.Fatalf("scoped list %+v %v", list, err)
		}
		_, err = p.GetUpload(ctx, r, a, u.ID)
		requireCode(t, err, "forbidden")
		owner := a
		owner.ManageOwner = true
		if _, err = p.GetUpload(ctx, r, owner, u.ID); err != nil {
			t.Fatal(err)
		}
		alien := token
		alien.ID = "token:two"
		_, err = p.GetUpload(ctx, r, alien, u.ID)
		requireCode(t, err, "forbidden")
		narrow.Scope = "folder/elsewhere"
		_, err = p.GetUpload(ctx, r, narrow, u.ID)
		requireCode(t, err, "forbidden")
		rotated := r
		rotated.Generation++
		_, err = p.GetUpload(ctx, rotated, token, u.ID)
		requireCode(t, err, "not_found")
		page, err := p.ListUploads(ctx, r, token, 1, "")
		if err != nil || len(page) != 1 {
			t.Fatal(err)
		}
		next, err := p.ListUploads(ctx, r, token, 1, page[0].ID)
		if err != nil || len(next) != 1 || next[0].ID == page[0].ID {
			t.Fatalf("keyset page %+v %v", next, err)
		}
		_, err = p.ListUploads(ctx, r, token, 1002, "")
		requireCode(t, err, "invalid_request")
		// Zero-byte sessions need no synthetic receipt before freeze.
		frozen, err := p.FreezeUpload(ctx, r, token, u.ID)
		if err != nil || frozen.PartCount != 0 || frozen.State != UploadCommitting {
			t.Fatalf("zero-byte freeze %+v %v", frozen, err)
		}
	})
	t.Run("receipt insertion serializes with abort and freeze", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 10000))
		for attempt := 0; attempt < 8; attempt++ {
			u := mustUpload(t, p, r, a, storage.Path(fmt.Sprintf("folder/race-%d", attempt)), 4, "")
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make(chan error, 2)
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, _, err := p.AcceptUploadPart(ctx, r, a, u.ID, receipt(1, 4))
				errs <- err
			}()
			go func() { defer wg.Done(); <-start; _, err := p.AbortUpload(ctx, r, a, u.ID); errs <- err }()
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil && !IsCode(err, "upload_gone") {
					t.Fatal(err)
				}
			}
			status, err := p.GetUpload(ctx, r, a, u.ID)
			if err != nil || status.State != UploadAborted {
				t.Fatalf("abort race %+v %v", status, err)
			}
			_, _, err = p.AcceptUploadPart(ctx, r, a, u.ID, receipt(1, 4))
			requireCode(t, err, "upload_gone")
		}
		u := mustUpload(t, p, r, a, "folder/freeze-race", 4, "")
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var partErr, freezeErr error
		go func() { defer wg.Done(); <-start; _, _, partErr = p.AcceptUploadPart(ctx, r, a, u.ID, receipt(1, 4)) }()
		go func() { defer wg.Done(); <-start; _, freezeErr = p.FreezeUpload(ctx, r, a, u.ID) }()
		close(start)
		wg.Wait()
		if partErr != nil {
			t.Fatal(partErr)
		}
		if freezeErr != nil && !IsCode(freezeErr, "upload_incomplete") {
			t.Fatal(freezeErr)
		}
		if _, err := p.FreezeUpload(ctx, r, a, u.ID); err != nil {
			t.Fatal(err)
		}
	})
}
