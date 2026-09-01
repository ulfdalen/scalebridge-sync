package syncer

// httptest stand-ins for Withings and Garmin, plus the harness that wires them
// to a real store in a temp dir. The tests drive the real client packages
// against these, so a missed persist-after-refresh, a reused single-use refresh
// token or a malformed FIT shows up as a test failure.

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/garmin"
	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/withings"
)

// ── fake Withings ────────────────────────────────────────

type fakeGroup struct {
	grp     withings.RawMeasureGroup
	updated int64 // server-side updatetime for this group
}

type fakeWithings struct {
	srv *httptest.Server
	t   *testing.T

	mu           sync.Mutex
	clientID     string
	clientSecret string
	access       string // currently valid access token
	refresh      string // currently valid refresh token
	rotations    int
	userID       string

	groups     []fakeGroup
	updateTime int64 // the cursor getmeas reports

	statuses    []int // scripted envelope statuses for getmeas, popped per call
	refreshDead bool  // refresh always answers invalid_grant

	measureCalls int
	tokenCalls   int
	lastForm     url.Values
	hold         chan struct{} // when set, getmeas blocks until closed

	// probe, when set, runs on every getmeas and its answer is recorded, so a
	// test can inspect the state file mid-tick.
	probe  func() string
	probes []string
}

func newFakeWithings(t *testing.T) *fakeWithings {
	f := &fakeWithings{
		t:            t,
		clientID:     "withings-client-id",
		clientSecret: "withings-client-secret",
		access:       "w-access-0",
		refresh:      "w-refresh-0",
		userID:       "42",
		updateTime:   2000,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/oauth2", f.handleToken)
	mux.HandleFunc("/measure", f.handleMeasure)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// add registers a weigh-in whose server-side updatetime is the current cursor,
// so the next getmeas with lastupdate == cursor no longer returns it.
func (f *fakeWithings) add(grpid int64, at time.Time, kg float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = append(f.groups, fakeGroup{
		grp: withings.RawMeasureGroup{
			GroupID:  grpid,
			Date:     at.Unix(),
			Category: 1,
			DeviceID: "fake-scale",
			Measures: []withings.RawMeasure{
				{Type: withings.TypeWeight, Value: int(kg*100 + 0.5), Unit: -2},
			},
		},
		updated: f.updateTime,
	})
}

// addFull is add() with every body-composition value Withings can report, in
// Withings wire units (value × 10^unit).
func (f *fakeWithings) addFull(grpid int64, at time.Time, kg, fatPct, muscleKG, boneKG, hydrationPct, bmi, visceralFat, bmrKcal, metabolicAgeYears float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hundredths := func(v float64) int { return int(v*100 + 0.5) }
	f.groups = append(f.groups, fakeGroup{
		grp: withings.RawMeasureGroup{
			GroupID:  grpid,
			Date:     at.Unix(),
			Category: 1,
			DeviceID: "fake-scale",
			Measures: []withings.RawMeasure{
				{Type: withings.TypeWeight, Value: hundredths(kg), Unit: -2},
				{Type: withings.TypeFatRatio, Value: hundredths(fatPct), Unit: -2},
				{Type: withings.TypeMuscleMass, Value: hundredths(muscleKG), Unit: -2},
				{Type: withings.TypeBoneMass, Value: hundredths(boneKG), Unit: -2},
				{Type: withings.TypeHydration, Value: hundredths(hydrationPct), Unit: -2},
				{Type: withings.TypeBMI, Value: int(bmi*10 + 0.5), Unit: -1},
				{Type: withings.TypeVisceralFat, Value: hundredths(visceralFat), Unit: -2},
				{Type: withings.TypeBMR, Value: hundredths(bmrKcal), Unit: -2},
				{Type: withings.TypeMetabolicAge, Value: hundredths(metabolicAgeYears), Unit: -2},
			},
		},
		updated: f.updateTime,
	})
}

func (f *fakeWithings) scriptStatuses(statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, statuses...)
}

func (f *fakeWithings) killRefresh() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshDead = true
}

// blockMeasure makes the next getmeas calls hang until the returned func runs.
func (f *fakeWithings) blockMeasure() func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	hold := make(chan struct{})
	f.hold = hold
	return sync.OnceFunc(func() { close(hold) })
}

func (f *fakeWithings) counts() (measure, token int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.measureCalls, f.tokenCalls
}

func (f *fakeWithings) form() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastForm
}

