package withings

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ── helpers ──────────────────────────────────────────────

func serveMeasure(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	prev := measureURL
	measureURL = srv.URL
	t.Cleanup(func() {
		measureURL = prev
		srv.Close()
	})
}

func serveToken(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	prev := tokenURL
	tokenURL = srv.URL
	t.Cleanup(func() {
		tokenURL = prev
		srv.Close()
	})
}

func testClient() *Client {
	return NewClient(Config{
		ClientID:     "cid",
		ClientSecret: "csecret",
		RedirectURL:  "http://localhost:8723/callback",
	})
}

// captureForm records the posted form so assertions can run after the call.
func captureForm(form *url.Values, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func deref(t *testing.T, p *float64) float64 {
	t.Helper()
	if p == nil {
		t.Fatal("expected non-nil pointer")
	}
	return *p
}

// ── normalize ────────────────────────────────────────────

func TestNormalize(t *testing.T) {
	tests := []struct {
		name  string
		group RawMeasureGroup
		ok    bool
		check func(t *testing.T, m Measurement)
	}{
		{
			name: "weight fat and bmi with negative units",
			group: RawMeasureGroup{
				GroupID: 42,
				Date:    1700000000,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 805, Unit: -1},    // 80.5 kg
					{Type: TypeFatRatio, Value: 1834, Unit: -2}, // 18.34 %
					{Type: TypeBMI, Value: 24, Unit: 0},         // 24
				},
				DeviceID: "dev-1",
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				if m.GroupID != 42 {
					t.Errorf("GroupID = %d, want 42", m.GroupID)
				}
				if !m.MeasuredAt.Equal(time.Unix(1700000000, 0).UTC()) {
					t.Errorf("MeasuredAt = %v", m.MeasuredAt)
				}
				if m.WeightKG != 80.5 {
					t.Errorf("WeightKG = %v, want 80.5", m.WeightKG)
				}
				if got := deref(t, m.BodyFatPct); got != 18.34 {
					t.Errorf("BodyFatPct = %v, want 18.34", got)
				}
				if got := deref(t, m.BMI); got != 24 {
					t.Errorf("BMI = %v, want 24", got)
				}
				if m.DeviceID != "dev-1" {
					t.Errorf("DeviceID = %q", m.DeviceID)
				}
				if m.MuscleMassKG != nil || m.BoneMassKG != nil || m.HydrationPct != nil {
					t.Errorf("expected nil pointers for absent measures: %+v", m)
				}
				if m.VisceralFat != nil || m.BMRKcal != nil || m.MetabolicAgeYears != nil {
					t.Errorf("expected nil pointers for absent BodyScan measures: %+v", m)
				}
			},
		},
		{
			name: "visceral fat, bmr and metabolic age",
			group: RawMeasureGroup{
				GroupID: 43,
				Date:    1700000000,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 805, Unit: -1},     // 80.5 kg
					{Type: TypeVisceralFat, Value: 8, Unit: 0},   // index 8
					{Type: TypeBMR, Value: 16855, Unit: -1},      // 1685.5 kcal/day
					{Type: TypeMetabolicAge, Value: 34, Unit: 0}, // 34 years
				},
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				if got := deref(t, m.VisceralFat); got != 8 {
					t.Errorf("VisceralFat = %v, want 8", got)
				}
				if got := deref(t, m.BMRKcal); got != 1685.5 {
					t.Errorf("BMRKcal = %v, want 1685.5", got)
				}
				if got := deref(t, m.MetabolicAgeYears); got != 34 {
					t.Errorf("MetabolicAgeYears = %v, want 34", got)
				}
			},
		},
		{
			name: "positive unit scales up",
			group: RawMeasureGroup{
				GroupID:  7,
				Measures: []RawMeasure{{Type: TypeWeight, Value: 8, Unit: 1}}, // 80 kg
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				if m.WeightKG != 80 {
					t.Errorf("WeightKG = %v, want 80", m.WeightKG)
				}
			},
		},
		{
			name: "no weight is dropped",
			group: RawMeasureGroup{
				GroupID: 9,
				Measures: []RawMeasure{
					{Type: TypeFatRatio, Value: 200, Unit: -1},
					{Type: TypeBoneMass, Value: 30, Unit: -1},
				},
			},
			ok: false,
		},
		{
			name: "no measures at all is dropped",
			group: RawMeasureGroup{
				GroupID:  10,
				Measures: nil,
			},
			ok: false,
		},
		{
			name: "unknown meastype ignored",
			group: RawMeasureGroup{
				GroupID: 11,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 700, Unit: -1},
					{Type: 11, Value: 60, Unit: 0}, // heart rate — not ours
					{Type: 9, Value: 80, Unit: 0},  // diastolic BP
					{Type: TypeHydration, Value: 555, Unit: -1},
				},
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				if m.WeightKG != 70 {
					t.Errorf("WeightKG = %v, want 70", m.WeightKG)
				}
				if got := deref(t, m.HydrationPct); got != 55.5 {
					t.Errorf("HydrationPct = %v, want 55.5", got)
				}
				if m.BodyFatPct != nil || m.BMI != nil {
					t.Errorf("unknown types leaked into %+v", m)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := normalize(tt.group)
			if ok != tt.ok {
				t.Fatalf("normalize ok = %v, want %v", ok, tt.ok)
			}
			if ok && tt.check != nil {
				tt.check(t, m)
			}
		})
	}
}

