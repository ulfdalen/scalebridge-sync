package webui

// The security model of the local server. There is no user authentication: a
// local process could read the state file anyway. What these layers defend
// against is a web page the user has open reaching the API behind their back.

import (
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Nothing the API accepts is larger than a few hundred bytes.
const maxBody = 1 << 20

// A header no cross-origin form or <img> can set, so its presence forces a
// preflight that we never answer.
const csrfHeader = "X-ScaleBridge-Local"

// The DNS-rebinding defense: an attacker's domain resolving to 127.0.0.1 still
// arrives with its own Host header.
func (s *Server) hostAllowlist(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			writeErr(w, http.StatusForbidden, "forbidden_host", "unexpected Host header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowedHost(host string) bool {
	if host == "" {
		return false
	}
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		// No port at all: only meaningful if we are serving on 80.
		name, port = host, "80"
	}
	if p, err := strconv.Atoi(port); err != nil || p != s.port {
		return false
	}
	switch strings.ToLower(strings.Trim(name, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// No inline scripts anywhere, including the /callback page.
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// Three independent checks on every state-changing request: a custom header
// (unsettable cross-origin without a preflight), a JSON content type (which
// rules out the three encodings a <form> can send), and a matching Origin.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(csrfHeader) != "1" {
			writeErr(w, http.StatusForbidden, "csrf", "missing "+csrfHeader+" header")
			return
		}
		// ContentLength -1 means chunked/unknown, which still counts as a body.
		if r.ContentLength != 0 && !isJSON(r.Header.Get("Content-Type")) {
			writeErr(w, http.StatusForbidden, "csrf", "body must be application/json")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.allowedOrigin(origin) {
			writeErr(w, http.StatusForbidden, "csrf", "cross-origin request")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	return s.allowedHost(u.Host)
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}

// ── JSON helpers ──────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeErr(w http.ResponseWriter, code int, errCode, detail string) {
	writeJSON(w, code, apiError{Error: errCode, Detail: detail})
}

func writeOK(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// An empty body is fine: every endpoint here has an all-optional request shape.
func readJSON(r *http.Request, v any) error {
	body := http.MaxBytesReader(nil, r.Body, maxBody)
	err := json.NewDecoder(body).Decode(v)
	if err == io.EOF {
		return nil
	}
	return err
}
