package api

import (
	"net/http"
	"strings"

	fc "everythingabout.me/backend/internal/filecontract"
)

const filesAllowedHeaders = "Authorization, Content-Type, Range, If-Match, If-None-Match, If-Range, If-Revision, Content-Digest, Idempotency-Key, Prefer, X-Source-Modified-At, X-CSRF-Token"
const filesExposedHeaders = "ETag, Accept-Ranges, Content-Range, Content-Length, Content-Disposition, Location, Retry-After, X-Request-Id"

func (s *Server) cookieOriginAllowed(origin string) bool {
	// Reuse the strict canonical origin comparison without trusting Host/Forwarded.
	for _, allowed := range s.cfg.FileCookieOrigins {
		policy, err := fc.NewCookieMutationPolicy(allowed)
		if err == nil && policy.Check("POST", http.Header{"Origin": {origin}, "X-Csrf-Token": {"fixture"}}, fc.CookieCredential, func(string) bool { return true }) == nil {
			return true
		}
	}
	return false
}
func (s *Server) filesCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		bearer := false
		for key := range r.Header {
			if strings.EqualFold(key, "Authorization") {
				bearer = true
			}
		}
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if preflight {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			for _, key := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
				if strings.EqualFold(strings.TrimSpace(key), "Authorization") {
					bearer = true
				}
			}
		}
		if origin != "" {
			if bearer {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if s.cookieOriginAllowed(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Access-Control-Expose-Headers", filesExposedHeaders)
		}
		if preflight {
			if origin == "" || w.Header().Get("Access-Control-Allow-Origin") == "" {
				writeFilesProblem(w, r, fc.NewProblem(403, "forbidden", "The cross-origin request is not approved."))
				return
			}
			method := r.Header.Get("Access-Control-Request-Method")
			if !strings.Contains(",GET,HEAD,PUT,POST,DELETE,", ","+method+",") {
				writeFilesProblem(w, r, fc.NewProblem(405, "method_not_allowed", "The preflight method is not supported."))
				return
			}
			allowed := map[string]bool{}
			for _, key := range strings.Split(filesAllowedHeaders, ",") {
				allowed[strings.ToLower(strings.TrimSpace(key))] = true
			}
			for _, key := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
				key = strings.ToLower(strings.TrimSpace(key))
				if key != "" && !allowed[key] {
					writeFilesProblem(w, r, fc.NewProblem(403, "forbidden", "A requested header is not supported."))
					return
				}
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", filesAllowedHeaders)
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}
