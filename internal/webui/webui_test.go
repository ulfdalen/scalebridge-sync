package webui

// Harness plus the tests about the server itself: security middleware, page
// redirects, settings, status and the dashboard reads. The connection flows
// live in handlers_test.go. Nothing here calls t.Parallel — several tests
// repoint the package-level endpoint vars in internal/{withings,garmin}.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/syncer"
)

const testPort = defaultPort // what store.Open seeds, so what New captures

type harness struct {
	t  *testing.T
	s  *Server
	sy *syncer.Syncer
	h  http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	sy := syncer.New(st)
	s := New(st, sy, "1.2.3")
	return &harness{t: t, s: s, sy: sy, h: s.Handler()}
}

// Builds a request that passes the middleware: right Host, CSRF header on
// non-GET, JSON content type whenever there is a body.
func (h *harness) req(method, path string, body any) *http.Request {
	h.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		buf, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(buf))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Host = fmt.Sprintf("localhost:%d", testPort)
	if method != http.MethodGet {
		r.Header.Set(csrfHeader, "1")
	}
	return r
}

func (h *harness) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) call(method, path string, body any) *httptest.ResponseRecorder {
	return h.do(h.req(method, path, body))
}

// callWant fails unless the status matches, and decodes the JSON body.
func (h *harness) callWant(method, path string, body any, want int) map[string]any {
	h.t.Helper()
	rec := h.call(method, path, body)
	if rec.Code != want {
		h.t.Fatalf("%s %s: status %d, want %d (body %s)", method, path, rec.Code, want, rec.Body.String())
	}
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			h.t.Fatalf("%s %s: decode body %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return out
}

// state reads or mutates the store the way the handlers do — through the
// syncer's lock, so -race stays quiet next to a running tick.
func (h *harness) state(fn func(*store.State)) {
	h.t.Helper()
	if err := h.sy.Do(func(st *store.Store) error {
		fn(&st.State)
		return st.Save()
	}); err != nil {
		h.t.Fatalf("store: %v", err)
	}
}

func (h *harness) setCreds(id, secret string) {
	h.t.Helper()
	h.callWant(http.MethodPut, "/api/setup/withings-credentials",
		map[string]string{"client_id": id, "client_secret": secret}, http.StatusOK)
}

// ── Host allowlist ────────────────────────────────────

func TestHostAllowlist(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/", "/api/status"} {
		r := h.req(http.MethodGet, path, nil)
		r.Host = "evil.example.com"
		if rec := h.do(r); rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s with bogus Host: status %d, want 403", path, rec.Code)
		}
		// A right hostname on the wrong port is still a rebinding attempt.
		r = h.req(http.MethodGet, path, nil)
		r.Host = "localhost:9999"
		if rec := h.do(r); rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s with wrong port: status %d, want 403", path, rec.Code)
		}
	}

	for _, host := range []string{
		fmt.Sprintf("localhost:%d", testPort),
		fmt.Sprintf("127.0.0.1:%d", testPort),
		fmt.Sprintf("[::1]:%d", testPort),
	} {
		r := h.req(http.MethodGet, "/api/status", nil)
		r.Host = host
		if rec := h.do(r); rec.Code != http.StatusOK {
			t.Fatalf("GET /api/status with Host %q: status %d, want 200", host, rec.Code)
		}
	}
}

// ── CSRF ──────────────────────────────────────────────