func (f *fakeWithings) tokens() (access, refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.access, f.refresh
}

func (f *fakeWithings) probeResults() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.probes...)
}

func (f *fakeWithings) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls++

	if r.PostForm.Get("action") != "requesttoken" {
		writeJSON(w, 200, map[string]any{"status": 503, "error": "missing action"})
		return
	}
	if r.PostForm.Get("client_id") != f.clientID || r.PostForm.Get("client_secret") != f.clientSecret {
		writeJSON(w, 200, map[string]any{"status": 503, "error": "bad client credentials"})
		return
	}
	if r.PostForm.Get("grant_type") != "refresh_token" {
		writeJSON(w, 200, map[string]any{"status": 503, "error": "unsupported grant"})
		return
	}
	if f.refreshDead || r.PostForm.Get("refresh_token") != f.refresh {
		writeJSON(w, 200, map[string]any{"status": 401, "error": "invalid_grant"})
		return
	}

	f.rotations++
	f.access = fmt.Sprintf("w-access-%d", f.rotations)
	f.refresh = fmt.Sprintf("w-refresh-%d", f.rotations)
	writeJSON(w, 200, map[string]any{"status": 0, "body": map[string]any{
		"userid":        f.userID,
		"access_token":  f.access,
		"refresh_token": f.refresh,
		"expires_in":    10800,
		"scope":         "user.metrics",
		"token_type":    "Bearer",
	}})
}

func (f *fakeWithings) handleMeasure(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	f.mu.Lock()
	f.measureCalls++
	f.lastForm = r.PostForm
	hold := f.hold
	if f.probe != nil {
		f.probes = append(f.probes, f.probe())
	}
	if len(f.statuses) > 0 {
		status := f.statuses[0]
		f.statuses = f.statuses[1:]
		if status != 0 {
			f.mu.Unlock()
			writeJSON(w, 200, map[string]any{"status": status, "error": "scripted"})
			return
		}
	}
	if got, want := r.Header.Get("Authorization"), "Bearer "+f.access; got != want {
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"status": 401, "error": "invalid_token"})
		return
	}

	groups := f.selectGroups(r.PostForm)
	cursor := f.updateTime
	f.mu.Unlock()

	if hold != nil {
		select {
		case <-hold:
		case <-time.After(5 * time.Second):
			f.t.Error("fake withings: getmeas hold was never released")
		}
	}

	body := map[string]any{"updatetime": cursor, "timezone": "UTC", "measuregrps": groups}
	writeJSON(w, 200, map[string]any{"status": 0, "body": body})
}

// selectGroups honors either windowing mode; caller holds f.mu.
func (f *fakeWithings) selectGroups(form url.Values) []withings.RawMeasureGroup {
	out := []withings.RawMeasureGroup{}
	if raw := form.Get("lastupdate"); raw != "" {
		lastUpdate, _ := strconv.ParseInt(raw, 10, 64)
		for _, g := range f.groups {
			if g.updated > lastUpdate {
				out = append(out, g.grp)
			}
		}
		return out
	}
	start, _ := strconv.ParseInt(form.Get("startdate"), 10, 64)
	end, _ := strconv.ParseInt(form.Get("enddate"), 10, 64)
	for _, g := range f.groups {
		if g.grp.Date >= start && (end == 0 || g.grp.Date <= end) {
			out = append(out, g.grp)
		}
	}
	return out
}

// ── fake Garmin ──────────────────────────────────────────

type uploadAttempt struct {
	fit    []byte
	auth   string
	status int
	probed string // what the probe saw on disk when this upload arrived
}

type fakeGarmin struct {
	srv *httptest.Server
	t   *testing.T

	mu          sync.Mutex
	clientID    string
	access      string
	refresh     string
	rotations   int
	usedRefresh map[string]bool
	refreshDead bool

	statuses       []int // scripted upload statuses, popped per call
	retryAfterSecs int   // sent with a scripted 429

	attempts   []uploadAttempt
	tokenCalls int

	// probe, when set, runs on every upload; see fakeWithings.probe.
	probe func() string
}

func newFakeGarmin(t *testing.T) *fakeGarmin {
	f := &fakeGarmin{
		t:           t,
		clientID:    garmin.DIClientIDs[0],
		access:      "g-access-0",
		refresh:     "g-refresh-0",
		usedRefresh: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", f.handleToken)
	mux.HandleFunc("/upload-service/upload", f.handleUpload)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGarmin) scriptStatuses(statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, statuses...)
}

