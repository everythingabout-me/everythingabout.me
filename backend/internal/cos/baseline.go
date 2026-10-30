package cos

import (
	"context"
	"fmt"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storagebaseline"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

// CaptureBaseline enumerates exact keys without the legacy reconcile path cleanup.
// Success proves pagination completed, not that external writers were quiescent.
func (c *Client) CaptureBaseline(ctx context.Context, prefix string) ([]storagebaseline.NativeEntry, error) {
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("baseline requires an explicit root prefix")
	}
	entries := []storagebaseline.NativeEntry{}
	marker := ""
	seen := map[string]bool{}
	for {
		page, _, err := c.client.Bucket.Get(ctx, &sdk.BucketGetOptions{Prefix: prefix, Marker: marker, MaxKeys: 1000})
		if err != nil {
			return nil, fmt.Errorf("native baseline listing failed: %w", err)
		}
		if page == nil {
			return nil, fmt.Errorf("native baseline listing returned no page")
		}
		for _, object := range page.Contents {
			if !strings.HasPrefix(object.Key, prefix) || seen[object.Key] {
				return nil, fmt.Errorf("native baseline listing returned an outside-root or duplicate key")
			}
			seen[object.Key] = true
			modified, err := time.Parse(time.RFC3339, object.LastModified)
			if err != nil {
				return nil, fmt.Errorf("native baseline listing returned an invalid modification time")
			}
			entries = append(entries, storagebaseline.NativeEntry{Key: object.Key, Size: object.Size, ETag: strings.Trim(object.ETag, `"`), ModifiedAt: modified})
		}
		if !page.IsTruncated {
			return entries, nil
		}
		if len(page.Contents) == 0 {
			return nil, fmt.Errorf("native baseline listing truncated an empty page")
		}
		next := page.NextMarker
		if next == "" {
			next = page.Contents[len(page.Contents)-1].Key
		}
		if next <= marker {
			return nil, fmt.Errorf("native baseline listing did not advance")
		}
		marker = next
	}
}