func TestCSRF(t *testing.T) {
	h := newHarness(t)

	// No custom header.
	r := h.req(http.MethodPost, "/api/withings/disconnect", nil)
	r.Header.Del(csrfHeader)
	rec := h.do(r)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"csrf"`) {
		t.Fatalf("POST without CSRF header: %d %s", rec.Code, rec.Body.String())
	}

	// Header + JSON body passes.
	if rec := h.call(http.MethodPost, "/api/withings/disconnect", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("POST with CSRF header: %d %s", rec.Code, rec.Body.String())
	}

	// A body that is not JSON is rejected even with the header.
	r = httptest.NewRequest(http.MethodPost, "/api/withings/disconnect", strings.NewReader("a=1"))
	r.Host = fmt.Sprintf("localhost:%d", testPort)
	r.Header.Set(csrfHeader, "1")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := h.do(r); rec.Code != http.StatusForbidden {
		t.Fatalf("POST with form body: status %d, want 403", rec.Code)
	}

	// Foreign Origin.
	r = h.req(http.MethodPost, "/api/withings/disconnect", map[string]any{})
	r.Header.Set("Origin", "https://evil.com")
	if rec := h.do(r); rec.Code != http.StatusForbidden {
		t.Fatalf("POST from evil.com: status %d, want 403", rec.Code)
	}

	// Our own Origin is fine.
	r = h.req(http.MethodPost, "/api/withings/disconnect", map[string]any{})
	r.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", testPort))
	if rec := h.do(r); rec.Code != http.StatusOK {
		t.Fatalf("POST from own origin: status %d, want 200", rec.Code)
	}

	// GET is unaffected.
	r = h.req(http.MethodGet, "/api/status", nil)
	r.Header.Del(csrfHeader)
	if rec := h.do(r); rec.Code != http.StatusOK {
		t.Fatalf("GET without CSRF header: status %d, want 200", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	rec := h.call(http.MethodGet, "/api/status", nil)
	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// no-store is for the API only; assets may be cached but must revalidate.
	rec = h.call(http.MethodGet, "/static/ui.js", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("GET /static/ui.js: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("/static Cache-Control = %q, want no-cache", got)
	}
	if rec := h.call(http.MethodGet, "/static/nope.js", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset: status %d, want 404", rec.Code)
	}
}

// ── the client secret never leaves ────────────────────

func TestSetupStateNeverLeaksSecret(t *testing.T) {
	h := newHarness(t)
	const secret = "s3cr3t-withings-client-secret"
	h.setCreds("client-id-1", secret)

	rec := h.call(http.MethodGet, "/api/setup/state", nil)
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("/api/setup/state leaked the client secret: %s", body)
	}
	if !strings.Contains(body, `"client_secret_set":true`) {
		t.Fatalf("client_secret_set not reported: %s", body)
	}
	// The contract promises the key is always present.
	if !strings.Contains(body, `"last_oauth_detail"`) {
		t.Fatalf("last_oauth_detail missing: %s", body)
	}
	if !strings.Contains(body, fmt.Sprintf(`"callback_url":"http://localhost:%d/callback"`, testPort)) {
		t.Fatalf("callback_url wrong: %s", body)
	}

	// The status endpoint must not leak it either.
	if body := h.call(http.MethodGet, "/api/status", nil).Body.String(); strings.Contains(body, secret) {
		t.Fatalf("/api/status leaked the client secret: %s", body)
	}
}

func TestStatusReportsConfigPath(t *testing.T) {
	h := newHarness(t)
	var want string
	if err := h.sy.Do(func(st *store.Store) error { want = st.FilePath(); return nil }); err != nil {
		t.Fatal(err)
	}
	got := h.callWant(http.MethodGet, "/api/status", nil, http.StatusOK)
	if got["config_path"] != want {
		t.Fatalf("config_path = %v, want %q", got["config_path"], want)
	}
	if got["version"] != "1.2.3" {
		t.Fatalf("version = %v, want 1.2.3", got["version"])
	}
	sync := got["sync"].(map[string]any)
	if sync["last_at"] != nil || sync["next_at"] != nil {
		t.Fatalf("timestamps should be null before the first tick: %v", sync)
	}
}

// ── changing the client id drops the tokens ───────────

func TestCredentialsClearTokensOnClientIDChange(t *testing.T) {
	h := newHarness(t)
	h.setCreds("client-id-1", "secret-1")
	h.state(func(st *store.State) {
		st.Withings.AccessToken = "at"
		st.Withings.RefreshToken = "rt"
		st.Withings.UserID = "42"
	})

	// Same id, new secret (the user re-pasted it): tokens survive.
	h.setCreds("client-id-1", "secret-2")
	h.sy.Do(func(st *store.Store) error {
		if st.State.Withings.AccessToken != "at" || st.State.Withings.UserID != "42" {
			t.Fatalf("same client_id should keep tokens, got %+v", st.State.Withings)
		}
		if st.State.Withings.ClientSecret != "secret-2" {
			t.Fatalf("secret not updated: %q", st.State.Withings.ClientSecret)
		}
		return nil
	})

	// New id: the old tokens belong to a different developer app.
	h.setCreds("client-id-2", "secret-2")
	h.sy.Do(func(st *store.Store) error {
		w := st.State.Withings
		if w.AccessToken != "" || w.RefreshToken != "" || w.UserID != "" {
			t.Fatalf("client_id change should clear tokens, got %+v", w)
		}
		return nil
	})

	// A half-filled form is a 4xx, never a silent "keep the old one".
	if rec := h.call(http.MethodPut, "/api/setup/withings-credentials",
		map[string]string{"client_id": "client-id-2", "client_secret": ""}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty secret: status %d, want 400", rec.Code)
	}
}

// ── page redirects ────────────────────────────────────

func TestPageRedirects(t *testing.T) {
	h := newHarness(t)

	rec := h.call(http.MethodGet, "/", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("incomplete setup: GET / gave %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := h.call(http.MethodGet, "/setup", nil); rec.Code != http.StatusOK {
		t.Fatalf("incomplete setup: GET /setup gave %d", rec.Code)
	}

	h.state(func(st *store.State) { st.SetupComplete = true })

	rec = h.call(http.MethodGet, "/setup", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("complete setup: GET /setup gave %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := h.call(http.MethodGet, "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("complete setup: GET / gave %d", rec.Code)
	}
	if rec := h.call(http.MethodGet, "/settings", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /settings gave %d", rec.Code)
	}
	if rec := h.call(http.MethodGet, "/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope gave %d, want 404", rec.Code)
	}
}

// ── finishing the wizard ──────────────────────────────

func TestSetupCompleteNoBackfill(t *testing.T) {
	h := newHarness(t)
	h.callWant(http.MethodPost, "/api/setup/backfill", map[string]string{"choice": "none"}, http.StatusOK)

	if steps := h.setupSteps(); steps["backfill"] != true {
		t.Fatalf("backfill step not marked after the choice was posted: %v", steps)
	}

	h.callWant(http.MethodPost, "/api/setup/complete", map[string]int{"interval_minutes": 20}, http.StatusOK)

	h.sy.Do(func(st *store.Store) error {
		if !st.State.SetupComplete {
			t.Fatal("setup_complete not set")
		}
		if st.State.Settings.IntervalMinutes != 20 {
			t.Fatalf("interval = %d, want 20", st.State.Settings.IntervalMinutes)
		}
		// "none" means "sync from today", i.e. the cursor starts at now.
		if st.State.Withings.LastUpdate <= 0 {
			t.Fatalf("LastUpdate = %d, want > 0", st.State.Withings.LastUpdate)
		}
		return nil
	})
}

func TestSetupCompleteWithBackfill(t *testing.T) {
	h := newHarness(t)
	h.callWant(http.MethodPost, "/api/setup/backfill", map[string]string{"choice": "30d"}, http.StatusOK)
	h.callWant(http.MethodPost, "/api/setup/complete", map[string]int{"interval_minutes": 15}, http.StatusOK)

	// A backfill owns the cursor: setup/complete must not move it.
	h.sy.Do(func(st *store.Store) error {
		if st.State.Withings.LastUpdate != 0 {
			t.Fatalf("LastUpdate = %d, want 0 (backfill decides)", st.State.Withings.LastUpdate)
		}
		return nil
	})

	// Withings is not connected, so the backfill fails — which is exactly the
	// proof that it was started.
	if !h.waitForEvent("Backfill") {
		t.Fatalf("no backfill event; events: %v", h.sy.Events(0))
	}
}

func TestSetupBackfillRejectsUnknownChoice(t *testing.T) {
	h := newHarness(t)
	h.callWant(http.MethodPost, "/api/setup/backfill", map[string]string{"choice": "forever"}, http.StatusBadRequest)
	if steps := h.setupSteps(); steps["backfill"] != false {
		t.Fatalf("rejected choice must not mark the step: %v", steps)
	}
}

func (h *harness) setupSteps() map[string]any {
	h.t.Helper()
	out := h.callWant(http.MethodGet, "/api/setup/state", nil, http.StatusOK)
	return out["steps"].(map[string]any)
}

func (h *harness) waitForEvent(substr string) bool {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.sy.Events(0) {
			if strings.Contains(e.Message, substr) {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// ── settings ──────────────────────────────────────────

func TestSettings(t *testing.T) {
	h := newHarness(t)

	got := h.callWant(http.MethodGet, "/api/settings", nil, http.StatusOK)
	if got["interval_minutes"] != float64(15) || got["port"] != float64(defaultPort) || got["update_check"] != false {
		t.Fatalf("defaults wrong: %v", got)
	}

	// Interval only: applied, no restart.
	got = h.callWant(http.MethodPut, "/api/settings", map[string]int{"interval_minutes": 30}, http.StatusOK)
	if got["restart_required"] != false {
		t.Fatalf("restart_required = %v, want false", got["restart_required"])
	}
	got = h.callWant(http.MethodGet, "/api/settings", nil, http.StatusOK)
	if got["interval_minutes"] != float64(30) {
		t.Fatalf("interval not applied: %v", got)
	}

	// Port change: stored, but only live after a restart.
	got = h.callWant(http.MethodPut, "/api/settings", map[string]int{"port": 9100}, http.StatusOK)
	if got["restart_required"] != true {
		t.Fatalf("restart_required = %v, want true", got["restart_required"])
	}
	got = h.callWant(http.MethodGet, "/api/settings", nil, http.StatusOK)
	if got["port"] != float64(9100) || got["interval_minutes"] != float64(30) {
		t.Fatalf("partial update clobbered a field: %v", got)
	}
	// The bound port is what the allowlist and the callback URL still use.
	if h.s.port != defaultPort {
		t.Fatalf("bound port changed to %d before a restart", h.s.port)
	}

	// Out-of-range values are refused before they reach the store.
	h.callWant(http.MethodPut, "/api/settings", map[string]int{"interval_minutes": 1}, http.StatusBadRequest)
	h.callWant(http.MethodPut, "/api/settings", map[string]int{"interval_minutes": 5000}, http.StatusBadRequest)
	h.callWant(http.MethodPut, "/api/settings", map[string]int{"port": 80}, http.StatusBadRequest)
	h.callWant(http.MethodPut, "/api/settings", map[string]int{"port": 70000}, http.StatusBadRequest)

	got = h.callWant(http.MethodPut, "/api/settings", map[string]bool{"update_check": true}, http.StatusOK)
	if got["restart_required"] != false {
		t.Fatalf("update_check toggle should not need a restart: %v", got)
	}
}

// ── update check ──────────────────────────────────────

func TestUpdateCheck(t *testing.T) {
	h := newHarness(t)

	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		io.WriteString(w, `{"tag_name":"v2.0.0","html_url":"https://github.com/ulfdalen/scalebridge-sync/releases/tag/v2.0.0"}`)
	}))
	defer srv.Close()
	prev := releasesURL
	releasesURL = srv.URL
	defer func() { releasesURL = prev }()

	got := h.callWant(http.MethodPost, "/api/update/check", map[string]any{}, http.StatusOK)
	if got["newer"] != true || got["latest"] != "v2.0.0" || got["current"] != "1.2.3" {
		t.Fatalf("update check: %v", got)
	}

	// The dashboard only ever sees the cached answer, never a fresh call.
	status := h.callWant(http.MethodGet, "/api/status", nil, http.StatusOK)
	update := status["update"].(map[string]any)
	if update["newer"] != true || update["latest"] != "v2.0.0" {
		t.Fatalf("status.update not populated from the cache: %v", update)
	}
	if h.callWant(http.MethodGet, "/api/update/check", nil, http.StatusOK)["latest"] != "v2.0.0" {
		t.Fatal("GET should have been served from the cache")
	}
	mu.Lock()
	cached := calls
	mu.Unlock()
	if cached != 1 {
		t.Fatalf("github was called %d times, want 1 (GET must use the cache)", cached)
	}

	// "Check now" always refetches.
	h.callWant(http.MethodPost, "/api/update/check", map[string]any{}, http.StatusOK)
	mu.Lock()
	forced := calls
	mu.Unlock()
	if forced != 2 {
		t.Fatalf("POST did not force a refresh (calls=%d)", forced)
	}

	// A dev build is never behind.
	dev := newHarness(t)
	dev.s.version = "dev"
	if got := dev.callWant(http.MethodPost, "/api/update/check", map[string]any{}, http.StatusOK); got["newer"] != false {
		t.Fatalf("dev build reported as outdated: %v", got)
	}

	// An unreachable GitHub is a 502, and the cache is left alone.
	srv.Close()
	rec := h.call(http.MethodPost, "/api/update/check", map[string]any{})
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "github_unreachable") {
		t.Fatalf("unreachable github: %d %s", rec.Code, rec.Body.String())
	}
	status = h.callWant(http.MethodGet, "/api/status", nil, http.StatusOK)
	if status["update"].(map[string]any)["latest"] != "v2.0.0" {
		t.Fatal("a failed check must not wipe the cached result")
	}
}

// ── measurements + events ─────────────────────────────

func TestMeasurementsNewestFirst(t *testing.T) {
	h := newHarness(t)
	base := time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC)
	fat := 18.25
	h.state(func(st *store.State) {
		for i := range 3 { // Recent is oldest-first on disk
			st.Recent = append(st.Recent, store.Measurement{
				GroupID:    int64(i + 1),
				MeasuredAt: base.AddDate(0, 0, i),
				WeightKG:   80 + float64(i),
				BodyFatPct: &fat,
				Synced:     i == 0,
			})
		}
	})

	out := h.callWant(http.MethodGet, "/api/measurements", nil, http.StatusOK)
	items := out["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	first := items[0].(map[string]any)
	if first["weight_kg"] != float64(82) {
		t.Fatalf("not newest first: %v", first)
	}
	// The contract promises the keys exist even when the value is null.
	for _, k := range []string{"muscle_kg", "bone_kg", "hydration_pct", "bmi"} {
		v, ok := first[k]
		if !ok || v != nil {
			t.Fatalf("%s = %v (present=%v), want present and null", k, v, ok)
		}
	}
	if first["body_fat_pct"] != fat {
		t.Fatalf("body_fat_pct = %v, want %v", first["body_fat_pct"], fat)
	}

	out = h.callWant(http.MethodGet, "/api/measurements?limit=2", nil, http.StatusOK)
	items = out["items"].([]any)
	if len(items) != 2 || items[1].(map[string]any)["weight_kg"] != float64(81) {
		t.Fatalf("limit=2 gave %v", items)
	}
}

func TestEvents(t *testing.T) {
	h := newHarness(t)
	out := h.callWant(http.MethodGet, "/api/events", nil, http.StatusOK)
	if items, ok := out["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("empty log should be [], got %v", out["items"])
	}

	for i := range 3 {
		h.sy.AddEvent("info", fmt.Sprintf("event %d", i))
	}
	out = h.callWant(http.MethodGet, "/api/events?limit=2", nil, http.StatusOK)
	items := out["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["message"] != "event 2" {
		t.Fatalf("events not newest-first/limited: %v", items)
	}
}

// ── one sync at a time ────────────────────────────────

func TestSyncNowConflict(t *testing.T) {
	h := newHarness(t)

	// Park a goroutine on the syncer's lock so the tick TriggerSync spawns
	// cannot finish; "running" then stays set for as long as we need it.
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.sy.Do(func(*store.Store) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	if !h.sy.TriggerSync() {
		t.Fatal("first TriggerSync refused")
	}
	rec := h.call(http.MethodPost, "/api/sync/now", map[string]any{})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already_running") {
		t.Fatalf("second sync: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.call(http.MethodPost, "/api/sync/backfill", map[string]string{"range": "30d"}); rec.Code != http.StatusConflict {
		t.Fatalf("backfill while running: %d %s", rec.Code, rec.Body.String())
	}

	close(release)
	<-done

	// An unknown range is a 400, not a 409 — the distinction the UI needs.
	h.callWant(http.MethodPost, "/api/sync/backfill", map[string]string{"range": "10y"}, http.StatusBadRequest)
}

// A hostile page can make the browser fire GET /api/withings/connect
// cross-site (an <img> is enough); Sec-Fetch-Site is the guard.
func TestWithingsConnectRefusesCrossSite(t *testing.T) {
	h := newHarness(t)
	h.setCreds(strings.Repeat("a", 64), strings.Repeat("b", 64))

	r := h.req(http.MethodGet, "/api/withings/connect", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := h.do(r); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site connect: status %d, want 403", rec.Code)
	}

	// Our own UI ("same-origin"), a typed URL ("none") and header-less
	// clients (curl) all pass.
	for _, site := range []string{"", "same-origin", "none"} {
		r := h.req(http.MethodGet, "/api/withings/connect", nil)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		if rec := h.do(r); rec.Code != http.StatusFound {
			t.Fatalf("Sec-Fetch-Site %q: status %d, want 302", site, rec.Code)
		}
	}
}

// With the daily check opted out, a GET must never reach the release API —
// only the POST behind the explicit "Check now" button may.
func TestUpdateCheckGetHonorsOptIn(t *testing.T) {
	h := newHarness(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"tag_name":"v9.9.9","html_url":"https://example.com/rel"}`)
	}))
	defer srv.Close()
	prev := releasesURL
	releasesURL = srv.URL
	defer func() { releasesURL = prev }()

	out := h.callWant(http.MethodGet, "/api/update/check", nil, http.StatusOK)
	if hits != 0 {
		t.Fatalf("GET with update_check off reached the release API %d time(s)", hits)
	}
	if out["newer"] != false {
		t.Errorf("cold cache, opted out: newer = %v, want false", out["newer"])
	}

	h.callWant(http.MethodPost, "/api/update/check", nil, http.StatusOK)
	if hits != 1 {
		t.Fatalf("POST: release API hits = %d, want 1", hits)
	}

	h.state(func(st *store.State) { st.Settings.UpdateCheck = true })
	out = h.callWant(http.MethodGet, "/api/update/check", nil, http.StatusOK)
	if hits != 1 {
		t.Fatalf("GET with warm cache: release API hits = %d, want still 1", hits)
	}
	if out["newer"] != true || out["latest"] != "v9.9.9" {
		t.Errorf("opted in, warm cache: got %v", out)
	}
}

// A repo with no releases returns 404 from the releases API — that is "nothing
// newer", not an outage the UI should warn about.
func TestUpdateCheckNoReleasesYet(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer srv.Close()
	prev := releasesURL
	releasesURL = srv.URL
	defer func() { releasesURL = prev }()

	out := h.callWant(http.MethodPost, "/api/update/check", nil, http.StatusOK)
	if out["newer"] != false || out["latest"] != "" {
		t.Fatalf("no releases: got %v", out)
	}
}

func TestRunningRelease(t *testing.T) {
	cases := []struct {
		tag, version string
		want         bool
	}{
		{"v0.0.1", "0.0.1", true},
		{"v0.0.1", "0.0.1-7-gabc123", true}, // post-tag git-describe build
		{"v0.0.1", "0.0.10", false},         // prefix must respect the boundary
		{"v0.0.2", "0.0.1", false},
	}
	for _, c := range cases {
		if got := runningRelease(c.tag, c.version); got != c.want {
			t.Errorf("runningRelease(%q, %q) = %v, want %v", c.tag, c.version, got, c.want)
		}
	}
}
