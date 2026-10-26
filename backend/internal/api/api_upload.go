package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
)

const (
	// 预签名上传地址的有效期，也是上传会话的有效期。
	uploadTTL = time.Hour
	// 单次 PutObject 的上限；超过这个就走分片。
	uploadSingleMax = int64(5) << 30
	// 单个对象的上限，给个保守值。
	uploadMaxObject = int64(5) << 40 // 5 TiB
	// 分片大小下限（COS 要求除最后一片外不小于 1 MiB）。
	uploadMinPartSize = int64(5) << 20
	// 一次响应里最多给多少个分片地址，控制响应体积。
	uploadMaxParts = 1000
	isoFormat      = "2006-01-02T15:04:05.000Z"
)

type apiUploadBody struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Mime      string `json:"mime"`
	Overwrite bool   `json:"overwrite"`
}

type apiCompleteBody struct {
	UploadID string `json:"uploadId"`
	Parts    []struct {
		PartNumber int    `json:"partNumber"`
		ETag       string `json:"etag"`
	} `json:"parts"`
}

type uploadSession struct {
	ID               string
	UserID           string
	Path             string
	Size             int64
	Mime             string
	PartSize         int64
	ProviderUploadID string
	ExpiresAt        time.Time
	Completed        bool
	Cancelled        bool
}

func newUploadID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "u_" + hex.EncodeToString(buf), nil
}

// partSizeFor 选一个分片大小，让分片数不超过 uploadMaxParts。
func (s *Server) partSizeFor(size int64) int64 {
	partSize := s.cfg.UploadPartSize
	if partSize < uploadMinPartSize {
		partSize = uploadMinPartSize
	}
	if count := (size + partSize - 1) / partSize; count > uploadMaxParts {
		partSize = (size + uploadMaxParts - 1) / uploadMaxParts
		if partSize < uploadMinPartSize {
			partSize = uploadMinPartSize
		}
	}
	return partSize
}

// handleAPIUpload 建一个上传会话，下发预签名地址。
// 小文件给一个 PUT 地址；超过阈值的给一组分片地址。
func (s *Server) handleAPIUpload(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[apiUploadBody](r)
	if err != nil {
		return err
	}

	path, err := paths.Normalize(body.Path)
	if err != nil {
		return err
	}
	if path == "" {
		return httpx.BadRequest("路径不能为空")
	}
	if err := p.RequireWrite(path); err != nil {
		return err
	}
	if body.Size <= 0 {
		return httpx.BadRequest("文件大小不合法")
	}
	if body.Size > uploadMaxObject {
		return httpx.NewError(http.StatusRequestEntityTooLarge, "单个对象上限 5 TiB")
	}

	mime := strings.TrimSpace(body.Mime)
	if mime == "" {
		mime = "application/octet-stream"
	}

	existing, err := s.nodes.Live(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.IsDir {
		return httpx.Conflict("同名目录已存在")
	}
	if existing != nil && !body.Overwrite {
		return httpx.Conflict("文件已存在；要覆盖请在请求体里加 overwrite: true")
	}
	if err := s.checkQuota(r.Context(), p.User.ID, body.Size, existing); err != nil {
		return err
	}
	// 同一个路径有历史软删除记录时，先挪走，否则新对象会覆盖它。

	// 同一路径只允许一个未完成的上传。
	if err := s.cancelPendingUploads(r.Context(), p.User.ID, path); err != nil {
		return err
	}
	s.cleanupExpiredUploads(r.Context())

	id, err := newUploadID()
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(uploadTTL).UTC()
	key := paths.KeyFor(path, p.User.ID)

	threshold := s.cfg.MultipartThreshold
	if threshold <= 0 {
		threshold = uploadSingleMax
	}

	if body.Size <= threshold {
		if _, err := s.db.Pool().Exec(r.Context(),
			`insert into upload_sessions (id, user_id, path, size, mime, part_size, expires_at)
			 values ($1, $2::uuid, $3, $4, $5, $4, $6)`,
			id, p.User.ID, path, body.Size, mime, expiresAt); err != nil {
			return err
		}
		url, err := s.storage.PutURL(r.Context(), key, int(uploadTTL.Seconds()))
		if err != nil {
			return err
		}
		s.recordAudit(r, p, "upload", path, http.StatusOK, map[string]any{
			"size": body.Size, "uploadId": id, "mode": "single",
		})
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"uploadId":  id,
			"mode":      "single",
			"partSize":  body.Size,
			"expiresAt": expiresAt.Format(isoFormat),
			"parts": []map[string]any{{
				"partNumber": 1,
				"url":        url,
				"headers":    map[string]string{"Content-Type": mime},
			}},
		})
		return nil
	}

	// 分片上传：先在 COS 建 multipart，再给每一片签一个 PUT 地址。
	partSize := s.partSizeFor(body.Size)
	count := int((body.Size + partSize - 1) / partSize)
	providerID, err := s.storage.InitiateMultipart(r.Context(), key)
	if err != nil {
		return err
	}

	parts := make([]map[string]any, 0, count)
	for number := 1; number <= count; number++ {
		url, err := s.storage.MultipartPartURL(
			r.Context(), key, providerID, number, int(uploadTTL.Seconds()),
		)
		if err != nil {
			// 签不出来就别留一个半死不活的 multipart 在桶里。
			_ = s.storage.AbortMultipart(context.Background(), key, providerID)
			return err
		}
		parts = append(parts, map[string]any{
			"partNumber": number,
			"url":        url,
			"headers":    map[string]string{"Content-Type": mime},
		})
	}

	if _, err := s.db.Pool().Exec(r.Context(),
		`insert into upload_sessions (id, user_id, path, size, mime, part_size, expires_at, provider_upload_id)
		 values ($1, $2::uuid, $3, $4, $5, $6, $7, $8)`,
		id, p.User.ID, path, body.Size, mime, partSize, expiresAt, providerID); err != nil {
		_ = s.storage.AbortMultipart(context.Background(), key, providerID)
		return err
	}

	s.recordAudit(r, p, "upload", path, http.StatusOK, map[string]any{
		"size": body.Size, "uploadId": id, "mode": "multipart", "parts": count,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"uploadId":  id,
		"mode":      "multipart",
		"partSize":  partSize,
		"expiresAt": expiresAt.Format(isoFormat),
		"parts":     parts,
	})
	return nil
}

