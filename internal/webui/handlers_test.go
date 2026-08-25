package webui

// The two connection flows end to end, against httptest stand-ins for Withings'
// token endpoint and Garmin's SSO + DI endpoints. The real client packages are
// in the loop, so a wire-level mistake (a lost cookie jar, the wrong DI client
// id) fails here rather than in production.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/garmin"
	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/withings"
)

// ── Withings ──────────────────────────────────────────

const (
	withingsOK   = `{"status":0,"body":{"userid":"42","access_token":"w-access","refresh_token":"w-refresh","expires_in":10800}}`
	withingsFail = `{"status":503,"error":"Invalid Params: invalid client_secret"}`
)

// Points the token endpoint at a server that always answers with envelope.
func startFakeWithings(t *testing.T, envelope string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, envelope)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withings.SetEndpointsForTest(srv.URL, srv.URL))
}

// Drives GET /api/withings/connect and returns the minted state nonce.
func (h *harness) connect() string {
	h.t.Helper()
	rec := h.call(http.MethodGet, "/api/withings/connect", nil)
	if rec.Code != http.StatusFound {
		h.t.Fatalf("connect: status %d, want 302 (%s)", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		h.t.Fatalf("connect: bad Location %q: %v", rec.Header().Get("Location"), err)
	}
	return u.Query().Get("state")
}

func (h *harness) oauthError() (string, string) {
	h.t.Helper()
	out := h.callWant(http.MethodGet, "/api/setup/state", nil, http.StatusOK)
	errCode, _ := out["last_oauth_error"].(string)
	detail, _ := out["last_oauth_detail"].(string)
	return errCode, detail
}

func TestWithingsConnect(t *testing.T) {
	h := newHarness(t)

	rec := h.call(http.MethodGet, "/api/withings/connect", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no_credentials") {
		t.Fatalf("connect without creds: %d %s", rec.Code, rec.Body.String())
	}

	h.setCreds("client-id-1", "secret-1")
	rec = h.call(http.MethodGet, "/api/withings/connect", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("connect: status %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "authorize2") {
		t.Fatalf("Location is not the authorize2 URL: %s", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != "client-id-1" {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if len(q.Get("state")) < 32 {
		t.Fatalf("state nonce looks too short: %q", q.Get("state"))
	}
	if want := fmt.Sprintf("http://localhost:%d/callback", testPort); q.Get("redirect_uri") != want {
		t.Fatalf("redirect_uri = %q, want %q", q.Get("redirect_uri"), want)
	}
	// Every call is a fresh single-use nonce.
	if h.connect() == q.Get("state") {
		t.Fatal("state nonce was reused")
	}
}

func TestCallbackHappyPathAndReplay(t *testing.T) {
	h := newHarness(t)
	startFakeWithings(t, withingsOK)
	h.setCreds("client-id-1", "secret-1")

	state := h.connect()
	rec := h.call(http.MethodGet, "/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Withings connected") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("callback Content-Type = %q", ct)
	}
	// The CSP applies to this page too, so it must not carry an inline script.
	if strings.Contains(rec.Body.String(), "<script") {
		t.Fatal("callback page has an inline script; the CSP would block it")
	}

	h.sy.Do(func(st *store.Store) error {
		w := st.State.Withings
		if w.AccessToken != "w-access" || w.RefreshToken != "w-refresh" || w.UserID != "42" {
			t.Fatalf("tokens not stored: %+v", w)
		}
		if w.ReconnectRequired {
			t.Fatal("reconnect_required should have been cleared")
		}
		if w.ExpiresAt.IsZero() {
			t.Fatal("expires_at not set")
		}
		return nil
	})
	if code, _ := h.oauthError(); code != "" {
		t.Fatalf("last_oauth_error = %q, want empty", code)
	}
	if steps := h.setupSteps(); steps["withings_oauth"] != true {
		t.Fatalf("withings_oauth step not derived from the stored token: %v", steps)
	}

	// Replaying the same state must not work: the nonce is single-use.
	rec = h.call(http.MethodGet, "/callback?code=auth-code&state="+url.QueryEscape(state), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed state: status %d, want 400", rec.Code)
	}
	if code, _ := h.oauthError(); code != "state_mismatch" {
		t.Fatalf("last_oauth_error = %q, want state_mismatch", code)
	}
}

func TestCallbackFailures(t *testing.T) {
	h := newHarness(t)
	h.setCreds("client-id-1", "secret-1")

	t.Run("no state at all", func(t *testing.T) {
		h.call(http.MethodGet, "/callback?code=x", nil)
		if code, _ := h.oauthError(); code != "state_mismatch" {
			t.Fatalf("last_oauth_error = %q, want state_mismatch", code)
		}
	})

	t.Run("user denied", func(t *testing.T) {
		state := h.connect()
		rec := h.call(http.MethodGet, "/callback?error=access_denied&state="+url.QueryEscape(state), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("denied callback: status %d", rec.Code)
		}
		if code, _ := h.oauthError(); code != "denied" {
			t.Fatalf("last_oauth_error = %q, want denied", code)
		}
	})

	t.Run("exchange failed", func(t *testing.T) {
		startFakeWithings(t, withingsFail)
		state := h.connect()
		rec := h.call(http.MethodGet, "/callback?code=auth-code&state="+url.QueryEscape(state), nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("failed exchange: status %d", rec.Code)
		}
		code, detail := h.oauthError()
		if code != "exchange_failed" {
			t.Fatalf("last_oauth_error = %q, want exchange_failed", code)
		}
		if !strings.Contains(detail, "503") {
			t.Fatalf("detail should carry the Withings status text, got %q", detail)
		}
		h.sy.Do(func(st *store.Store) error {
			if st.State.Withings.AccessToken != "" {
				t.Fatal("a failed exchange must not store tokens")
			}
			return nil
		})
	})

	// The wizard only reports last_oauth_error when it changes, so starting a
	// new attempt has to reset it.
	t.Run("connect clears the previous error", func(t *testing.T) {
		if code, _ := h.oauthError(); code == "" {
			t.Fatal("precondition: expected a recorded error")
		}
		h.connect()
		code, detail := h.oauthError()
		if code != "" || detail != "" {
			t.Fatalf("connect left last_oauth_error = %q / %q", code, detail)
		}
	})
}

// ── Garmin ────────────────────────────────────────────

const (
	garminPassword = "correct-horse-battery"
	garminMFAEmail = "mfa@example.com"
	// The fake accepts only the second id in the fallback chain, so the test
	// proves we persist the one that actually worked.
	garminWantDI = "GARMIN_CONNECT_MOBILE_ANDROID_DI_2024Q4"
)

type garminFake struct {
	mu           sync.Mutex
	usedTicket   string
	sawMFACookie bool
}

func startFakeGarmin(t *testing.T) *garminFake {
	t.Helper()
	f := &garminFake{}
	mux := http.NewServeMux()

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Username, Password string }
		json.NewDecoder(r.Body).Decode(&body)
		if body.Password != garminPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Login seeds the cookie that verify must send back.
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "session-1", Path: "/"})
		if body.Username == garminMFAEmail {
			io.WriteString(w, `{"responseStatus":{"type":"MFA_REQUIRED"},"customerMfaInfo":{"mfaLastMethodUsed":"sms"}}`)
			return
		}
		io.WriteString(w, `{"serviceTicketId":"ST-direct"}`)
	})

	mux.HandleFunc("/mfa", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("JSESSIONID")
		f.mu.Lock()
		f.sawMFACookie = err == nil && c.Value == "session-1"
		f.mu.Unlock()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"no session"}`)
			return
		}
		var body struct {
			Code string `json:"mfaVerificationCode"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Code != "123456" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"serviceTicketId":"ST-mfa"}`)
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("client_id") != garminWantDI {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_client"}`)
			return
		}
		f.mu.Lock()
		f.usedTicket = r.FormValue("service_ticket")
		f.mu.Unlock()
		io.WriteString(w, `{"access_token":"g-access","refresh_token":"g-refresh","expires_in":3600,"token_type":"Bearer"}`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(garmin.SetSSOEndpointsForTest(srv.URL+"/login", srv.URL+"/mfa"))
	t.Cleanup(garmin.SetEndpointsForTest(srv.URL+"/token", srv.URL+"/upload"))
	return f
}