func TestPow10(t *testing.T) {
	tests := []struct {
		n    int
		want float64
	}{
		{0, 1},
		{1, 10},
		{3, 1000},
		{-1, 0.1},
		{-3, 0.001},
	}
	for _, tt := range tests {
		if got := pow10(tt.n); got != tt.want {
			t.Errorf("pow10(%d) = %v, want %v", tt.n, got, tt.want)
		}
	}
}

func TestCoerceString(t *testing.T) {
	tests := []struct {
		name   string
		in     any
		want   string
		wantOK bool
	}{
		{"json string", "1234567", "1234567", true},
		{"go int", 1234567, "1234567", true},
		{"json number", float64(1234567), "1234567", true},
		{"unsupported", []string{"nope"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := coerceString(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("coerceString(%v) = %q,%v; want %q,%v", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// ── getmeas ──────────────────────────────────────────────

func TestGetMeasuresSince(t *testing.T) {
	var form url.Values
	serveMeasure(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		if got := r.Header.Get("Authorization"); got != "Bearer at-1" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":0,"body":{
			"updatetime": 1700009999,
			"timezone": "Europe/Berlin",
			"measuregrps": [
				{"grpid": 1, "date": 1700000000, "category": 1, "deviceid": "dev-1",
				 "measures": [{"value":805,"type":1,"unit":-1},{"value":1834,"type":6,"unit":-2}]},
				{"grpid": 2, "date": 1700003600, "category": 1,
				 "measures": [{"value":1834,"type":6,"unit":-2}]}
			]}}`))
	})

	ms, cursor, err := testClient().GetMeasuresSince(context.Background(), "at-1", 1699999999)
	if err != nil {
		t.Fatalf("GetMeasuresSince: %v", err)
	}

	if got := form.Get("lastupdate"); got != "1699999999" {
		t.Errorf("lastupdate = %q, want 1699999999", got)
	}
	if form.Has("startdate") || form.Has("enddate") {
		t.Errorf("absolute window params must not be sent: %v", form)
	}
	if got := form.Get("action"); got != "getmeas" {
		t.Errorf("action = %q", got)
	}
	if got := form.Get("category"); got != "1" {
		t.Errorf("category = %q", got)
	}
	if got := form.Get("meastypes"); got != "1,6,76,77,88,75,170,226,227" {
		t.Errorf("meastypes = %q", got)
	}

	if cursor != 1700009999 {
		t.Errorf("cursor = %d, want 1700009999", cursor)
	}
	// Group 2 has no weight, so it is dropped.
	if len(ms) != 1 {
		t.Fatalf("got %d measurements, want 1: %+v", len(ms), ms)
	}
	if ms[0].GroupID != 1 || ms[0].WeightKG != 80.5 {
		t.Errorf("measurement = %+v", ms[0])
	}
}

func TestGetMeasuresSendsAbsoluteWindow(t *testing.T) {
	var form url.Values
	serveMeasure(t, captureForm(&form, `{"status":0,"body":{"updatetime":1,"measuregrps":[]}}`))

	since := time.Unix(1690000000, 0)
	if _, err := testClient().GetMeasures(context.Background(), "at-1", since); err != nil {
		t.Fatalf("GetMeasures: %v", err)
	}
	if got := form.Get("startdate"); got != "1690000000" {
		t.Errorf("startdate = %q, want 1690000000", got)
	}
	if form.Get("enddate") == "" {
		t.Error("enddate missing")
	}
	if form.Has("lastupdate") {
		t.Errorf("lastupdate must not be sent by GetMeasures: %v", form)
	}
}

func TestGetMeasuresSinceAuthExpired(t *testing.T) {
	// A dead access token arrives as envelope status 401 inside an HTTP 200.
	serveMeasure(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":401,"error":"invalid_token"}`))
	})

	_, _, err := testClient().GetMeasuresSince(context.Background(), "at-1", 0)
	if err != ErrAuthExpired {
		t.Fatalf("err = %v, want ErrAuthExpired", err)
	}
}

func TestGetMeasuresSinceAPIError(t *testing.T) {
	serveMeasure(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":503,"error":"Invalid Params"}`))
	})

	_, _, err := testClient().GetMeasuresSince(context.Background(), "at-1", 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q should mention status 503", err)
	}
}