func (f *fakeGarmin) killRefresh() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshDead = true
}

// expectClientID changes the DI client id the token endpoint accepts, so a
// refresh fails with invalid_client — Garmin rotating its client ids out from
// under us. Transient, not a dead grant.
func (f *fakeGarmin) expectClientID(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clientID = id
}

func (f *fakeGarmin) setProbe(fn func() string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probe = fn
}

func (f *fakeGarmin) allAttempts() []uploadAttempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uploadAttempt(nil), f.attempts...)
}

// accepted returns only the uploads Garmin took, in order.
func (f *fakeGarmin) accepted() []uploadAttempt {
	var out []uploadAttempt
	for _, a := range f.allAttempts() {
		if a.status == http.StatusOK {
			out = append(out, a)
		}
	}
	return out
}

func (f *fakeGarmin) tokens() (access, refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.access, f.refresh
}

func (f *fakeGarmin) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

// handleToken enforces the two rules that matter: the DI client id must be the
// one that originally worked, and a refresh token is single-use.
func (f *fakeGarmin) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls++

	if got := basicUser(r.Header.Get("Authorization")); got != f.clientID {
		writeJSON(w, 400, map[string]any{"error": "invalid_client"})
		return
	}
	if r.PostForm.Get("client_id") != f.clientID {
		writeJSON(w, 400, map[string]any{"error": "invalid_client"})
		return
	}
	if r.PostForm.Get("grant_type") != "refresh_token" {
		writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
		return
	}

	presented := r.PostForm.Get("refresh_token")
	switch {
	case f.refreshDead, presented != f.refresh, f.usedRefresh[presented]:
		// Garmin echoes the submitted token in the description; the client
		// must never let this body reach a log.
		writeJSON(w, 400, map[string]any{
			"error":             "invalid_grant",
			"error_description": "token " + presented + " is not valid",
		})
		return
	}

	f.usedRefresh[presented] = true
	f.rotations++
	f.access = fmt.Sprintf("g-access-%d", f.rotations)
	f.refresh = fmt.Sprintf("g-refresh-%d", f.rotations)
	writeJSON(w, 200, map[string]any{
		"access_token":  f.access,
		"refresh_token": f.refresh,
		"expires_in":    3600,
		"token_type":    "Bearer",
	})
}