func (f *garminFake) ticket() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usedTicket
}

func (h *harness) assertGarminStored(email, ticket string) {
	h.t.Helper()
	h.sy.Do(func(st *store.Store) error {
		g := st.State.Garmin
		if g.Email != email || g.AccessToken != "g-access" || g.RefreshToken != "g-refresh" {
			h.t.Fatalf("garmin not persisted: %+v", g)
		}
		if g.DIClientID != garminWantDI {
			h.t.Fatalf("DIClientID = %q, want %q", g.DIClientID, garminWantDI)
		}
		if g.ReconnectRequired || g.ExpiresAt.IsZero() {
			h.t.Fatalf("garmin state wrong: %+v", g)
		}
		return nil
	})
	if steps := h.setupSteps(); steps["garmin"] != true {
		h.t.Fatalf("garmin step not derived from the stored token: %v", steps)
	}
}

func TestGarminLoginDirect(t *testing.T) {
	h := newHarness(t)
	f := startFakeGarmin(t)

	got := h.callWant(http.MethodPost, "/api/garmin/login",
		map[string]string{"email": "user@example.com", "password": garminPassword}, http.StatusOK)
	if got["ok"] != true {
		t.Fatalf("login: %v", got)
	}
	h.assertGarminStored("user@example.com", "ST-direct")
	if f.ticket() != "ST-direct" {
		t.Fatalf("exchanged ticket = %q", f.ticket())
	}

	// The password must not reach the event log or the state file.
	for _, e := range h.sy.Events(0) {
		if strings.Contains(e.Message, garminPassword) {
			t.Fatalf("password leaked into an event: %q", e.Message)
		}
	}
	var path string
	h.sy.Do(func(st *store.Store) error { path = st.FilePath(); return nil })
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), garminPassword) {
		t.Fatal("password leaked into the state file")
	}
}

