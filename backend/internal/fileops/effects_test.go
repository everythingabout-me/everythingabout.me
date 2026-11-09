package fileops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// effectJournal exercises coordinator boundaries only; real Postgres guard,
// lease and reservation invariants are tested by the journal integration suite.
// Embedding Journal makes an unexpected settlement/cleanup call fail immediately.
type effectJournal struct {
	Journal
	op                          Operation
	calls                       []string
	beginErr, claimErr, saveErr error
	afterCheckpointError        error
	failState                   string
	onClaim                     func(*Operation)
}

func (j *effectJournal) Get(ctx context.Context, root Root, actor Actor, id string) (Operation, error) {
	j.calls = append(j.calls, "get")
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	if id != j.op.ID || root != j.op.Root || actor.ID != j.op.PrincipalID {
		return Operation{}, E("forbidden", nil)
	}
	return cloneOperation(j.op), nil
}
func (j *effectJournal) Claim(_ context.Context, _ Root, _ Actor, _ string, _ time.Duration) (Operation, error) {
	j.calls = append(j.calls, "claim")
	if j.claimErr != nil {
		return Operation{}, j.claimErr
	}
	j.op.Lease.Epoch++
	j.op.Lease.Token = fmt.Sprint(j.op.Lease.Epoch)
	if j.onClaim != nil {
		j.onClaim(&j.op)
	}
	return cloneOperation(j.op), nil
}
func (j *effectJournal) Renew(_ context.Context, l Lease, _ time.Duration) (Lease, error) {
	return l, nil
}
func (j *effectJournal) Release(_ context.Context, l Lease) error {
	j.calls = append(j.calls, "release")
	if l.Epoch != j.op.Lease.Epoch {
		return E("lease_lost", nil)
	}
	return nil
}
func (j *effectJournal) BeginEntry(_ context.Context, l Lease, n int) error {
	j.calls = append(j.calls, "begin")
	if j.beginErr != nil {
		return j.beginErr
	}
	if l.Epoch != j.op.Lease.Epoch {
		return E("lease_lost", nil)
	}
	for i := range j.op.Entries {
		e := &j.op.Entries[i]
		if e.Number == n {
			if e.State != "pending" {
				return E("recovery_required", nil)
			}
			e.State = "started"
			j.op.Phase = EffectStarted
			return nil
		}
	}
	return E("not_found", nil)
}
func (j *effectJournal) SaveEntry(_ context.Context, l Lease, n int, state string, checkpoint, evidence []byte, result json.RawMessage) error {
	j.calls = append(j.calls, "save:"+state)
	if j.failState == state && j.saveErr != nil {
		return j.saveErr
	}
	if l.Epoch != j.op.Lease.Epoch {
		return E("lease_lost", nil)
	}
	for i := range j.op.Entries {
		e := &j.op.Entries[i]
		if e.Number == n {
			if e.State != "started" && e.State != "outcome_unknown" {
				return E("invalid_transition", nil)
			}
			if state == "started" && e.State != "started" {
				return E("invalid_transition", nil)
			}
			e.State = state
			e.Checkpoint = bytes.Clone(checkpoint)
			e.Evidence = bytes.Clone(evidence)
			e.Result = bytes.Clone(result)
			if state == "outcome_unknown" {
				j.op.State = Unknown
			} else {
				j.op.State = Running
			}
			if state == "started" && j.afterCheckpointError != nil {
				return j.afterCheckpointError
			}
			return nil
		}
	}
	return E("not_found", nil)
}
func cloneOperation(op Operation) Operation {
	b, _ := json.Marshal(op)
	var copy Operation
	_ = json.Unmarshal(b, &copy)
	return copy
}

type effectRecovery func(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error)

func (effectRecovery) StorageBinding() storage.Config {
	return storage.Config{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "content", SystemRoot: "system"}
}
func (effectRecovery) StorageNameRules() storage.NameRules {
	return storage.NameRules{CaseSensitive: true}
}

