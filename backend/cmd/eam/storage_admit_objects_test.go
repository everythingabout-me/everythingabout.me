package main

import (
	"encoding/json"
	"testing"

	"everythingabout.me/backend/internal/config"
)

func TestAdmissionQuiescenceRequired(t *testing.T) {
	if validateAdmissionQuiescence(false) == nil {
		t.Fatal("unquiesced admission accepted")
	}
	if err := validateAdmissionQuiescence(true); err != nil {
		t.Fatalf("quiesced admission rejected: %v", err)
	}
}

func TestObjectAdmissionRequiresExplicitRetainedPhysicalEvidence(t *testing.T) {
	valid := json.RawMessage(`{"writers":"drained","retainedVersionInventory":"inventory://owner/root/generations-20261102"}`)
	for _, backend := range []string{"cos", "s3"} {
		t.Run(backend, func(t *testing.T) {
			cfg := &config.Config{Storage: config.Storage{Backend: backend, CoordinatedWrites: true}}
			if objects, err := validateAdmissionAccounting(cfg, valid, true, 19); err != nil || !objects {
				t.Fatalf("valid explicit object accounting rejected: %t %v", objects, err)
			}
			for _, tc := range []struct {
				name     string
				proof    json.RawMessage
				provided bool
				physical int64
			}{
				{"missing declaration", valid, false, -1},
				{"negative declaration", valid, true, -1},
				{"unsafe integer", valid, true, 9007199254740992},
				{"missing inventory", json.RawMessage(`{"writers":"drained"}`), true, 19},
				{"empty inventory", json.RawMessage(`{"retainedVersionInventory":" "}`), true, 19},
				{"nonstring inventory", json.RawMessage(`{"retainedVersionInventory":true}`), true, 19},
				{"control in reference", json.RawMessage(`{"retainedVersionInventory":"native\nsecret"}`), true, 19},
				{"array proof", json.RawMessage(`[{"retainedVersionInventory":"inventory"}]`), true, 19},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if _, err := validateAdmissionAccounting(cfg, tc.proof, tc.provided, tc.physical); err == nil {
						t.Fatal("incomplete physical evidence admitted")
					}
				})
			}
		})
	}
}
func TestLocalAdmissionPhysicalMeasurementCannotBeOverridden(t *testing.T) {
	cfg := &config.Config{Storage: config.Storage{Backend: "local", CoordinatedWrites: true}}
	proof := json.RawMessage(`{"writers":"drained"}`)
	if objects, err := validateAdmissionAccounting(cfg, proof, false, -1); err != nil || objects {
		t.Fatalf("Local behavior changed: %t %v", objects, err)
	}
	for _, amount := range []int64{0, 7, 100} {
		if _, err := validateAdmissionAccounting(cfg, proof, true, amount); err == nil {
			t.Fatal("Local physical count could be overridden")
		}
	}
	if physical, err := admissionPhysicalBytes(false, 7, -1); err != nil || physical != 7 {
		t.Fatalf("Local measured bytes changed %d %v", physical, err)
	}
	cfg.Storage.CoordinatedWrites = false
	if _, err := validateAdmissionAccounting(cfg, proof, false, -1); err == nil {
		t.Fatal("writer admission assertion was bypassed")
	}
}
func TestObjectAdmissionPhysicalCountIncludesCurrentAndRetainedBytes(t *testing.T) {
	for _, tc := range []struct {
		logical, declared int64
		valid             bool
	}{{7, 7, true}, {7, 25, true}, {0, 0, true}, {7, 6, false}, {7, -1, false}, {7, 9007199254740992, false}, {-1, 7, false}} {
		got, err := admissionPhysicalBytes(true, tc.logical, tc.declared)
		if tc.valid {
			if err != nil || got != tc.declared {
				t.Fatalf("valid baseline rejected: %+v got=%d err=%v", tc, got, err)
			}
		} else if err == nil {
			t.Fatalf("understated baseline admitted: %+v", tc)
		}
	}
}
func TestStorageAdmitRejectsInvalidObjectPhysicalArgumentsBeforeConfig(t *testing.T) {
	for _, raw := range []string{"-1", "-2", "9007199254740992"} {
		if err := runStorageAdmit([]string{"--owner-id", "11111111-1111-1111-1111-111111111111", "--writers-quiesced", "--evidence", "missing-proof", "--physical-bytes", raw}); err == nil {
			t.Fatalf("invalid physical argument accepted: %s", raw)
		}
	}
}
