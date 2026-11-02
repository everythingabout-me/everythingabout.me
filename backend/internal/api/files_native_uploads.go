package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"sort"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

func (s *Server) receiveNativeUploadPart(w http.ResponseWriter, r *http.Request, native storage.NumberedUploadWriter, root fileops.Root, actor fileops.Actor, id string, intent storage.WriteIntent, number int, size int64, digest *storage.Digest) error {
	input := storage.UploadPart{Number: number, Size: size}
	if digest != nil {
		input.SHA256 = hex.EncodeToString(digest.Value)
	}
	var receipt fileops.PartReceipt
	var replay bool
	_, err := native.ReceivePart(r.Context(), intent, input, r.Body, s.filesJournal, func(part storage.UploadPart) (storage.UploadPart, error) {
		var err error
		receipt, replay, err = s.filesJournal.AcceptUploadPart(r.Context(), root, actor, id, fileops.PartReceipt{Number: part.Number, Size: part.Size, SHA256: part.SHA256, Handle: part.Handle})
		if err != nil {
			return part, err
		}
		return storage.UploadPart{Number: receipt.Number, Size: receipt.Size, SHA256: receipt.SHA256, Handle: receipt.Handle}, nil
	})
	if err != nil {
		return filesUploadSpoolProblem(err)
	}
	status := 201
	if replay {
		status = 200
	}
	return writeFilesJSON(w, status, filesUploadReceipt(receipt))
}

type nativeUploadWriter struct {
	fileops.NativeActions
	numbered storage.NumberedUploadWriter
	metadata storage.UploadMetadata
	parts    []storage.UploadPart
}

func (w *nativeUploadWriter) Stage(ctx context.Context, intent storage.WriteIntent, _ io.Reader, cp storage.Checkpoint) (storage.Staged, error) {
	return w.numbered.SealStage(ctx, intent, w.parts, w.metadata, cp)
}
func (w *nativeUploadWriter) Discard(ctx context.Context, intent storage.WriteIntent, evidence []byte) error {
	// Existing final-path evidence is authoritative. Never abort a native upload
	// when the publication driver still reports an unknown final request.
	if err := w.NativeActions.Discard(ctx, intent, evidence); err != nil {
		return err
	}
	return w.numbered.DiscardParts(ctx, intent, w.metadata)
}
func (s *Server) runNativeUploadOperation(ctx context.Context, op fileops.Operation, actor fileops.Actor, native fileops.NativeActions, numbered storage.NumberedUploadWriter, u fileops.Upload, service *fileops.Service) (fileops.Operation, error) {
	writer := &nativeUploadWriter{NativeActions: native, numbered: numbered, metadata: s.filesJournal}
	var err error
	if !op.Terminal() {
		if op.Phase == fileops.Admitted && !op.CancelRequested {
			var after, watermark int64
			for {
				page, mark, e := s.filesJournal.ListUploadParts(ctx, op.Root, actor, op.ID, 1000, after, watermark)
				if e != nil {
					return op, e
				}
				watermark = mark
				for _, part := range page {
					writer.parts = append(writer.parts, storage.UploadPart{Number: part.Number, Size: part.Size, SHA256: part.SHA256, Handle: part.Handle})
				}
				if len(writer.parts) > filesUploadMaxParts {
					return op, fileops.E("invalid_intent", nil)
				}
				if len(page) < 1000 {
					break
				}
				after = page[len(page)-1].Sequence
			}
			if len(writer.parts) != u.PartCount {
				return op, fileops.E("upload_incomplete", nil)
			}
			sort.Slice(writer.parts, func(i, j int) bool { return writer.parts[i].Number < writer.parts[j].Number })
			// Service.Write's Stage entry requires a body, but this adapter seals native
			// receipts only. No bytes are read, assembled, downloaded or resent here.
			op, err = service.Write(ctx, op.Root, actor, op.ID, writer, bytes.NewReader(nil))
		} else if op.Phase == fileops.StagedReady && op.State != fileops.Unknown {
			op, err = service.Write(ctx, op.Root, actor, op.ID, writer, nil)
		} else {
			op, err = service.Recover(ctx, op.Root, actor, op.ID, writer)
		}
	}
	if fileops.IsCode(err, "forbidden") || fileops.IsCode(err, "target_changed") || fileops.IsCode(err, "precondition_failed") {
		latest, e := s.filesJournal.Get(ctx, op.Root, actor, op.ID)
		if e == nil && (latest.Phase == fileops.Admitted || latest.Phase == fileops.Receiving || latest.Phase == fileops.StagedReady) {
			if _, e = s.filesJournal.RequestCancel(ctx, op.Root, actor, op.ID); e == nil {
				op, err = service.Recover(ctx, op.Root, actor, op.ID, writer)
			}
		}
	}
	if err == nil && op.Terminal() {
		err = service.Cleanup(ctx, op.Root, actor, op.ID, writer)
	}
	return op, err
}
