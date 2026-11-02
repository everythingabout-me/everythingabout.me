package objectmutation

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestObjectConfirmedPutMetadataRefreshIsRestartable(t *testing.T) {
	d, b, cfg, opts := fixture(t)
	ctx := context.Background()
	b.zeroPutMetadata = true
	b.failRefreshOnce = true
	in := createIntent("metadata", "file", "bytes")
	stage, err := d.Stage(ctx, in, strings.NewReader("bytes"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Publish(ctx, stage, cpOK); err == nil {
		t.Fatal("metadata refresh loss was hidden")
	}
	recorded, err := d.load("write", in.ID)
	if err != nil || recorded.Phase != "applied" || recorded.Result == nil || recorded.MetadataReady {
		t.Fatalf("terminal native receipt lost: %+v %v", recorded, err)
	}
	restarted, err := New(&fakeTree{backend: b, root: cfg.ContentRoot}, cfg, b, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	inspected, err := restarted.Inspect(ctx, in, stage.Handle)
	if err != nil || inspected.State != storage.Applied {
		t.Fatalf("metadata repair=%+v %v", inspected, err)
	}
	observed, err := restarted.Stat(ctx, in.Target)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(*inspected.Result.Entry, observed) || observed.ModifiedAt.IsZero() || observed.MediaType != in.MediaType || observed.Digest != nil {
		t.Fatalf("result not native metadata: saved=%+v current=%+v", inspected.Result.Entry, observed)
	}
	if b.puts != 1 {
		t.Fatal("read-only metadata recovery replayed native effect")
	}
}

func TestObjectRegressedSpoolCannotDowngradeFinalReceipt(t *testing.T) {
	for _, action := range []string{"write", "copy", "mkdir"} {
		for _, cleanupFirst := range []bool{false, true} {
			t.Run(action+map[bool]string{false: "/inspect", true: "/discard"}[cleanupFirst], func(t *testing.T) {
				d, b, cfg, opts := fixture(t)
				ctx := context.Background()
				var in storage.WriteIntent
				var effect storage.EffectIntent
				var stronger []byte
				var savedReady []byte
				b.putError = io.ErrUnexpectedEOF
				capture := func(_ context.Context, evidence []byte) error {
					var receipt receipt
					if err := json.Unmarshal(evidence, &receipt); err != nil {
						t.Fatal(err)
					}
					if receipt.Phase == "ready" {
						var err error
						savedReady, err = d.root.ReadFile(keyFor(action, receipt.ID) + ".json")
						if err != nil {
							t.Fatal(err)
						}
					}
					if receipt.Phase == "publishing" {
						stronger = bytes.Clone(evidence)
					}
					return nil
				}
				switch action {
				case "write":
					in = createIntent("regressed", "target", "payload")
					stage, err := d.Stage(ctx, in, strings.NewReader("payload"), capture)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = d.Publish(ctx, stage, capture); err == nil {
						t.Fatal("expected ambiguous native failure")
					}
				case "mkdir":
					effect = storage.EffectIntent{ID: "regressed", Action: "mkdir", Target: "target", Conditions: storage.Conditions{Absent: true}}
					_, err := d.Mkdir(ctx, storage.Mutation{ID: effect.ID, Path: effect.Target, Conditions: effect.Conditions}, capture)
					if err == nil {
						t.Fatal("expected ambiguous mkdir")
					}
				case "copy":
					b.putError = nil
					source := createIntent("source", "source", "payload")
					stage, err := d.Stage(ctx, source, strings.NewReader("payload"), cpOK)
					if err != nil {
						t.Fatal(err)
					}
					result, err := d.Publish(ctx, stage, cpOK)
					if err != nil {
						t.Fatal(err)
					}
					b.putError = io.ErrUnexpectedEOF
					effect = storage.EffectIntent{ID: "regressed", Action: "copy", Source: "source", Target: "target", SourceVersion: result.Entry.Version, Conditions: storage.Conditions{Absent: true}}
					_, err = d.Copy(ctx, storage.MoveRequest{ID: effect.ID, Source: effect.Source, Target: effect.Target, SourceVersion: effect.SourceVersion, TargetConditions: effect.Conditions}, capture)
					if err == nil {
						t.Fatal("expected ambiguous copy")
					}
				}
				if len(savedReady) == 0 || len(stronger) == 0 {
					t.Fatal("missing deterministic pre/post-RPC snapshots")
				}
				if err := d.root.WriteFile(keyFor(action, "regressed")+".json", savedReady, 0600); err != nil {
					t.Fatal(err)
				}
				restarted, err := New(&fakeTree{backend: b, root: cfg.ContentRoot}, cfg, b, opts)
				if err != nil {
					t.Fatal(err)
				}
				defer restarted.Close()
				heads := b.heads
				puts := b.puts
				if cleanupFirst {
					if action == "write" {
						err = restarted.Discard(ctx, in, stronger)
					} else {
						err = restarted.discard(ctx, action, effect.ID, nil, &effect, stronger)
					}
					requireCode(t, err, "outcome_unknown")
				}
				var inspected storage.Inspection
				if action == "write" {
					inspected, err = restarted.Inspect(ctx, in, stronger)
				} else {
					inspected, err = restarted.InspectEffect(ctx, effect, stronger)
				}
				if err != nil || inspected.State != storage.OutcomeUnknown {
					t.Fatalf("regressed intent became retryable %+v %v", inspected, err)
				}
				if b.heads != heads || b.puts != puts {
					t.Fatal("native observation or effect used to guess uncertainty")
				}
				r, err := restarted.load(action, "regressed")
				if err != nil || r.Phase != "unknown" {
					t.Fatal("unknown fence not persisted")
				}
				if _, err = restarted.root.Stat(restarted.dataName(r)); errorsIsNotExist(err) {
					t.Fatal("uncertain stage deleted")
				}
			})
		}
	}
}
func errorsIsNotExist(err error) bool { return os.IsNotExist(err) }

func TestObjectMismatchedAttemptReceiptCannotReopenReadyStage(t *testing.T) {
	d, _, _, _ := fixture(t)
	ctx := context.Background()
	in := createIntent("attempt", "file", "abc")
	stage, err := d.Stage(ctx, in, strings.NewReader("abc"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	var receipt receipt
	_ = json.Unmarshal(stage.Handle, &receipt)
	receipt.Attempt = "00000000000000000000000000000000"
	evidence, _ := json.Marshal(receipt)
	inspected, err := d.Inspect(ctx, in, evidence)
	if err != nil || inspected.State != storage.OutcomeUnknown {
		t.Fatalf("attempt mismatch was admitted %+v %v", inspected, err)
	}
}
