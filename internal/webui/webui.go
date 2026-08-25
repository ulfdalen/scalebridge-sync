// Package webui serves the embedded static UI and the JSON API in API.md.
// It never binds a socket: main owns the listener and calls Handler(), and all
// store access goes through syncer.Do. Wizard state (OAuth nonces, pending MFA
// clients, backfill choice) is in-memory only — it dies with the process.
package webui

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/syncer"
)

//go:embed all:static
var staticFiles embed.FS

const (
	// The callback URL users register at Withings contains this port, so it is
	// effectively frozen per installation.
	defaultPort = 8723

	// How long a half-finished wizard step stays resumable. Both handles are
	// single-use and held only in memory.
	oauthStateTTL = 10 * time.Minute
	mfaTTL        = 10 * time.Minute

	// Keeps the opt-in GitHub call to at most one a day.
	updateCacheTTL = 24 * time.Hour
)

type Server struct {
	st      *store.Store
	sy      *syncer.Syncer
	version string

	// Captured once at New: a port change only takes effect on restart, and the
	// Host allowlist and callback URL must agree on the port we are bound to.
	port int

	mu              sync.Mutex
	oauthStates     map[string]time.Time
	mfa             map[string]*pendingMFA
	lastOAuthError  string
	lastOAuthDetail string
	backfillChoice  string // "" until the wizard posts one
	update          updateInfo
	updateAt        time.Time
}

func New(st *store.Store, sy *syncer.Syncer, version string) *Server {
	s := &Server{
		st:          st,
		sy:          sy,
		version:     version,
		port:        defaultPort,
		oauthStates: make(map[string]time.Time),
		mfa:         make(map[string]*pendingMFA),
	}
	_ = sy.Do(func(st *store.Store) error {
		if p := st.State.Settings.Port; p > 0 {
			s.port = p
		}
		return nil
	})
	return s
}

// Handler returns the full mux with the middleware chain already applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleIndexPage)
	mux.HandleFunc("GET /setup", s.handleSetupPage)
	mux.HandleFunc("GET /settings", s.handleSettingsPage)
	mux.HandleFunc("GET /callback", s.handleCallback)
	mux.Handle("GET /static/", staticHandler())

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/measurements", s.handleMeasurements)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/sync/now", s.handleSyncNow)
	mux.HandleFunc("POST /api/sync/backfill", s.handleSyncBackfill)

	mux.HandleFunc("GET /api/setup/state", s.handleSetupState)
	mux.HandleFunc("PUT /api/setup/withings-credentials", s.handleWithingsCredentials)
	mux.HandleFunc("POST /api/setup/backfill", s.handleSetupBackfill)
	mux.HandleFunc("POST /api/setup/complete", s.handleSetupComplete)

	mux.HandleFunc("GET /api/withings/connect", s.handleWithingsConnect)
	mux.HandleFunc("POST /api/withings/disconnect", s.handleWithingsDisconnect)

	mux.HandleFunc("POST /api/garmin/login", s.handleGarminLogin)
	mux.HandleFunc("POST /api/garmin/verify-mfa", s.handleGarminVerifyMFA)
	mux.HandleFunc("POST /api/garmin/disconnect", s.handleGarminDisconnect)

	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	mux.HandleFunc("GET /api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("POST /api/update/check", s.handleUpdateCheck)

	return s.hostAllowlist(securityHeaders(s.csrf(mux)))
}

// ── pages ─────────────────────────────────────────────

func staticHandler() http.Handler {
	fileServer := http.FileServerFS(staticFiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no-cache, not no-store: the browser may keep the file but must
		// revalidate, so an upgrade never serves a stale HTML/JS pair.
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndexPage(w http.ResponseWriter, r *http.Request) {
	// "GET /" is the catch-all pattern, so anything unrouted lands here.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.setupComplete() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	s.servePage(w, r, "index.html")
}

func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if s.setupComplete() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.servePage(w, r, "setup.html")
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, "settings.html")
}

func (s *Server) servePage(w http.ResponseWriter, r *http.Request, name string) {
	data, err := staticFiles.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func (s *Server) setupComplete() bool {
	complete := false
	_ = s.sy.Do(func(st *store.Store) error {
		complete = st.State.SetupComplete
		return nil
	})
	return complete
}

// ── the OAuth result page ─────────────────────────────

// Script-free by necessity: the CSP (default-src 'self') applies here too, and
// it rules out an inline <style> as well — hence the stylesheet link.
var callbackPage = template.Must(template.New("callback").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} — ScaleBridge Sync</title>
<link rel="icon" href="/static/img/favicon.svg">
<link rel="stylesheet" href="/static/app.css">
</head>
<body>
<main class="page page-narrow">
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
<p>You can close this tab and go back to the setup window.</p>
<p><a href="/setup">Back to setup</a></p>
</main>
</body>
</html>
`))

func renderCallback(w http.ResponseWriter, code int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = callbackPage.Execute(w, struct{ Title, Message string }{title, message})
}

// ── single-use nonces ─────────────────────────────────

// newNonce returns 32 bytes of randomness as base64url. The OAuth state and the
// MFA handle are unguessable capabilities: never weaken this past crypto/rand.
func newNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
