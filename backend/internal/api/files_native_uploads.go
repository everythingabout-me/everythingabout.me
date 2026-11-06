package api

import (
	"bytes"
	"context"
	"encoding/hex"
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

func (s *Server) runNativeUploadOperation(ctx context.Context, op fileops.Operation, actor fileops.Actor, native fileops.NativeActions, numbered storage.NumberedUploadWriter, u fileops.Upload, service *fileops.Service) (fileops.Operation, error) {
	writer := fileops.NewNativeUploadWriter(native, numbered, s.filesJournal)
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
					writer.Parts = append(writer.Parts, storage.UploadPart{Number: part.Number, Size: part.Size, SHA256: part.SHA256, Handle: part.Handle})
				}
				if len(writer.Parts) > filesUploadMaxParts {
					return op, fileops.E("invalid_intent", nil)
				}
				if len(page) < 1000 {
					break
				}
				after = page[len(page)-1].Sequence
			}
			if len(writer.Parts) != u.PartCount {
				return op, fileops.E("upload_incomplete", nil)
			}
			sort.Slice(writer.Parts, func(i, j int) bool { return writer.Parts[i].Number < writer.Parts[j].Number })
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

func (s *Server) runPlannedUploadOperation(ctx context.Context, op fileops.Operation, actor fileops.Actor, native fileops.NativeActions, planned storage.PlannedUploadWriter, u fileops.Upload, service *fileops.Service) (fileops.Operation, error) {
	writer := fileops.NewPlannedUploadWriter(native, planned, s.filesJournal)
	var err error
	if !op.Terminal() {
		switch {
		case op.Phase == fileops.Admitted && !op.CancelRequested:
			// Stage seals the provider's part listing; the empty body is never read.
			op, err = service.Write(ctx, op.Root, actor, op.ID, writer, bytes.NewReader(nil))
		case op.Phase == fileops.StagedReady && op.State != fileops.Unknown:
			op, err = service.Write(ctx, op.Root, actor, op.ID, writer, nil)
		default:
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
