package objectmutation

import (
	"context"
	"errors"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestObjectMkdirUnderImplicitPrefixFolder(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	ctx := context.Background()
	b.current[cfg.ContentRoot+"Prefix/child"] = Object{Key: cfg.ContentRoot + "Prefix/child", ETag: "external", Size: 1}
	parent, err := d.Stat(ctx, "Prefix")
	if err != nil || parent.Kind != storage.Folder || parent.Revision != nil {
		t.Fatalf("implicit parent=%+v %v", parent, err)
	}
	made, err := d.Mkdir(ctx, storage.Mutation{ID: "mkdir-implicit", Path: "Prefix/sub", Conditions: storage.Conditions{Absent: true}}, cpOK)
	if err != nil || made.Entry.Kind != storage.Folder || len(made.Entry.Revision) == 0 {
		t.Fatalf("mkdir under implicit prefix=%+v %v", made, err)
	}
	if _, ok := b.current[cfg.ContentRoot+"Prefix/sub/"]; !ok {
		t.Fatal("mkdir did not publish a native folder marker")
	}
	if _, ok := b.current[cfg.ContentRoot+"Prefix/"]; ok {
		t.Fatal("mkdir invented a marker for the implicit parent")
	}
}

func TestObjectRecursiveDeleteOfImplicitFolder(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	ctx := context.Background()
	b.current[cfg.ContentRoot+"Prefix/child"] = Object{Key: cfg.ContentRoot + "Prefix/child", ETag: "external", Size: 1}
	folder, err := d.Stat(ctx, "Prefix")
	if err != nil || folder.Revision != nil {
		t.Fatalf("implicit folder=%+v %v", folder, err)
	}
	_, err = d.Remove(ctx, storage.Mutation{ID: "early", Path: "Prefix", Conditions: storage.Conditions{ImplicitFolder: true}}, cpOK)
	var none *storage.NoFinalEffect
	if !errors.As(err, &none) {
		t.Fatalf("nonempty implicit folder delete=%v", err)
	}
	// Children first, then the anchor, as a recursive delete plans it.
	child, err := d.Stat(ctx, "Prefix/child")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Remove(ctx, storage.Mutation{ID: "child", Path: "Prefix/child", Conditions: storage.Conditions{ExpectedRevision: child.Revision}}, cpOK); err != nil {
		t.Fatal(err)
	}
	deletes := b.deletes
	result, err := d.Remove(ctx, storage.Mutation{ID: "anchor", Path: "Prefix", Conditions: storage.Conditions{ImplicitFolder: true}}, cpOK)
	if err != nil || len(result.Evidence) == 0 {
		t.Fatalf("implicit folder delete=%+v %v", result, err)
	}
	if b.deletes != deletes {
		t.Fatal("implicit folder delete issued a native request")
	}
	if _, err = d.Stat(ctx, "Prefix"); !storage.IsCode(err, "not_found") {
		t.Fatalf("implicit folder survived: %v", err)
	}
}