// handleAPIComplete 在客户端传完字节后调用：校验对象、落索引。
func (s *Server) handleAPIComplete(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[apiCompleteBody](r)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body.UploadID) == "" {
		return httpx.BadRequest("uploadId 不能为空")
	}

	session, err := s.loadUploadSession(r.Context(), body.UploadID)
	if err != nil {
		return err
	}
	// 别人的会话也返回 404，不泄露存在性。
	if session == nil || session.UserID != p.User.ID {
		return httpx.NotFound("上传会话不存在")
	}
	if session.Completed {
		return httpx.Conflict("这次上传已经完成过了")
	}
	if session.Cancelled {
		return httpx.Conflict("这次上传已被同一路径的新上传取代")
	}
	if time.Now().After(session.ExpiresAt) {
		return httpx.NewError(http.StatusGone, "上传会话已过期，请重新发起")
	}
	if err := p.RequireWrite(session.Path); err != nil {
		return err
	}

	key := paths.KeyFor(session.Path, p.User.ID)

	var info *storage.ObjectInfo
	if session.ProviderUploadID != "" {
		parts, err := completeParts(body.Parts)
		if err != nil {
			return err
		}
		info, err = s.storage.CompleteMultipart(r.Context(), key, session.ProviderUploadID, parts)
		if err != nil {
			return err
		}
	} else {
		info, err = s.storage.Head(r.Context(), key)
		if err != nil {
			return err
		}
	}
	if info == nil {
		return httpx.BadRequest("对象不存在，上传可能还没完成")
	}
	if info.Size != session.Size {
		return httpx.BadRequest(fmt.Sprintf(
			"大小不一致：会话声明 %d 字节，实际对象 %d 字节", session.Size, info.Size))
	}

	if err := s.nodes.EnsureParents(r.Context(), session.Path, p.User.ID); err != nil {
		return err
	}

	var etag, storageClass *string
	if info.ETag != "" {
		etag = &info.ETag
	}
	if info.StorageClass != "" {
		storageClass = &info.StorageClass
	}
	mime := session.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}

	node, err := s.nodes.UpsertFile(r.Context(), nodes.UpsertInput{
		OwnerID:      p.User.ID,
		Path:         session.Path,
		Size:         info.Size,
		Mime:         &mime,
		ETag:         etag,
		StorageClass: storageClass,
		Meta:         map[string]any{"mime": mime},
	})
	if err != nil {
		return err
	}

	if _, err := s.db.Pool().Exec(r.Context(),
		`update upload_sessions set completed_at = now() where id = $1`, session.ID); err != nil {
		// 索引已经写了，这里失败只影响幂等判断，不值得把请求判失败。
		slog.Warn("upload: 标记会话完成失败", "uploadId", session.ID, "error", err)
	}

	if node.ETag != nil {
		w.Header().Set("ETag", `"`+*node.ETag+`"`)
	}
	s.recordAudit(r, p, "upload_complete", session.Path, http.StatusCreated, map[string]any{
		"size": info.Size, "uploadId": session.ID, "mode": sessionMode(session),
	})
	httpx.WriteJSON(w, http.StatusCreated, toAPINode(node))
	return nil
}