func TestGarminLoginBadPassword(t *testing.T) {
	h := newHarness(t)
	startFakeGarmin(t)

	rec := h.call(http.MethodPost, "/api/garmin/login",
		map[string]string{"email": "user@example.com", "password": "wrong"})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid_credentials") {
		t.Fatalf("bad password: %d %s", rec.Code, rec.Body.String())
	}
	// The rejection must not echo anything back that looks like the attempt.
	if strings.Contains(rec.Body.String(), "wrong") {
		t.Fatalf("response echoed the password: %s", rec.Body.String())
	}
}

func TestGarminMFAFlow(t *testing.T) {
	h := newHarness(t)
	f := startFakeGarmin(t)

	got := h.callWant(http.MethodPost, "/api/garmin/login",
		map[string]string{"email": garminMFAEmail, "password": garminPassword}, http.StatusOK)
	if got["mfa_required"] != true || got["mfa_method"] != "sms" {
		t.Fatalf("expected an MFA challenge, got %v", got)
	}
	token, _ := got["mfa_token"].(string)
	if len(token) < 32 {
		t.Fatalf("mfa_token looks too short: %q", token)
	}
	h.sy.Do(func(st *store.Store) error {
		if st.State.Garmin.AccessToken != "" {
			t.Fatal("nothing may be stored before the code is verified")
		}
		return nil
	})

	// A wrong code is retryable: the parked client stays.
	rec := h.call(http.MethodPost, "/api/garmin/verify-mfa",
		map[string]string{"mfa_token": token, "code": "000000"})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid_mfa_code") {
		t.Fatalf("wrong code: %d %s", rec.Code, rec.Body.String())
	}

	h.callWant(http.MethodPost, "/api/garmin/verify-mfa",
		map[string]string{"mfa_token": token, "code": "123456"}, http.StatusOK)

	f.mu.Lock()
	sawCookie := f.sawMFACookie
	f.mu.Unlock()
	if !sawCookie {
		t.Fatal("verify did not reuse the login cookie jar")
	}
	if f.ticket() != "ST-mfa" {
		t.Fatalf("exchanged ticket = %q, want ST-mfa", f.ticket())
	}
	h.assertGarminStored(garminMFAEmail, "ST-mfa")

	// The handle is burned once it has been redeemed.
	rec = h.call(http.MethodPost, "/api/garmin/verify-mfa",
		map[string]string{"mfa_token": token, "code": "123456"})
	if rec.Code != http.StatusGone {
		t.Fatalf("reused mfa_token: status %d, want 410", rec.Code)
	}
}

