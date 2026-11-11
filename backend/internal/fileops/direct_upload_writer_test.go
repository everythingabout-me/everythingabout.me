package fileops

import (
	"bytes"
	"context"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// directTestNative answers the only call a direct publication makes of the driver. The
// embedded interface is nil: a test that reaches another method is testing the wrong thing.
type directTestNative struct {
	NativeActions
	entry storage.Entry
}

func (d directTestNative) Stat(context.Context, storage.Path) (storage.Entry, error) {
	return d.entry, nil
}

// A direct publication is proven by the target itself, so the writer must answer with that
// observation as evidence. Without it Service.Write refuses the applied result, and the
// commit retries as outcome_unknown forever while the file sits at the destination,
// unsettled and unindexed — the state a 23k-file import reached with every PUT succeeding.
func TestDirectWriterAnswersWithTheObservedRevision(t *testing.T) {
	entry := storage.Entry{Path: "trip/a.txt", Kind: storage.File, Size: 5, Revision: []byte(`native-etag:"5d41402abc4b2a76b9719d911017c592"`)}
	writer := NewDirectUploadWriter(directTestNative{entry: entry})
	intent := storage.WriteIntent{Target: entry.Path, Size: entry.Size, Conditions: storage.Conditions{Absent: true}}
	ctx := context.Background()

	staged, err := writer.Stage(ctx, intent, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staged.Handle, entry.Revision) {
		t.Fatalf("stage handle %q", staged.Handle)
	}
	result, err := writer.Publish(ctx, staged, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateNative(intent, result); err != nil {
		t.Fatalf("the settlement pipeline would reject this publication: %v", err)
	}
	if !bytes.Equal(result.Evidence, entry.Revision) {
		t.Fatalf("publication evidence %q is not the observed revision", result.Evidence)
	}

	// A lost attempt is answered the same way: Inspect proves the object, so its result has
	// to be as acceptable to the pipeline as a fresh Publish.
	inspection, err := writer.Inspect(ctx, intent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.State != storage.Applied || inspection.Result == nil {
		t.Fatalf("inspection %+v", inspection)
	}
	if err = validateNative(intent, *inspection.Result); err != nil {
		t.Fatalf("recovery would reject the observed publication: %v", err)
	}
	if !bytes.Equal(inspection.Result.Evidence, entry.Revision) {
		t.Fatalf("recovery evidence %q is not the observed revision", inspection.Result.Evidence)
	}
}
