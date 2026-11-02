package objectmutation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validAdmissionFixture() Admission {
	return Admission{ConnectionID: "fixture", Endpoint: "https://objects.example", Bucket: "bucket", Region: "region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated-positive-negative-probes", AtomicPublish: true, VersionedReads: true, ConditionalCreate: true, CurrentKeyDelete: true, CoordinatedExclusiveWriter: true, SingleGateway: true, LegacyWritersDrained: true}
}
func TestAdmissionExplicitConnectionAndCapabilityEvidence(t *testing.T) {
	a := validAdmissionFixture()
	if err := ValidateAdmission(a); err != nil {
		t.Fatal(err)
	}
	if !a.Matches("fixture", "https://objects.example", "bucket", "region") || a.Matches("other", "https://objects.example", "bucket", "region") {
		t.Fatal("connection evidence was not exact")
	}
	for _, field := range []string{"AtomicPublish", "VersionedReads", "ConditionalCreate", "CurrentKeyDelete", "CoordinatedExclusiveWriter", "SingleGateway", "LegacyWritersDrained"} {
		b := a
		switch field {
		case "AtomicPublish":
			b.AtomicPublish = false
		case "VersionedReads":
			b.VersionedReads = false
		case "ConditionalCreate":
			b.ConditionalCreate = false
		case "CurrentKeyDelete":
			b.CurrentKeyDelete = false
		case "CoordinatedExclusiveWriter":
			b.CoordinatedExclusiveWriter = false
		case "SingleGateway":
			b.SingleGateway = false
		case "LegacyWritersDrained":
			b.LegacyWritersDrained = false
		}
		if ValidateAdmission(b) == nil {
			t.Fatalf("missing %s was admitted", field)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.json")
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAdmission(path)
	if err != nil || loaded.ConnectionID != a.ConnectionID {
		t.Fatalf("valid evidence: %+v %v", loaded, err)
	}
	for _, bad := range [][]byte{[]byte(`{}`), append(data, []byte(` {}`)...), []byte(`{"unknown":true}`), make([]byte, (1<<20)+1)} {
		if err = os.WriteFile(path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadAdmission(path); err == nil {
			t.Fatal("invalid evidence accepted")
		}
	}
	if _, err = LoadAdmission("relative.json"); err == nil {
		t.Fatal("relative evidence file admitted")
	}
}