func TestGarminMFAExpired(t *testing.T) {
	h := newHarness(t)
	startFakeGarmin(t)

	rec := h.call(http.MethodPost, "/api/garmin/verify-mfa",
		map[string]string{"mfa_token": "never-existed", "code": "123456"})
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "mfa_expired") {
		t.Fatalf("unknown token: %d %s", rec.Code, rec.Body.String())
	}

	// A handle past its TTL is indistinguishable from an unknown one.
	h.s.mu.Lock()
	h.s.mfa["stale"] = &pendingMFA{expires: time.Now().Add(-time.Minute)}
	h.s.mu.Unlock()
	rec = h.call(http.MethodPost, "/api/garmin/verify-mfa",
		map[string]string{"mfa_token": "stale", "code": "123456"})
	if rec.Code != http.StatusGone {
		t.Fatalf("expired token: status %d, want 410", rec.Code)
	}
}

func TestGarminUnreachable(t *testing.T) {
	h := newHarness(t)
	// A server that is already closed: the connection is refused immediately.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	t.Cleanup(garmin.SetSSOEndpointsForTest(deadURL+"/login", deadURL+"/mfa"))

	rec := h.call(http.MethodPost, "/api/garmin/login",
		map[string]string{"email": "user@example.com", "password": garminPassword})
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "garmin_unreachable") {
		t.Fatalf("unreachable garmin: %d %s", rec.Code, rec.Body.String())
	}
}

// ── disconnects ───────────────────────────────────────

func TestDisconnects(t *testing.T) {
	h := newHarness(t)
	h.setCreds("client-id-1", "secret-1")
	h.state(func(st *store.State) {
		st.Withings.AccessToken = "at"
		st.Withings.RefreshToken = "rt"
		st.Withings.UserID = "42"
		st.Withings.ReconnectRequired = true
		st.Garmin = store.Garmin{
			Email: "user@example.com", AccessToken: "g-at", RefreshToken: "g-rt",
			DIClientID: garminWantDI, ReconnectRequired: true,
		}
	})

	h.callWant(http.MethodPost, "/api/withings/disconnect", map[string]any{}, http.StatusOK)
	h.callWant(http.MethodPost, "/api/garmin/disconnect", map[string]any{}, http.StatusOK)

	h.sy.Do(func(st *store.Store) error {
		w := st.State.Withings
		if w.AccessToken != "" || w.RefreshToken != "" || w.UserID != "" || w.ReconnectRequired {
			t.Fatalf("withings tokens not cleared: %+v", w)
		}
		// The developer app is the user's own: it survives a disconnect.
		if w.ClientID != "client-id-1" || w.ClientSecret != "secret-1" {
			t.Fatalf("withings credentials should survive a disconnect: %+v", w)
		}
		if (st.State.Garmin != store.Garmin{}) {
			t.Fatalf("garmin not fully cleared: %+v", st.State.Garmin)
		}
		return nil
	})
}