func (f effectRecovery) InspectEffect(ctx context.Context, intent storage.EffectIntent, checkpoint []byte) (storage.Inspection, error) {
	return f(ctx, intent, checkpoint)
}
func newEffectFixture(t *testing.T, action string) (*Service, *effectJournal, Actor) {
	t.Helper()
	root := Root{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "content", SystemRoot: "system", CaseSensitive: true}
	entry := EntryEffect{ManifestEntry: ManifestEntry{Number: 7, Action: action, Source: "source", Target: "target", SourceRevision: []byte("captured-source"), TargetConditions: storage.Conditions{Absent: true}, Size: 3}, State: "pending"}
	switch action {
	case "mkdir":
		entry.Source = ""
		entry.Size = 0
	case "delete":
		entry.Target = ""
	}
	j := &effectJournal{op: Operation{ID: "op-fixture", Root: root, PrincipalID: "principal", Kind: action, State: Running, Phase: Admitted, Guards: []Guard{{Path: "source", Subtree: true}, {Path: "target", Subtree: true}}, Entries: []EntryEffect{entry}, Lease: Lease{OperationID: "op-fixture"}}}
	s := &Service{journal: j, options: ServiceOptions{LeaseTTL: time.Hour, Authorize: func(context.Context, Operation) error { j.calls = append(j.calls, "authorize"); return nil }}}
	return s, j, Actor{ID: "principal", CanRead: true, CanWrite: true}
}
func nativeEntryResult(action string) storage.MutationResult {
	result := storage.MutationResult{Evidence: []byte("terminal-native-evidence")}
	if action != "delete" {
		kind := storage.File
		size := int64(3)
		if action == "mkdir" {
			kind = storage.Folder
			size = 0
		}
		result.Entry = &storage.Entry{Path: "target", Kind: kind, Size: size, NativeRef: []byte("private-reference"), Revision: []byte("private-version")}
	}
	return result
}
func neverInspect(t *testing.T) storage.EffectRecovery {
	return effectRecovery(func(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error) {
		t.Fatal("unexpected recovery")
		return storage.Inspection{}, nil
	})
}
func forbiddenEffect(t *testing.T) func(context.Context, storage.EffectIntent, storage.Checkpoint) (storage.MutationResult, error) {
	return func(context.Context, storage.EffectIntent, storage.Checkpoint) (storage.MutationResult, error) {
		t.Fatal("final effect replayed")
		return storage.MutationResult{}, nil
	}
}

func TestApplyEntryJournalsIntentAndCheckpointsBeforeNativeSuccess(t *testing.T) {
	for _, action := range []string{"mkdir", "move", "copy", "delete"} {
		t.Run(action, func(t *testing.T) {
			s, j, actor := newEffectFixture(t, action)
			original := cloneOperation(j.op)
			calls := 0
			op, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, neverInspect(t), func(ctx context.Context, intent storage.EffectIntent, save storage.Checkpoint) (storage.MutationResult, error) {
				calls++
				j.calls = append(j.calls, "effect")
				if j.op.Entries[0].State != "started" {
					t.Fatal("native effect preceded durable BeginEntry")
				}
				if intent.ID != "op-fixture/entry/7" || intent.Action != action || intent.Source != original.Entries[0].Source || intent.Target != original.Entries[0].Target || !bytes.Equal(intent.SourceRevision, original.Entries[0].SourceRevision) || !reflect.DeepEqual(intent.Conditions, original.Entries[0].TargetConditions) {
					t.Fatalf("immutable intent mismatch: %+v", intent)
				}
				if err := save(ctx, []byte("allocated-native-id")); err != nil {
					return storage.MutationResult{}, err
				}
				if string(j.op.Entries[0].Checkpoint) != "allocated-native-id" {
					t.Fatal("checkpoint was not durable")
				}
				// Callback-owned slices cannot mutate the admitted manifest or recovery intent.
				intent.SourceRevision[0] = 'X'
				return nativeEntryResult(action), nil
			})
			if err != nil || op.Entries[0].State != "applied" || calls != 1 {
				t.Fatalf("result %+v %v", op, err)
			}
			var private storage.MutationResult
			if err = json.Unmarshal(op.Entries[0].Result, &private); err != nil || !reflect.DeepEqual(private, nativeEntryResult(action)) {
				t.Fatalf("saved private result %+v %v", private, err)
			}
			if !bytes.Equal(j.op.Entries[0].SourceRevision, original.Entries[0].SourceRevision) {
				t.Fatal("callback changed manifest")
			}
			if !reflect.DeepEqual(op.Guards, original.Guards) || op.State != Running {
				t.Fatal("entry success settled operation or released guards")
			}
			want := []string{"get", "claim", "authorize", "begin", "authorize", "effect", "save:started", "save:applied", "get", "release"}
			if !reflect.DeepEqual(j.calls, want) {
				t.Fatalf("call order %v", j.calls)
			}
			j.calls = nil
			again, err := s.ApplyEntry(context.Background(), op.Root, actor, op.ID, 7, neverInspect(t), forbiddenEffect(t))
			if err != nil || !bytes.Equal(again.Entries[0].Result, op.Entries[0].Result) || !reflect.DeepEqual(j.calls, []string{"get"}) {
				t.Fatalf("replay %v %v", j.calls, err)
			}
		})
	}
}

