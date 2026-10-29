package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/httpx"
)

// SaveLinkTokenPrefix distinguishes save links from ordinary API tokens in the audit log.
const SaveLinkTokenPrefix = "link:"

const (
	saveLinkIDLen  = 8
	saveLinkMACLen = 8
	saveLinkLen    = saveLinkIDLen + saveLinkMACLen
)

var saveLinkEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// SaveLink is one save link as shown on the account page. Code is recomputed by the
// server per row and never stored.
type SaveLink struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Code       string  `json:"code"`
	Path       string  `json:"path"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
	Revoked    bool    `json:"revoked"`
}

// Plaintext of a save link: <8-char link_id><8-char MAC>, all lowercase base32.
// MAC = the first 8 characters of HMAC-SHA256(server key, user_id + ":" + link_id).
func (s *Service) saveLinkCode(userID, linkID string) string {
	mac := hmac.New(sha256.New, s.linkKey)
	mac.Write([]byte(userID))
	mac.Write([]byte(":"))
	mac.Write([]byte(linkID))
	encoded := strings.ToLower(saveLinkEncoding.EncodeToString(mac.Sum(nil)))
	return linkID + encoded[:saveLinkMACLen]
}

func parseSaveLinkCode(code string) (linkID, mac string, ok bool) {
	code = strings.ToLower(strings.TrimSpace(code))
	if len(code) != saveLinkLen {
		return "", "", false
	}
	for _, char := range code {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			return "", "", false
		}
	}
	return code[:saveLinkIDLen], code[saveLinkIDLen:], true
}

func newSaveLinkID() (string, error) {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(saveLinkEncoding.EncodeToString(buf))[:saveLinkIDLen], nil
}

func (s *Service) saveLinkFromRow(id, linkID, name, createdAt string, lastUsed *string, revoked bool, userID string) SaveLink {
	code := s.saveLinkCode(userID, linkID)
	return SaveLink{
		ID:         id,
		Name:       name,
		Code:       code,
		Path:       "/s/" + code,
		CreatedAt:  createdAt,
		LastUsedAt: lastUsed,
		Revoked:    revoked,
	}
}

// CreateSaveLink mints a save link with full privileges. The plaintext never hits the
// database, so the account page can recompute it at any time.
func (s *Service) CreateSaveLink(ctx context.Context, userID string) (*SaveLink, error) {
	name := "Save link"
	var (
		link SaveLink
		id   string
	)
	for attempt := 0; attempt < 5; attempt++ {
		linkID, err := newSaveLinkID()
		if err != nil {
			return nil, err
		}
		var createdAt string
		err = s.db.Pool().QueryRow(ctx,
			`insert into save_links (link_id, user_id, name)
			 values ($1, $2::uuid, $3)
			 on conflict (link_id) do nothing
			 returning id::text, `+isoUTC("created_at"),
			linkID, userID, name).Scan(&id, &createdAt)
		if err == nil {
			link = s.saveLinkFromRow(id, linkID, name, createdAt, nil, false, userID)
			return &link, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	return nil, httpx.Internal("failed to allocate a save link; please retry")
}

// ListSaveLinks returns every link on the account, revoked ones included, so the account
// page can show status.
func (s *Service) ListSaveLinks(ctx context.Context, userID string) ([]SaveLink, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select id::text, link_id, name,
		        `+isoUTC("created_at")+`,
		        case when last_used_at is null then null else `+isoUTC("last_used_at")+` end,
		        revoked_at is not null
		   from save_links
		  where user_id = $1::uuid
		  order by created_at desc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	links := make([]SaveLink, 0, 4)
	for rows.Next() {
		var (
			id, linkID, name, createdAt string
			lastUsed                    *string
			revoked                     bool
		)
		if err := rows.Scan(&id, &linkID, &name, &createdAt, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		links = append(links, s.saveLinkFromRow(id, linkID, name, createdAt, lastUsed, revoked, userID))
	}
	return links, rows.Err()
}

// RevokeSaveLink invalidates a link immediately.
func (s *Service) RevokeSaveLink(ctx context.Context, userID, id string) error {
	tag, err := s.db.Pool().Exec(ctx,
		`update save_links set revoked_at = now()
		  where user_id = $1::uuid and id = $2::uuid and revoked_at is null`,
		userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("save link not found")
	}
	return nil
}

// VerifySaveLink checks the short code from the URL and returns an identity with the same
// privileges as the user themself. On failure it returns (nil, nil); the caller decides
// whether that means a 404 or falling through to another auth method.
func (s *Service) VerifySaveLink(ctx context.Context, code string) (*Principal, error) {
	linkID, mac, ok := parseSaveLinkCode(code)
	if !ok {
		return nil, nil
	}

	var (
		token     Token
		lastUsed  *string
		createdAt string
	)
	err := s.db.Pool().QueryRow(ctx,
		`select id::text, user_id::text, name,
		        `+isoUTC("created_at")+`,
		        case when last_used_at is null then null else `+isoUTC("last_used_at")+` end
		   from save_links
		  where link_id = $1
		    and revoked_at is null
		  limit 1`, linkID).
		Scan(&token.ID, &token.userID, &token.Name, &createdAt, &lastUsed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	want := s.saveLinkCode(token.userID, linkID)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(want[saveLinkIDLen:])) != 1 {
		return nil, nil
	}

	token.ID = SaveLinkTokenPrefix + linkID
	token.Kind = KindApp
	token.Prefix = ""
	token.CanRead = true
	token.CanWrite = true
	token.CanEvents = true
	token.CreatedAt = createdAt
	token.LastUsedAt = lastUsed

	// Update at most once a minute so every request does not hit the database.
	_, _ = s.db.Pool().Exec(ctx,
		`update save_links set last_used_at = now()
		  where link_id = $1
		    and (last_used_at is null or last_used_at < now() - interval '1 minute')`, linkID)

	user, err := s.FindUserByID(ctx, token.userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, nil
	}
	return &Principal{User: ToUser(user), Token: &token}, nil
}
