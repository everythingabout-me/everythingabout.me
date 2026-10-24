// Package httpx 收敛 HTTP 层的公共细节：统一响应信封、错误类型、
// Origin 校验、客户端 IP 和 JSON 解码。
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"everythingabout.me/backend/internal/paths"
)

// Error 是会被原样映射到 HTTP 状态码的业务错误。
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func NewError(status int, message string) *Error {
	return &Error{Status: status, Message: message}
}

func BadRequest(message string) *Error   { return NewError(http.StatusBadRequest, message) }
func Unauthorized(message string) *Error { return NewError(http.StatusUnauthorized, message) }
func Forbidden(message string) *Error    { return NewError(http.StatusForbidden, message) }
func NotFound(message string) *Error     { return NewError(http.StatusNotFound, message) }
func Conflict(message string) *Error     { return NewError(http.StatusConflict, message) }
func TooManyRequests(m string) *Error    { return NewError(http.StatusTooManyRequests, m) }
func ServiceUnavailable(m string) *Error { return NewError(http.StatusServiceUnavailable, m) }
func BadGateway(message string) *Error   { return NewError(http.StatusBadGateway, message) }
func Internal(message string) *Error     { return NewError(http.StatusInternalServerError, message) }

type okEnvelope struct {
	OK   bool `json:"ok"`
	Data any  `json:"data"`
}

type errEnvelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// WriteJSON 直接写一个 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// WriteOK 写 {ok:true,data:...}，和改造前的接口形状保持一致。
func WriteOK(w http.ResponseWriter, data any) {
	WriteJSON(w, http.StatusOK, okEnvelope{OK: true, Data: data})
}

// WriteError 把 error 映射成 {ok:false,error:"..."}。
// 非业务错误统一 500，并把原始信息放进消息里，方便定位。
func WriteError(w http.ResponseWriter, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		WriteJSON(w, apiErr.Status, errEnvelope{OK: false, Error: apiErr.Message})
		return
	}
	var pathErr *paths.Error
	if errors.As(err, &pathErr) {
		WriteJSON(w, http.StatusBadRequest, errEnvelope{OK: false, Error: pathErr.Message})
		return
	}
	WriteJSON(w, http.StatusInternalServerError, errEnvelope{OK: false, Error: err.Error()})
}

// AssertSameOrigin 比较 Origin 和 Host。没有 Origin 的请求（curl、脚本）放行；
// SameSite=Lax 已经挡住绝大多数跨站 POST，这里再挡一层同站子域。
func AssertSameOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return Forbidden("请求来源不合法")
	}
	requestHost := forwardedHost(r)
	if requestHost != "" && !strings.EqualFold(parsed.Host, requestHost) {
		return Forbidden("跨站请求被拒绝")
	}
	return nil
}

func forwardedHost(r *http.Request) string {
	if value := r.Header.Get("X-Forwarded-Host"); value != "" {
		if first, _, _ := strings.Cut(value, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	return r.Host
}

// ReadJSON 解码请求体，限制 1MB，并拒绝 JSON 后面跟多余内容。
func ReadJSON[T any](r *http.Request) (T, error) {
	var value T
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(&value); err != nil {
		return value, BadRequest("请求体不是合法 JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return value, BadRequest("请求体不是合法 JSON")
	}
	return value, nil
}

// ClientIP 取反向代理传来的第一个地址。
func ClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, _ := strings.Cut(forwarded, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	host := r.RemoteAddr
	if index := strings.LastIndex(host, ":"); index > 0 {
		host = host[:index]
	}
	if host == "" {
		return "local"
	}
	return host
}
