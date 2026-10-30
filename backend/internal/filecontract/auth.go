package filecontract

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type CredentialSource string

const (
	CookieCredential CredentialSource = "cookie"
	BearerCredential CredentialSource = "bearer"
)

// CredentialSelection describes selection only. Token still needs cryptographic
// verification, revocation/expiry checks and scope resolution by authentication.
type CredentialSelection struct {
	Source CredentialSource
	Token  string
}

// headerValues also handles noncanonical Header keys in adapters/tests and
// distinguishes absent from present-but-empty. Repeated credentials fail closed.
func headerValues(header http.Header, name string) ([]string, bool) {
	var values []string
	present := false
	for key, entries := range header {
		if strings.EqualFold(key, name) {
			present = true
			values = append(values, entries...)
		}
	}
	return values, present
}

func SelectCredential(header http.Header) (CredentialSelection, error) {
	values, present := headerValues(header, "Authorization")
	if !present {
		return CredentialSelection{Source: CookieCredential}, nil
	}
	selected := CredentialSelection{Source: BearerCredential}
	if len(values) != 1 {
		return selected, problem(http.StatusUnauthorized, "unauthenticated", "A single valid Bearer credential is required.")
	}
	value := strings.Trim(values[0], " \t")
	for _, char := range value {
		if char <= 0x20 && char != ' ' && char != '\t' || char > 0x7e {
			return selected, problem(http.StatusUnauthorized, "unauthenticated", "A valid supported Bearer credential is required.")
		}
	}
	fields := strings.Fields(value)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || !validBearerToken(fields[1]) {
		return selected, problem(http.StatusUnauthorized, "unauthenticated", "A valid supported Bearer credential is required.")
	}
	selected.Token = fields[1]
	return selected, nil
}

// RFC 6750 b64token syntax. Valid syntax is not proof of a valid credential.
func validBearerToken(token string) bool {
	if token == "" {
		return false
	}
	padding := false
	for _, char := range token {
		if char == '=' {
			padding = true
			continue
		}
		if padding || !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-._~+/", char)) {
			return false
		}
	}
	return token[0] != '='
}

// CookieMutationPolicy is constructed from trusted deployment configuration.
// Proxies may forward the original Origin unchanged; they cannot infer or expand
// the allowed origin from client-controlled forwarding/request-host headers.
type CookieMutationPolicy struct{ origin string }

func NewCookieMutationPolicy(canonicalOrigin string) (CookieMutationPolicy, error) {
	origin, ok := canonicalOriginValue(canonicalOrigin)
	if !ok {
		return CookieMutationPolicy{}, problem(http.StatusBadRequest, "invalid_request", "The configured canonical origin must be an HTTP or HTTPS origin without a path.")
	}
	return CookieMutationPolicy{origin: origin}, nil
}

// Check requires a caller-supplied, session-bound token verifier on every cookie
// mutation, independent of body type. The token is passed byte-for-byte without
// decoding or trimming. Missing Origin has no Referer or forwarded-host fallback.
// Call only after successful credential verification. Bearer requests are exempt
// from cookie CSRF checks because ambient cookies did not select their principal.
func (p CookieMutationPolicy) Check(method string, header http.Header, source CredentialSource, validateToken func(string) bool) error {
	if source != CookieCredential && source != BearerCredential {
		return problem(http.StatusForbidden, "forbidden", "The credential source is invalid.")
	}
	if source == BearerCredential || safeMethod(method) {
		return nil
	}
	origins, _ := headerValues(header, "Origin")
	if p.origin == "" || len(origins) != 1 {
		return problem(http.StatusForbidden, "forbidden", "An approved Origin is required for cookie mutations.")
	}
	origin, ok := canonicalOriginValue(origins[0])
	if !ok || origin != p.origin {
		return problem(http.StatusForbidden, "forbidden", "The Origin is not approved for cookie mutations.")
	}
	tokens, _ := headerValues(header, "X-CSRF-Token")
	if len(tokens) != 1 || tokens[0] == "" || validateToken == nil || !validateToken(tokens[0]) {
		return problem(http.StatusForbidden, "forbidden", "A valid session-bound CSRF token is required.")
	}
	return nil
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions || method == http.MethodTrace
}

func canonicalOriginValue(value string) (string, bool) {
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, " \t\r\n\\%") || strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	port := u.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", false
		}
		port = strconv.Itoa(number)
	}
	if port == "80" && scheme == "http" || port == "443" && scheme == "https" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, true
}
