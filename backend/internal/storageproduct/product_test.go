package storageproduct

import (
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// Every product this service offers has to be publishable at all, and every product it
// hands presigned URLs for has to complete multipart uploads: those are the two promises
// the rest of the code assumes rather than checks at run time.
func TestEveryProductCanPublish(t *testing.T) {
	for _, id := range IDs() {
		profile, err := Lookup(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if profile.Backend == "local" {
			continue
		}
		if err := profile.Capabilities.Validate(); err != nil {
			t.Errorf("%s cannot publish: %v", id, err)
		}
		if profile.TransferClass == storage.TransferPresignedParts && !profile.MultipartUploads {
			t.Errorf("%s serves class A without multipart uploads, so a file above one part could never be completed", id)
		}
		if !profile.SingleWriter {
			t.Errorf("%s is served by one service but does not say so, which blocks adopting an empty owner", id)
		}
	}
}

// The two rows whose answers are not the obvious ones. A change here changes what the
// service is willing to do with a provider, so the test names the reason.
func TestKnownProductFacts(t *testing.T) {
	cosProfile, err := Lookup("cos")
	if err != nil {
		t.Fatal(err)
	}
	// COS enforces conditional GET and DELETE but replaces the object on a stale If-Match,
	// so a direct single-PUT replacement is never offered there.
	if cosProfile.ConditionalPut {
		t.Error("COS was given conditional PUT, which its measured transcript refuses")
	}

	rustfsProfile, err := Lookup("rustfs")
	if err != nil {
		t.Fatal(err)
	}
	// CVE-2026-27607: RustFS accepts presigned POST policy conditions without enforcing
	// them, and a form upload's signed policy is its whole guard.
	if rustfsProfile.FormPost {
		t.Error("RustFS was given form post, whose policy conditions it is known to ignore")
	}
	if !rustfsProfile.ConditionalCreate || !rustfsProfile.ConditionalPut {
		t.Error("RustFS lost the conditional writes its own s3-tests cover")
	}
}

func TestLookupNamesTheAlternatives(t *testing.T) {
	_, err := Lookup("s3")
	if err == nil {
		t.Fatal("a protocol family was accepted as a product")
	}
	for _, id := range IDs() {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the error does not name %q: %v", id, err)
		}
	}
}

// A backend that predates the product split has exactly one product when it has one, and
// none when the products differ — guessing there is what the split exists to stop.
func TestProductForBackendOnlyAnswersWhenThereIsOne(t *testing.T) {
	if id, ok := ProductForBackend("cos"); !ok || id != "cos" {
		t.Fatalf("COS resolved to %q, %t", id, ok)
	}
	if id, ok := ProductForBackend("local"); !ok || id != "local" {
		t.Fatalf("local resolved to %q, %t", id, ok)
	}
	if _, ok := ProductForBackend("s3"); ok {
		t.Fatal("the S3 family resolved to one product")
	}
}