func TestApplyEntryLostNativeReplyInspectsWithoutRepeating(t *testing.T) {
	s, j, actor := newEffectFixture(t, "copy")
	inspections := 0
	recovery := effectRecovery(func(_ context.Context, intent storage.EffectIntent, checkpoint []byte) (storage.Inspection, error) {
		inspections++
		j.calls = append(j.calls, "inspect")
		if intent.ID != "op-fixture/entry/7" || string(checkpoint) != "copy-native-id" || string(intent.SourceRevision) != "captured-source" {
			t.Fatalf("lost saved recovery identity %+v %q", intent, checkpoint)
		}
		result := nativeEntryResult("copy")
		return storage.Inspection{State: storage.Applied, Result: &result}, nil
	})
	op, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, recovery, func(ctx context.Context, intent storage.EffectIntent, save storage.Checkpoint) (storage.MutationResult, error) {
		if err := save(ctx, []byte("copy-native-id")); err != nil {
			return storage.MutationResult{}, err
		}
		intent.SourceRevision[0] = 'X'
		return storage.MutationResult{}, ioFailure
	})
	if err != nil || op.Entries[0].State != "applied" || inspections != 1 {
		t.Fatalf("lost reply outcome %+v %v", op, err)
	}
}

var ioFailure = errors.New("lost native response")

func TestStartedOrUnknownEntryAlwaysInspectsAfterNewLease(t *testing.T) {
	for _, state := range []string{"started", "outcome_unknown"} {
		t.Run(state, func(t *testing.T) {
			s, j, actor := newEffectFixture(t, "move")
			j.op.Entries[0].State = state
			j.op.Entries[0].Checkpoint = []byte("captured")
			j.op.Phase = EffectStarted
			j.op.State = Unknown
			inspections := 0
			recovery := effectRecovery(func(_ context.Context, _ storage.EffectIntent, cp []byte) (storage.Inspection, error) {
				inspections++
				if string(cp) != "captured" {
					t.Fatal("checkpoint lost")
				}
				return storage.Inspection{State: storage.OutcomeUnknown, Evidence: []byte("request-may-arrive")}, nil
			})
			op, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, recovery, forbiddenEffect(t))
			if !IsCode(err, "outcome_unknown") || op.Entries[0].State != "outcome_unknown" || len(op.Guards) != 2 || inspections != 1 {
				t.Fatalf("unknown resolved falsely: %+v %v", op, err)
			}
			op, err = s.RecoverEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, recovery)
			if !IsCode(err, "outcome_unknown") || inspections != 2 || op.Lease.Epoch != 2 {
				t.Fatalf("new worker retried effect: %+v %v", op, err)
			}
		})
	}
}

func TestNoFinalEffectErrorStillRequiresRecoveryAndEvidence(t *testing.T) {
	for _, state := range []storage.PublishState{storage.NotApplied, storage.OutcomeUnknown, storage.StagedReady} {
		t.Run(string(state), func(t *testing.T) {
			s, j, actor := newEffectFixture(t, "delete")
			inspections := 0
			recovery := effectRecovery(func(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error) {
				inspections++
				return storage.Inspection{State: state, Evidence: []byte("native-terminal-or-uncertain-evidence")}, nil
			})
			op, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, recovery, func(context.Context, storage.EffectIntent, storage.Checkpoint) (storage.MutationResult, error) {
				return storage.MutationResult{}, &storage.NoFinalEffect{Cause: ioFailure}
			})
			if inspections != 1 {
				t.Fatal("NoFinalEffect bypassed native reconciliation")
			}
			if state == storage.NotApplied {
				if !IsCode(err, "native_not_applied") || op.Entries[0].State != "not_applied" || len(op.Guards) != 2 {
					t.Fatalf("definite result %+v %v", op, err)
				}
				_, err = s.ApplyEntry(context.Background(), op.Root, actor, op.ID, 7, recovery, forbiddenEffect(t))
				if !IsCode(err, "native_not_applied") || inspections != 1 {
					t.Fatal("terminal not_applied was retried")
				}
			} else if !IsCode(err, "outcome_unknown") || op.State != Unknown {
				t.Fatalf("nonterminal inspection was treated as rollback %+v %v", op, err)
			}
		})
	}
}