func sessionMode(session *uploadSession) string {
	if session.ProviderUploadID != "" {
		return "multipart"
	}
	return "single"
}

func completeParts(raw []struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
}) ([]storage.MultipartPart, error) {
	if len(raw) == 0 {
		return nil, httpx.BadRequest("分片上传需要 parts（每个分片的 partNumber 和 etag）")
	}
	parts := make([]storage.MultipartPart, 0, len(raw))
	seen := make(map[int]bool, len(raw))
	for _, part := range raw {
		etag := strings.Trim(strings.TrimSpace(part.ETag), `"`)
		if part.PartNumber <= 0 || etag == "" {
			return nil, httpx.BadRequest("parts 里有不合法的分片")
		}
		if seen[part.PartNumber] {
			return nil, httpx.BadRequest("parts 里有重复的分片号")
		}
		seen[part.PartNumber] = true
		parts = append(parts, storage.MultipartPart{PartNumber: part.PartNumber, ETag: etag})
	}
	return parts, nil
}

// cancelPendingUploads 把同一路径没完成的会话标成取消，并 abort 掉 COS 侧的
// multipart（否则碎片会一直占空间）。
func (s *Server) cancelPendingUploads(ctx context.Context, userID, path string) error {
	rows, err := s.db.Pool().Query(ctx,
		`select id, provider_upload_id from upload_sessions
		  where user_id = $1::uuid and path = $2
		    and completed_at is null and cancelled_at is null`, userID, path)
	if err != nil {
		return err
	}
	type pending struct{ id, provider string }
	var list []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.id, &item.provider); err != nil {
			rows.Close()
			return err
		}
		list = append(list, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, item := range list {
		if item.provider != "" {
			if err := s.storage.AbortMultipart(ctx, paths.KeyFor(path, userID), item.provider); err != nil {
				slog.Warn("upload: abort 旧的分片上传失败", "providerUploadId", item.provider, "error", err)
			}
		}
	}
	_, err = s.db.Pool().Exec(ctx,
		`update upload_sessions set cancelled_at = now()
		  where user_id = $1::uuid and path = $2
		    and completed_at is null and cancelled_at is null`, userID, path)
	return err
}

// cleanupExpiredUploads 清掉一天前过期、还没完成的分片会话，顺手 abort。
func (s *Server) cleanupExpiredUploads(ctx context.Context) {
	rows, err := s.db.Pool().Query(ctx,
		`select id, path, user_id::text, provider_upload_id from upload_sessions
		  where expires_at < now() - interval '1 day'
		    and completed_at is null and provider_upload_id <> ''`)
	if err != nil {
		return
	}
	type expired struct{ id, path, userID, provider string }
	var list []expired
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.id, &item.path, &item.userID, &item.provider); err != nil {
			rows.Close()
			return
		}
		list = append(list, item)
	}
	rows.Close()

	for _, item := range list {
		if err := s.storage.AbortMultipart(ctx, paths.KeyFor(item.path, item.userID), item.provider); err != nil {
			slog.Warn("upload: abort 过期分片失败", "uploadId", item.id, "error", err)
		}
	}
	_, _ = s.db.Pool().Exec(ctx,
		`delete from upload_sessions where expires_at < now() - interval '1 day' and completed_at is null`)
}

func (s *Server) loadUploadSession(ctx context.Context, id string) (*uploadSession, error) {
	var (
		session     uploadSession
		completedAt *time.Time
		cancelledAt *time.Time
	)
	err := s.db.Pool().QueryRow(ctx,
		`select id, user_id::text, path, size, mime, part_size, provider_upload_id,
		        expires_at, completed_at, cancelled_at
		   from upload_sessions
		  where id = $1`, id).
		Scan(&session.ID, &session.UserID, &session.Path, &session.Size, &session.Mime,
			&session.PartSize, &session.ProviderUploadID, &session.ExpiresAt,
			&completedAt, &cancelledAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	session.Completed = completedAt != nil
	session.Cancelled = cancelledAt != nil
	return &session, nil
}