func TestGetMeasuresSinceTruncatesRawBody(t *testing.T) {
	junk := strings.Repeat("x", 5000)
	serveMeasure(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>" + junk + "</html>"))
	})

	_, _, err := testClient().GetMeasuresSince(context.Background(), "at-1", 0)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if n := strings.Count(err.Error(), "x"); n > 200 {
		t.Errorf("raw body not truncated: %d body bytes in error", n)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error %q should say it was truncated", err)
	}
}

// ── token endpoint ───────────────────────────────────────

func TestExchangeCode(t *testing.T) {
	tests := []struct {
		name   string
		userid string // raw JSON, quoted or not
		want   string
	}{
		{"userid as string", `"1234567"`, "1234567"},
		{"userid as number", `1234567`, "1234567"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var form url.Values
			serveToken(t, captureForm(&form, `{"status":0,"body":{
				"userid": `+tt.userid+`,
				"access_token": "at-new",
				"refresh_token": "rt-new",
				"expires_in": 10800,
				"scope": "user.metrics",
				"token_type": "Bearer"}}`))

			ts, err := testClient().ExchangeCode(context.Background(), "the-code")
			if err != nil {
				t.Fatalf("ExchangeCode: %v", err)
			}

			// Without action=requesttoken the endpoint answers 503.
			if got := form.Get("action"); got != "requesttoken" {
				t.Errorf("action = %q, want requesttoken", got)
			}
			if got := form.Get("grant_type"); got != "authorization_code" {
				t.Errorf("grant_type = %q", got)
			}
			if got := form.Get("code"); got != "the-code" {
				t.Errorf("code = %q", got)
			}
			if got := form.Get("client_id"); got != "cid" {
				t.Errorf("client_id = %q", got)
			}
			if got := form.Get("client_secret"); got != "csecret" {
				t.Errorf("client_secret = %q", got)
			}
			if got := form.Get("redirect_uri"); got != "http://localhost:8723/callback" {
				t.Errorf("redirect_uri = %q", got)
			}

			if ts.AccessToken != "at-new" || ts.RefreshToken != "rt-new" {
				t.Errorf("tokens = %+v", ts)
			}
			if ts.UserID != tt.want {
				t.Errorf("UserID = %q, want %q", ts.UserID, tt.want)
			}
			if d := time.Until(ts.ExpiresAt); d < 3*time.Hour-time.Minute || d > 3*time.Hour {
				t.Errorf("ExpiresAt in %v, want ~3h", d)
			}
		})
	}
}

func TestRefresh(t *testing.T) {
	var form url.Values
	serveToken(t, captureForm(&form, `{"status":0,"body":{
		"userid": "1234567",
		"access_token": "at-2",
		"refresh_token": "rt-2",
		"expires_in": 10800}}`))

	ts, err := testClient().Refresh(context.Background(), "rt-1")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := form.Get("action"); got != "requesttoken" {
		t.Errorf("action = %q, want requesttoken", got)
	}
	if got := form.Get("grant_type"); got != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", got)
	}
	if got := form.Get("refresh_token"); got != "rt-1" {
		t.Errorf("refresh_token = %q", got)
	}
	// The rotated refresh token must win over the one that was sent.
	if ts.AccessToken != "at-2" || ts.RefreshToken != "rt-2" {
		t.Errorf("tokens = %+v", ts)
	}
}

func TestRefreshAuthExpired(t *testing.T) {
	// A dead refresh token arrives as envelope status 401 inside an HTTP 200.
	serveToken(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":401,"error":"invalid_grant"}`))
	})

	_, err := testClient().Refresh(context.Background(), "rt-dead")
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("err = %v, want ErrAuthExpired", err)
	}
}

func TestRefreshTransientStatusIsNotAuthExpired(t *testing.T) {
	// Every other status is transient; treating one as auth expiry would
	// disconnect the user over a blip.
	serveToken(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":503,"error":"Invalid Params"}`))
	})

	_, err := testClient().Refresh(context.Background(), "rt-1")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrAuthExpired) {
		t.Fatalf("status 503 must not classify as ErrAuthExpired: %v", err)
	}
}

func TestTokenErrorStatus(t *testing.T) {
	serveToken(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":503,"error":"Invalid Params"}`))
	})

	if _, err := testClient().Refresh(context.Background(), "rt-1"); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q should mention status 503", err)
	}
}

func TestTokenErrorNeverLeaksBody(t *testing.T) {
	// Token endpoint bodies carry tokens in plaintext, so an undecodable one
	// (captive-portal HTML, say) must never reach the error.
	serveToken(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>access_token=SUPERSECRET-abc123 refresh_token=SUPERSECRET-def456</html>`))
	})

	_, err := testClient().Refresh(context.Background(), "rt-1")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("token material leaked into error: %q", err)
	}
	if strings.Contains(err.Error(), "html") {
		t.Fatalf("raw body leaked into error: %q", err)
	}
}
