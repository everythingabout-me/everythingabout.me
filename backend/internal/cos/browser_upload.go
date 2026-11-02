package cos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"everythingabout.me/backend/internal/browserupload"
	"everythingabout.me/backend/internal/paths"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const browserPartBytes = 16 << 20
const browserMarker = "x-cos-meta-eam-upload"

var browserReceiveSlots = make(chan struct{}, 4)

type browserStage struct {
	UploadID string       `json:"uploadId,omitempty"`
	Parts    []cos.Object `json:"parts,omitempty"`
	Ready    bool         `json:"ready,omitempty"`
	SHA256   string       `json:"sha256,omitempty"`
}

// Stage receives exact bytes into unpublished native multipart state. Only this
// backend can complete it; no provider credentials or native handles reach a client.
func (c *Client) Stage(ctx context.Context, session browserupload.Session, body io.Reader, checkpoint func(context.Context, json.RawMessage) error) (json.RawMessage, error) {
	select {
	case browserReceiveSlots <- struct{}{}:
		defer func() { <-browserReceiveSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	stage := browserStage{}
	save := func() error {
		data, err := json.Marshal(stage)
		if err != nil {
			return err
		}
		return checkpoint(ctx, data)
	}
	key := paths.KeyFor(session.Path, session.OwnerID)
	if session.Size > 0 {
		// Allocation can leave only unpublished fragments if its reply is lost.
		// Persist its identity immediately when it is known, before sending any parts.
		headers := http.Header{}
		headers.Set(browserMarker, session.ID)
		result, _, err := c.publishClient.Object.InitiateMultipartUpload(ctx, key, &cos.InitiateMultipartUploadOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{ContentType: session.Mime, XOptionHeader: &headers}})
		if err != nil {
			return nil, err
		}
		stage.UploadID = result.UploadID
		if err = save(); err != nil {
			return nil, err
		}
	}
	digest := sha256.New()
	remaining := session.Size
	buffer := make([]byte, int(min(remaining, browserPartBytes)))
	for number := 1; remaining > 0; number++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		length := min(remaining, browserPartBytes)
		part := buffer[:int(length)]
		if _, err := io.ReadFull(body, part); err != nil {
			return nil, fmt.Errorf("incomplete upload body: %w", err)
		}
		_, _ = digest.Write(part)
		response, err := c.publishClient.Object.UploadPart(ctx, key, stage.UploadID, number, bytes.NewReader(part), &cos.ObjectUploadPartOptions{ContentLength: length})
		if err != nil {
			return nil, err
		}
		stage.Parts = append(stage.Parts, cos.Object{PartNumber: number, ETag: response.Header.Get("ETag")})
		remaining -= length
		if err = save(); err != nil {
			return nil, err
		}
	}
	var extra [1]byte
	if count, err := io.ReadFull(body, extra[:]); count != 0 || err != io.EOF {
		return nil, fmt.Errorf("upload body exceeds declared size or did not end")
	}
	stage.Ready = true
	stage.SHA256 = hex.EncodeToString(digest.Sum(nil))
	if err := save(); err != nil {
		return nil, err
	}
	return json.Marshal(stage)
}

// Publish sends final-path I/O once. A failure is inspected, never replayed as
// another write; successful publication is followed immediately by index commit.
func (c *Client) Publish(ctx context.Context, session browserupload.Session, data json.RawMessage) (bool, error) {
	var stage browserStage
	if err := json.Unmarshal(data, &stage); err != nil || !stage.Ready {
		return false, fmt.Errorf("upload staging is incomplete")
	}
	key := paths.KeyFor(session.Path, session.OwnerID)
	if session.Size == 0 {
		headers := http.Header{}
		headers.Set(browserMarker, session.ID)
		_, err := c.publishClient.Object.Put(ctx, key, bytes.NewReader(nil), &cos.ObjectPutOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{ContentType: session.Mime, XOptionHeader: &headers}})
		return err == nil, err
	}
	if stage.UploadID == "" || len(stage.Parts) == 0 {
		return false, fmt.Errorf("multipart staging is incomplete")
	}
	_, _, err := c.publishClient.Object.CompleteMultipartUpload(ctx, key, stage.UploadID, &cos.CompleteMultipartUploadOptions{Parts: stage.Parts})
	return err == nil, err
}

func (c *Client) Inspect(ctx context.Context, session browserupload.Session) (browserupload.Inspection, error) {
	response, err := c.publishClient.Object.Head(ctx, paths.KeyFor(session.Path, session.OwnerID), nil)
	if err != nil {
		if cos.IsNotFoundError(err) {
			return browserupload.Inspection{}, nil
		}
		return browserupload.Inspection{}, err
	}
	if response.Header.Get(browserMarker) != session.ID {
		return browserupload.Inspection{}, nil
	}
	// A marker identifies this attempt. A lost final response does not prove that
	// all native I/O is terminal. Retain only that unresolved attempt for recovery.
	return browserupload.Inspection{Applied: true, Terminal: false}, nil
}

func (c *Client) Discard(ctx context.Context, session browserupload.Session) error {
	var stage browserStage
	if len(session.Checkpoint) > 0 {
		if err := json.Unmarshal(session.Checkpoint, &stage); err != nil {
			return err
		}
	}
	if stage.UploadID == "" {
		return nil
	}
	_, err := c.publishClient.Object.AbortMultipartUpload(ctx, paths.KeyFor(session.Path, session.OwnerID), stage.UploadID)
	if cos.IsNotFoundError(err) {
		return nil
	}
	return err
}

// BrowserUploadMarker verifies attempt attribution before client metadata is applied.
func (c *Client) BrowserUploadMarker(ctx context.Context, ownerID, path, id string) error {
	response, err := c.publishClient.Object.Head(ctx, paths.KeyFor(path, ownerID), nil)
	if err != nil {
		return err
	}
	if response.Header.Get(browserMarker) != id {
		return fmt.Errorf("uploaded object no longer matches its receipt")
	}
	return nil
}

var _ browserupload.Driver = (*Client)(nil)