func (f *fakeGarmin) handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		f.t.Errorf("fake garmin: multipart parse: %v", err)
		w.WriteHeader(400)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		f.t.Errorf("fake garmin: no file part: %v", err)
		w.WriteHeader(400)
		return
	}
	defer file.Close()
	if header.Filename != "weigh-in.fit" {
		f.t.Errorf("fake garmin: filename = %q, want weigh-in.fit", header.Filename)
	}
	if got := r.Header.Get("NK"); got != "NT" {
		f.t.Errorf("fake garmin: NK header = %q, want NT", got)
	}
	fit, _ := io.ReadAll(file)
	auth := r.Header.Get("Authorization")

	f.mu.Lock()
	status := http.StatusOK
	if len(f.statuses) > 0 {
		status = f.statuses[0]
		f.statuses = f.statuses[1:]
	} else if auth != "Bearer "+f.access {
		status = http.StatusUnauthorized
	}
	retryAfter := f.retryAfterSecs
	probed := ""
	if f.probe != nil {
		probed = f.probe()
	}
	f.attempts = append(f.attempts, uploadAttempt{fit: fit, auth: auth, status: status, probed: probed})
	f.mu.Unlock()

	if status == http.StatusTooManyRequests && retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"status":` + strconv.Itoa(status) + `}`))
}

func basicUser(header string) string {
	raw, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return ""
	}
	user, _, _ := strings.Cut(string(decoded), ":")
	return user
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── FIT reader (just enough to assert on what we sent) ───

// Offsets from the encoder: 14 header + 18 file_id def + 10 file_id data +
// 36 weight_scale def, then 1 + 4 + 7×2 + 2 per record, then a 2-byte CRC.
const (
	fitFirstRecord = 14 + 18 + 10 + 36
	fitRecordLen   = 1 + 4 + 7*2 + 2
)

// Field order inside a weight_scale record, straight from the encoder's
// definition message.
type fitRecord struct {
	timestamp    uint32
	weight       uint16 // kg × 100
	fat          uint16 // % × 100
	hydration    uint16 // % × 100
	bone         uint16 // kg × 100
	muscle       uint16 // kg × 100
	basalMet     uint16 // kcal/day × 4
	metabolicAge uint8  // years
	visceralFat  uint8  // rating
	bmi          uint16 // × 10
}

func parseFIT(t *testing.T, b []byte) []fitRecord {
	t.Helper()
	if len(b) < fitFirstRecord+2 {
		t.Fatalf("FIT too short: %d bytes", len(b))
	}
	if string(b[8:12]) != ".FIT" {
		t.Fatalf("FIT signature = %q", string(b[8:12]))
	}
	payload := len(b) - 2 - fitFirstRecord
	if payload%fitRecordLen != 0 {
		t.Fatalf("FIT body %d bytes is not a whole number of %d-byte records", payload, fitRecordLen)
	}
	records := make([]fitRecord, payload/fitRecordLen)
	for i := range records {
		off := fitFirstRecord + i*fitRecordLen
		if b[off] != 0x01 {
			t.Fatalf("record %d header = %02x, want 01", i, b[off])
		}
		// The two uint8 ratings sit between muscle_mass and bmi, so only the
		// leading six fields are evenly spaced.
		field := func(n int) uint16 { return binary.LittleEndian.Uint16(b[off+5+n*2 : off+7+n*2]) }
		records[i] = fitRecord{
			timestamp:    binary.LittleEndian.Uint32(b[off+1 : off+5]),
			weight:       field(0),
			fat:          field(1),
			hydration:    field(2),
			bone:         field(3),
			muscle:       field(4),
			basalMet:     field(5),
			metabolicAge: b[off+17],
			visceralFat:  b[off+18],
			bmi:          binary.LittleEndian.Uint16(b[off+19 : off+21]),
		}
	}
	return records
}

// ── harness ──────────────────────────────────────────────

type harness struct {
	t   *testing.T
	dir string
	st  *store.Store
	s   *Syncer
	w   *fakeWithings
	g   *fakeGarmin
}

// newHarness returns a fully connected syncer whose clients talk to the fakes
// and whose waits are collapsed to nothing.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	w := newFakeWithings(t)
	g := newFakeGarmin(t)
	t.Cleanup(withings.SetEndpointsForTest(w.srv.URL+"/v2/oauth2", w.srv.URL+"/measure"))
	t.Cleanup(garmin.SetEndpointsForTest(g.srv.URL+"/oauth/token", g.srv.URL+"/upload-service/upload"))

	st.State.SetupComplete = true
	st.State.Withings = store.Withings{
		ClientID:     w.clientID,
		ClientSecret: w.clientSecret,
		AccessToken:  w.access,
		RefreshToken: w.refresh,
		ExpiresAt:    time.Now().Add(time.Hour),
		UserID:       w.userID,
		LastUpdate:   1000,
	}
	st.State.Garmin = store.Garmin{
		Email:        "user@example.com",
		AccessToken:  g.access,
		RefreshToken: g.refresh,
		ExpiresAt:    time.Now().Add(time.Hour),
		DIClientID:   g.clientID,
	}
	if err := st.Save(); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	s := New(st)
	s.backoffShort = 0
	s.backoffLong = 0
	s.interBatchDelay = 0
	s.startupDelay = 10 * time.Millisecond
	s.pollDelay = 10 * time.Millisecond

	return &harness{t: t, dir: dir, st: st, s: s, w: w, g: g}
}

func (h *harness) tick() {
	h.t.Helper()
	if !h.s.runTick(h.t.Context()) {
		h.t.Fatal("runTick refused: syncer busy")
	}
}

// watchDisk makes both fakes report, on every request, which refresh tokens the
// state file holds at that instant — proof that a rotated pair was persisted
// BEFORE the next call used it.
func (h *harness) watchDisk() {
	read := func() (string, string) {
		st, err := store.Open(h.dir)
		if err != nil {
			return "", "" // handlers may not call t.Fatal
		}
		return st.State.Withings.RefreshToken, st.State.Garmin.RefreshToken
	}
	h.w.mu.Lock()
	h.w.probe = func() string { w, _ := read(); return w }
	h.w.mu.Unlock()
	h.g.mu.Lock()
	h.g.probe = func() string { _, g := read(); return g }
	h.g.mu.Unlock()
}

// reopen reads the state file back from disk — the only way to prove a Save
// actually happened.
func (h *harness) reopen() *store.Store {
	h.t.Helper()
	st, err := store.Open(h.dir)
	if err != nil {
		h.t.Fatalf("reopen store: %v", err)
	}
	return st
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
