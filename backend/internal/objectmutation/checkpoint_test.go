package objectmutation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestObjectSpoolChangedDuringFinalCheckpointNeverReachesProvider(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	in := createIntent("changed-stage", "target", "good")
	stage, err := d.Stage(ctx, in, strings.NewReader("good"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Publish(ctx, stage, func(_ context.Context, evidence []byte) error {
		var receipt receipt
		_ = json.Unmarshal(evidence, &receipt)
		if receipt.Phase == "publishing" {
			r, e := d.load("write", in.ID)
			if e != nil {
				return e
			}
			return d.root.WriteFile(d.dataName(r), []byte("evil"), 0600)
		}
		return nil
	})
	if err == nil || b.puts != 0 {
		t.Fatal("corrupted acknowledged bytes reached provider")
	}
}
func TestObjectDeleteRevalidatesAfterFinalCheckpoint(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	in := createIntent("create", "target", "old")
	stage, err := d.Stage(ctx, in, strings.NewReader("old"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.Publish(ctx, stage, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Remove(ctx, storage.Mutation{ID: "remove", Path: "target", Conditions: storage.Conditions{ExpectedVersion: result.Entry.Version}}, func(_ context.Context, evidence []byte) error {
		var receipt receipt
		_ = json.Unmarshal(evidence, &receipt)
		if receipt.Phase == "publishing" {
			_, e := b.Put(ctx, Put{Key: "drive/owner/target", Body: strings.NewReader("new"), Size: 3, MediaType: "text/plain"})
			return e
		}
		return nil
	})
	if err == nil || b.deletes != 0 {
		t.Fatal("delayed Delete changed a newer generation")
	}
	current, err := d.Stat(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(current.Version, result.Entry.Version) {
		t.Fatal("replacement fixture did not change native version")
	}
}
func TestObjectReplacementRetainsHistoryAndIndependentCurrentGeneration(t *testing.T) {
	d, b, _, _ := fixture(t)
	ctx := context.Background()
	original := createIntent("first", "target", "old")
	stage, err := d.Stage(ctx, original, strings.NewReader("old"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.Publish(ctx, stage, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	replacement := createIntent("replace", "target", "newer")
	replacement.Conditions = storage.Conditions{ExpectedVersion: first.Entry.Version}
	stage, err = d.Stage(ctx, replacement, strings.NewReader("newer"), cpOK)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := d.Stat(ctx, "target")
	if !bytes.Equal(before.Version, first.Entry.Version) {
		t.Fatal("receiving bytes replaced current content")
	}
	second, err := d.Publish(ctx, stage, cpOK)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Entry.Version, second.Entry.Version) {
		t.Fatal("replacement generation did not change")
	}
	historical, err := b.Open(ctx, string(first.Entry.NativeRef), strings.TrimPrefix(string(first.Entry.Version), "native-version:"))
	if err != nil {
		t.Fatal(err)
	}
	historical.Close()
	if !d.Describe().RetainsVersions || d.Describe().NativeConditionalMutation {
		t.Fatal("incorrect retained physical/native CAS capabilities")
	}
}