func TestEntryPersistenceFailureKeepsStartedForRecovery(t *testing.T) {
	s, j, actor := newEffectFixture(t, "copy")
	j.failState = "applied"
	j.saveErr = errors.New("database unavailable after native success")
	op, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, neverInspect(t), func(context.Context, storage.EffectIntent, storage.Checkpoint) (storage.MutationResult, error) {
		return nativeEntryResult("copy"), nil
	})
	if !errors.Is(err, j.saveErr) || j.op.Entries[0].State != "started" || len(j.op.Guards) != 2 {
		t.Fatalf("native success was rolled back: %+v %v", op, err)
	}
	j.saveErr = nil
	result := nativeEntryResult("copy")
	op, err = s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, effectRecovery(func(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error) {
		return storage.Inspection{State: storage.Applied, Result: &result}, nil
	}), forbiddenEffect(t))
	if err != nil || op.Entries[0].State != "applied" {
		t.Fatalf("saved effect not recovered %+v %v", op, err)
	}
}

func TestInvalidEntryResultsAndMissingEvidenceRemainUnknown(t *testing.T) {
	cases := []struct {
		name, action string
		result       storage.MutationResult
	}{
		{"missing evidence", "copy", storage.MutationResult{Entry: &storage.Entry{Path: "target", Kind: storage.File, Size: 3}}},
		{"wrong target", "copy", storage.MutationResult{Entry: &storage.Entry{Path: "another-owner", Kind: storage.File, Size: 3}, Evidence: []byte("proof")}},
		{"wrong size", "move", storage.MutationResult{Entry: &storage.Entry{Path: "target", Kind: storage.File, Size: 4}, Evidence: []byte("proof")}},
		{"folder expected", "mkdir", storage.MutationResult{Entry: &storage.Entry{Path: "target", Kind: storage.File}, Evidence: []byte("proof")}},
		{"delete result", "delete", storage.MutationResult{Entry: &storage.Entry{Path: "source", Kind: storage.File}, Evidence: []byte("proof")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, j, actor := newEffectFixture(t, tc.action)
			op, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, neverInspect(t), func(context.Context, storage.EffectIntent, storage.Checkpoint) (storage.MutationResult, error) {
				return tc.result, nil
			})
			if !IsCode(err, "outcome_unknown") || op.Entries[0].State != "outcome_unknown" || len(op.Entries[0].Result) != 0 && string(op.Entries[0].Result) != "null" {
				t.Fatalf("invalid result applied: %+v %v", op, err)
			}
		})
	}
	for _, inspection := range []storage.Inspection{{State: storage.Applied}, {State: storage.NotApplied}, {State: storage.Applied, Result: &storage.MutationResult{}}} {
		s, j, actor := newEffectFixture(t, "copy")
		j.op.Phase = EffectStarted
		j.op.Entries[0].State = "started"
		op, err := s.RecoverEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, effectRecovery(func(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error) {
			return inspection, nil
		}))
		if !IsCode(err, "outcome_unknown") || op.Entries[0].State != "outcome_unknown" {
			t.Fatalf("invalid inspection applied %+v %v", op, err)
		}
	}
}

type unboundEffectRecovery struct{}

func (unboundEffectRecovery) InspectEffect(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error) {
	panic("unbound recovery reached native storage")
}

type alteredEffectRecovery struct {
	effectRecovery
	binding storage.Config
	names   storage.NameRules
}

func (r alteredEffectRecovery) StorageBinding() storage.Config      { return r.binding }
func (r alteredEffectRecovery) StorageNameRules() storage.NameRules { return r.names }

