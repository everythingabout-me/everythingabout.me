package objectmutation

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func TestObjectUnsupportedIngressCanBeSealedWithoutAllocation(t *testing.T) {
	for _, name := range []string{"source-modified", "above-limit", "digest-algorithm"} {
		t.Run(name, func(t *testing.T) {
			d, b, _, _ := fixture(t)
			ctx := context.Background()
			in := createIntent(name, "target", "abc")
			switch name {
			case "source-modified":
				v := time.Now()
				in.SourceModifiedAt = &v
			case "above-limit":
				in.Size = d.options.MaxPublishBytes + 1
			case "digest-algorithm":
				in.Digest = &storage.Digest{Algorithm: "unsupported", Value: []byte("hash")}
			}
			if _, err := d.Stage(ctx, in, strings.NewReader("abc"), cpOK); err == nil {
				t.Fatal("unsupported ingress admitted")
			}
			if _, err := d.load("write", in.ID); !os.IsNotExist(err) {
				t.Fatalf("rejected ingress allocated intent before cleanup: %v", err)
			}
			if err := d.Discard(ctx, in, nil); err != nil {
				t.Fatal("rejected intent cannot release its known-unused reservation", err)
			}
			inspected, err := d.Inspect(ctx, in, nil)
			if err != nil || inspected.State != storage.NotApplied {
				t.Fatalf("rejected intent resurrected: %+v %v", inspected, err)
			}
			if b.puts != 0 || b.deletes != 0 {
				t.Fatal("unsupported cleanup changed native content")
			}
		})
	}
}
