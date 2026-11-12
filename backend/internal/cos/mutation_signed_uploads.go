package cos

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

var _ objectmutation.SignedPartBackend = (*mutationBackend)(nil)
var _ objectmutation.ReadSignerBackend = (*mutationBackend)(nil)

// SignedPartsSupported reports the native multipart capability; class A signs with
// the configured COS credential, which never leaves the server.
func (b *mutationBackend) SignedPartsSupported() bool { return b.client != nil }

func (b *mutationBackend) SignPart(ctx context.Context, key, id string, number int, size int64, expires time.Time) (storage.SignedPartGrant, error) {
	var out storage.SignedPartGrant
	if b.client == nil {
		return out, storage.NewError("unsupported", storage.Path(key), nil)
	}
	query := url.Values{"partNumber": {strconv.Itoa(number)}, "uploadId": {id}}
	// A class A grant binds the exact part length and no content digest. The
	// length is deterministic for the session geometry, so it can be signed.
	headers := http.Header{"Content-Length": {strconv.FormatInt(size, 10)}, "Content-Type": {"application/octet-stream"}}
	duration := time.Until(expires)
	if duration <= 0 || duration > time.Hour {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	signed, err := b.client.Object.GetPresignedURL2(ctx, http.MethodPut, key, duration, &sdk.PresignedURLOptions{Query: &query, Header: &headers}, true)
	if err != nil {
		return out, err
	}
	return storage.SignedPartGrant{
		Number: number, Size: size, Method: http.MethodPut, URL: signed.String(),
		Headers:   map[string]string{"Content-Length": strconv.FormatInt(size, 10), "Content-Type": "application/octet-stream"},
		ExpiresAt: expires.UTC(),
	}, nil
}

// SignPut signs one conditional PUT at the destination itself. The condition is part of
// the signed headers, so a client cannot publish past it, and the length is bound the same
// way a part length is.
func (b *mutationBackend) SignPut(ctx context.Context, key string, size int64, mediaType string, cond storage.Conditions, expires time.Time) (storage.SignedPartGrant, error) {
	var out storage.SignedPartGrant
	if b.client == nil || size <= 0 || key == "" {
		return out, storage.NewError("unsupported", storage.Path(key), nil)
	}
	headers := http.Header{"Content-Length": {strconv.FormatInt(size, 10)}}
	if mediaType != "" {
		headers.Set("Content-Type", mediaType)
	} else {
		headers.Set("Content-Type", "application/octet-stream")
	}
	if cond.Absent {
		// Both the standard conditional header and the provider's own switch: one of them
		// is honoured by every admitted COS generation, and neither lets an existing object
		// be replaced.
		headers.Set("If-None-Match", "*")
		headers.Set("x-cos-forbid-overwrite", "true")
	} else {
		// The revision is the ETag *in header form*: a COS ETag arrives quoted, and
		// nativeRevision keeps it that way, so the value is `"6a1c…"` — quotes and all.
		// Quoting it again produced `""6a1c…""`, and the injection guard that rejected a
		// quote refused every revision this service signs, because every revision carries
		// one. A grant is signed by the server, never by the client, so the header form is
		// what must be bound: strip a surrounding pair, refuse the rest.
		etag := strings.Trim(strings.TrimPrefix(string(cond.ExpectedRevision), "native-etag:"), `"`)
		if etag == "" || strings.ContainsAny(etag, "\r\n\x00\"") {
			return out, storage.NewError("unsupported", storage.Path(key), nil)
		}
		headers.Set("If-Match", `"`+etag+`"`)
	}
	duration := time.Until(expires)
	if duration <= 0 || duration > time.Hour {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	signed, err := b.client.Object.GetPresignedURL2(ctx, http.MethodPut, key, duration, &sdk.PresignedURLOptions{Header: &headers}, true)
	if err != nil {
		return out, err
	}
	bound := make(map[string]string, len(headers))
	for name, values := range headers {
		if len(values) > 0 {
			bound[name] = values[0]
		}
	}
	return storage.SignedPartGrant{Number: 1, Size: size, Method: http.MethodPut, URL: signed.String(), Headers: bound, ExpiresAt: expires.UTC()}, nil
}

func (b *mutationBackend) ListParts(ctx context.Context, key, id string, after, limit int) ([]storage.NativeUploadPart, bool, error) {
	if limit < 1 {
		limit = 1000
	}
	if limit > 1000 {
		limit = 1000
	}
	marker := ""
	if after > 0 {
		marker = strconv.Itoa(after)
	}
	ctx, cancel := metadataDeadline(ctx)
	defer cancel()
	result, response, err := b.client.Object.ListParts(ctx, key, id, &sdk.ObjectListPartsOptions{MaxParts: strconv.Itoa(limit), PartNumberMarker: marker})
	if err != nil {
		return nil, false, mutationReadError(ctx, key, err, false)
	}
	if result == nil || response == nil || response.StatusCode != http.StatusOK {
		return nil, false, storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	parts := make([]storage.NativeUploadPart, 0, len(result.Parts))
	for _, part := range result.Parts {
		parts = append(parts, storage.NativeUploadPart{Number: part.PartNumber, Size: part.Size, ETag: part.ETag})
	}
	return parts, result.IsTruncated, nil
}

func (b *mutationBackend) ReadSigningSupported() bool { return b.client != nil }

func (b *mutationBackend) PresignRead(ctx context.Context, key string, options storage.ReadGrantOptions) (storage.ReadGrant, error) {
	var out storage.ReadGrant
	if b.client == nil {
		return out, storage.NewError("unsupported", storage.Path(key), nil)
	}
	expires := options.ExpiresAt
	if expires.IsZero() {
		expires = time.Now().UTC().Add(time.Hour)
	}
	duration := time.Until(expires)
	if duration <= 0 || duration > time.Hour {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	disposition := "attachment"
	if options.Disposition == "inline" {
		disposition = "inline"
	}
	mediaType := options.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	filename := path.Base(key)
	query := url.Values{}
	query.Set("response-content-type", mediaType)
	query.Set("response-content-disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(filename))
	signed, err := b.client.Object.GetPresignedURL3(ctx, http.MethodGet, key, duration, &sdk.PresignedURLOptions{Query: &query})
	if err != nil {
		return out, err
	}
	return storage.ReadGrant{Kind: "url", URL: signed.String(), ExpiresAt: expires.UTC()}, nil
}

var _ objectmutation.DirectPostBackend = (*mutationBackend)(nil)

// DirectPostSupported reports whether this connection can sign a form upload that refuses
// to create an existing key. The switch it depends on is the same one the admitted
// conditional PUT sends, so the connection's conditional-create evidence covers it.
func (b *mutationBackend) DirectPostSupported() bool { return b.client != nil }

// SignPost signs one form upload (POST Object) that creates key only if it is absent.
//
// A form upload cannot carry a conditional request header, so the condition lives inside
// the signed policy: the exact key, the exact length, the media type, and COS's
// x-cos-forbid-overwrite switch. A client that drops the switch answers a policy refusal
// ("Condition key x-cos-forbid-overwrite doesn't match the value true"), and a client that
// publishes to another key or another length answers a policy refusal too — the browser
// does not have to be trusted with any of it.
//
// The signature is the form-post one, which is not the URL signature: SignKey is
// HMAC-SHA1(secret, keyTime), StringToSign is SHA1 of the policy JSON, and the signature is
// HMAC-SHA1(SignKey, StringToSign), all lowercase hex. The bucket is HTTP/1.1 and cannot
// multiplex, so a request with no custom header — this one — is also the only upload the
// browser sends without a CORS preflight.
func (b *mutationBackend) SignPost(ctx context.Context, key string, size int64, mediaType string, expires time.Time) (storage.SignedPartGrant, error) {
	var out storage.SignedPartGrant
	if b.client == nil || b.secretID == "" || b.secretKey == "" || key == "" || size <= 0 {
		return out, storage.NewError("unsupported", storage.Path(key), nil)
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if strings.ContainsAny(key+mediaType, "\r\n\x00") || size > 5<<30 {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	duration := time.Until(expires)
	if duration <= 0 || duration > time.Hour {
		return out, storage.NewError("invalid_request", storage.Path(key), nil)
	}
	start := time.Now().Unix()
	end := expires.Unix()
	keyTime := strconv.FormatInt(start, 10) + ";" + strconv.FormatInt(end, 10)
	policy, err := json.Marshal(map[string]any{
		"expiration": expires.UTC().Format("2006-01-02T15:04:05.000Z"),
		"conditions": []any{
			map[string]string{"bucket": b.bucket},
			map[string]string{"q-sign-algorithm": "sha1"},
			map[string]string{"q-ak": b.secretID},
			map[string]string{"q-sign-time": keyTime},
			[]string{"eq", "$key", key},
			[]string{"eq", "$Content-Type", mediaType},
			[]any{"content-length-range", size, size},
			map[string]string{"x-cos-forbid-overwrite": "true"},
		},
	})
	if err != nil {
		return out, err
	}
	signKey := hmacSHA1Hex(b.secretKey, keyTime)
	policySum := sha1.Sum(policy)
	signature := hmacSHA1Hex(signKey, hex.EncodeToString(policySum[:]))
	return storage.SignedPartGrant{
		Number: 1, Size: size, Method: http.MethodPost,
		URL: "https://" + b.bucket + ".cos." + b.region + ".myqcloud.com/",
		Form: withSecurityToken([]storage.FormField{
			{Name: "key", Value: key},
			{Name: "policy", Value: base64.StdEncoding.EncodeToString(policy)},
			{Name: "q-sign-algorithm", Value: "sha1"},
			{Name: "q-ak", Value: b.secretID},
			{Name: "q-key-time", Value: keyTime},
			{Name: "q-signature", Value: signature},
			{Name: "x-cos-forbid-overwrite", Value: "true"},
			{Name: "Content-Type", Value: mediaType},
			{Name: "success_action_status", Value: "201"},
		}, b.token),
		FileField: "file",
		Headers:   map[string]string{},
		ExpiresAt: expires.UTC(),
	}, nil
}

// hmacSHA1Hex returns the lowercase hex digest of one HMAC-SHA1.
func hmacSHA1Hex(key, message string) string {
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// withSecurityToken appends the temporary-credential form field when the connection signs
// with one. A permanent key pair sends none.
func withSecurityToken(fields []storage.FormField, token string) []storage.FormField {
	if token == "" {
		return fields
	}
	return append(fields, storage.FormField{Name: "x-cos-security-token", Value: token})
}