func TestEntryRejectsUnboundOrMismatchedRecoveryBeforeJournal(t *testing.T) {
	s, j, actor := newEffectFixture(t, "copy")
	for _, recoverOnly := range []bool{false, true} {
		var err error
		if recoverOnly {
			_, err = s.RecoverEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, unboundEffectRecovery{})
		} else {
			_, err = s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, unboundEffectRecovery{}, forbiddenEffect(t))
		}
		if !IsCode(err, "unsupported") || len(j.calls) != 0 {
			t.Fatalf("unbound executor accessed journal: %v %v", j.calls, err)
		}
	}
	for _, field := range []string{"owner", "connection", "generation", "content", "system", "names"} {
		t.Run(field, func(t *testing.T) {
			base := effectRecovery(func(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error) {
				t.Fatal("mismatched recovery inspected native storage")
				return storage.Inspection{}, nil
			})
			r := alteredEffectRecovery{effectRecovery: base, binding: base.StorageBinding(), names: base.StorageNameRules()}
			switch field {
			case "owner":
				r.binding.OwnerID = "other"
			case "connection":
				r.binding.ConnectionID = "other"
			case "generation":
				r.binding.Generation++
			case "content":
				r.binding.ContentRoot = "other"
			case "system":
				r.binding.SystemRoot = "other"
			case "names":
				r.names.CaseSensitive = false
			}
			for _, recoverOnly := range []bool{false, true} {
				var err error
				if recoverOnly {
					_, err = s.RecoverEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, r)
				} else {
					_, err = s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, r, forbiddenEffect(t))
				}
				if !IsCode(err, "root_conflict") || len(j.calls) != 0 {
					t.Fatalf("mismatched executor accessed journal: %v %v", j.calls, err)
				}
			}
		})
	}
}

func TestEntryAdmissionLeaseAndAuthorizationFailuresHaveNoEffect(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Service, *effectJournal)
	}{
		{"authorization denied", func(s *Service, _ *effectJournal) {
			s.options.Authorize = func(context.Context, Operation) error { return E("forbidden", nil) }
		}},
		{"lease owned", func(_ *Service, j *effectJournal) { j.claimErr = E("operation_in_progress", nil) }},
		{"begin database failure", func(_ *Service, j *effectJournal) { j.beginErr = errors.New("durable intent unavailable") }},
		{"cancel pending", func(_ *Service, j *effectJournal) { j.op.CancelRequested = true }},
		{"unsupported action", func(_ *Service, j *effectJournal) { j.op.Entries[0].Action = "arbitrary-task" }},
		{"missing guard", func(_ *Service, j *effectJournal) { j.op.Guards = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, j, actor := newEffectFixture(t, "copy")
			tc.change(s, j)
			_, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, neverInspect(t), forbiddenEffect(t))
			if err == nil {
				t.Fatal("inadmissible effect accepted")
			}
		})
	}
	s, j, actor := newEffectFixture(t, "copy")
	if _, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, nil, forbiddenEffect(t)); !IsCode(err, "unsupported") {
		t.Fatal("missing native recovery admitted")
	}
	if _, err := s.RecoverEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, neverInspect(t)); !IsCode(err, "recovery_required") {
		t.Fatal("pending effect recovered as applied")
	}
}

func TestEntryRepairAndRecordedReplaySurvivePublicationRevocation(t *testing.T) {
	for _, state := range []string{"started", "outcome_unknown"} {
		t.Run(state, func(t *testing.T) {
			s, j, actor := newEffectFixture(t, "copy")
			j.op.Phase = EffectStarted
			j.op.Entries[0].State = state
			authorizations := 0
			s.options.Authorize = func(context.Context, Operation) error { authorizations++; return E("forbidden", nil) }
			inspections := 0
			recovery := effectRecovery(func(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error) {
				inspections++
				result := nativeEntryResult("copy")
				return storage.Inspection{State: storage.Applied, Result: &result}, nil
			})
			op, err := s.RecoverEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, recovery)
			if err != nil || op.Entries[0].State != "applied" || inspections != 1 || authorizations != 0 {
				t.Fatalf("publication revocation blocked native repair: %+v %v inspect%d auth%d", op, err, inspections, authorizations)
			}
			_, err = s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, recovery, forbiddenEffect(t))
			if err != nil || authorizations != 0 || inspections != 1 {
				t.Fatalf("recorded replay required publication rights: %v", err)
			}
			// The same currently valid journal actor still cannot start new effects.
			j.op.Entries[0].State = "pending"
			j.op.Entries[0].Result = nil
			j.op.Entries[0].Evidence = nil
			j.op.Phase = Admitted
			_, err = s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, recovery, forbiddenEffect(t))
			if !IsCode(err, "forbidden") || authorizations != 1 || j.op.Entries[0].State != "pending" {
				t.Fatalf("revoked publication admitted fresh effect: %+v %v", j.op, err)
			}
		})
	}
}

func TestAuthorizationChangeAfterStartInspectsInsteadOfRollback(t *testing.T) {
	s, j, actor := newEffectFixture(t, "copy")
	checks := 0
	s.options.Authorize = func(context.Context, Operation) error {
		checks++
		if checks == 2 {
			return E("forbidden", nil)
		}
		return nil
	}
	recovered := 0
	op, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, effectRecovery(func(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error) {
		recovered++
		return storage.Inspection{State: storage.OutcomeUnknown}, nil
	}), forbiddenEffect(t))
	if !IsCode(err, "outcome_unknown") || recovered != 1 || op.Entries[0].State != "outcome_unknown" {
		t.Fatalf("started authorization failure lost guard %+v %v", op, err)
	}
}

func TestEntryCheckpointLostAckUsesLatestDurableEvidence(t *testing.T) {
	s, j, actor := newEffectFixture(t, "copy")
	j.afterCheckpointError = errors.New("lost checkpoint acknowledgement")
	inspection := effectRecovery(func(_ context.Context, _ storage.EffectIntent, checkpoint []byte) (storage.Inspection, error) {
		if string(checkpoint) != "durably-saved-id" {
			t.Fatalf("recovery used stale checkpoint %q", checkpoint)
		}
		return storage.Inspection{State: storage.OutcomeUnknown, Evidence: []byte("still-in-flight")}, nil
	})
	op, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, inspection, func(ctx context.Context, _ storage.EffectIntent, save storage.Checkpoint) (storage.MutationResult, error) {
		return storage.MutationResult{}, save(ctx, []byte("durably-saved-id"))
	})
	if !IsCode(err, "outcome_unknown") || string(op.Entries[0].Checkpoint) != "durably-saved-id" {
		t.Fatalf("checkpoint loss was hidden: %+v %v", op, err)
	}
}

func TestEntryLateCheckpointIsRejectedAndCrashRetainsStarted(t *testing.T) {
	s, j, actor := newEffectFixture(t, "copy")
	var late storage.Checkpoint
	_, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, neverInspect(t), func(_ context.Context, _ storage.EffectIntent, save storage.Checkpoint) (storage.MutationResult, error) {
		late = save
		return nativeEntryResult("copy"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := cloneOperation(j.op)
	if err = late(context.Background(), []byte("late")); !IsCode(err, "invalid_transition") {
		t.Fatalf("late checkpoint accepted: %v", err)
	}
	if !equalJSON(before, j.op) {
		t.Fatal("late callback changed terminal entry")
	}
	s, j, actor = newEffectFixture(t, "copy")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("native crash did not propagate")
			}
		}()
		_, _ = s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, neverInspect(t), func(context.Context, storage.EffectIntent, storage.Checkpoint) (storage.MutationResult, error) {
			panic("native worker crashed")
		})
	}()
	if j.op.Entries[0].State != "started" || len(j.op.Guards) != 2 {
		t.Fatal("crash cleared uncertain effect")
	}
}

func TestEntryCheckpointFailureAndLeaseLossNeverReplay(t *testing.T) {
	for _, leaseLost := range []bool{false, true} {
		t.Run(fmt.Sprint(leaseLost), func(t *testing.T) {
			s, j, actor := newEffectFixture(t, "copy")
			inspections := 0
			recovery := effectRecovery(func(context.Context, storage.EffectIntent, []byte) (storage.Inspection, error) {
				inspections++
				return storage.Inspection{State: storage.OutcomeUnknown}, nil
			})
			_, err := s.ApplyEntry(context.Background(), j.op.Root, actor, j.op.ID, 7, recovery, func(ctx context.Context, _ storage.EffectIntent, save storage.Checkpoint) (storage.MutationResult, error) {
				if leaseLost {
					j.op.Lease.Epoch++
				} else {
					j.failState = "started"
					j.saveErr = errors.New("checkpoint database failure")
				}
				if err := save(ctx, []byte("unsafe-unacknowledged-checkpoint")); err != nil {
					return storage.MutationResult{}, err
				}
				t.Fatal("checkpoint failure acknowledged")
				return storage.MutationResult{}, nil
			})
			if err == nil || inspections != 1 || len(j.op.Guards) != 2 {
				t.Fatalf("lease/checkpoint failure accepted %+v %v", j.op, err)
			}
			if leaseLost && j.op.Entries[0].State != "started" {
				t.Fatal("stale worker changed journal state")
			}
		})
	}
}
