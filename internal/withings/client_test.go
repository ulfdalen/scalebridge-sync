package withings

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
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
			name: "weight and fat with negative units",
			group: RawMeasureGroup{
				GroupID: 42,
				Date:    1700000000,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 805, Unit: -1},    // 80.5 kg
					{Type: TypeFatRatio, Value: 1834, Unit: -2}, // 18.34 %
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
				if m.DeviceID != "dev-1" {
					t.Errorf("DeviceID = %q", m.DeviceID)
				}
				if m.MuscleMassKG != nil || m.BoneMassKG != nil || m.WaterMassKG != nil {
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
			name: "water is mass in kg, and hydration % comes from the same weigh-in",
			group: RawMeasureGroup{
				GroupID: 44,
				Date:    1700000000,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 70000, Unit: -3},   // 70 kg
					{Type: TypeWaterMass, Value: 4200, Unit: -2}, // 42 kg of water
				},
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				water := deref(t, m.WaterMassKG)
				if water != 42 {
					t.Errorf("WaterMassKG = %v, want 42", water)
				}
				if pct, ok := HydrationPct(water, m.WeightKG); !ok || math.Abs(pct-60) > 1e-9 {
					t.Errorf("HydrationPct = %v,%v, want 60 — 42 kg must not ship as 42%%", pct, ok)
				}
			},
		},
		{
			name: "positive unit scales up",
			group: RawMeasureGroup{
				GroupID:  7,
				Date:     1700000000,
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
				Date:    1700000000,
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
				Date:     1700000000,
				Measures: nil,
			},
			ok: false,
		},
		{
			name: "zero weight is dropped",
			group: RawMeasureGroup{
				GroupID:  12,
				Date:     1700000000,
				Measures: []RawMeasure{{Type: TypeWeight, Value: 0, Unit: -2}},
			},
			ok: false,
		},
		{
			name: "negative weight is dropped",
			group: RawMeasureGroup{
				GroupID:  13,
				Date:     1700000000,
				Measures: []RawMeasure{{Type: TypeWeight, Value: -7000, Unit: -2}},
			},
			ok: false,
		},
		{
			name: "dateless group is dropped",
			group: RawMeasureGroup{
				GroupID:  14,
				Measures: []RawMeasure{{Type: TypeWeight, Value: 7000, Unit: -2}},
			},
			ok: false,
		},
		{
			name: "negative date is dropped",
			group: RawMeasureGroup{
				GroupID:  15,
				Date:     -1,
				Measures: []RawMeasure{{Type: TypeWeight, Value: 7000, Unit: -2}},
			},
			ok: false,
		},
		{
			name: "weight above what FIT can hold is dropped",
			group: RawMeasureGroup{
				GroupID:  16,
				Date:     1700000000,
				Measures: []RawMeasure{{Type: TypeWeight, Value: 65534, Unit: -2}}, // 655.34 kg
			},
			ok: false,
		},
		{
			name: "largest FIT weight is kept",
			group: RawMeasureGroup{
				GroupID:  17,
				Date:     1700000000,
				Measures: []RawMeasure{{Type: TypeWeight, Value: 65533, Unit: -2}}, // 655.33 kg
			},
			ok: true,
		},
		{
			name: "unknown meastypes, BMI's 75 among them, are ignored",
			group: RawMeasureGroup{
				GroupID: 11,
				Date:    1700000000,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 700, Unit: -1},
					{Type: 11, Value: 60, Unit: 0}, // heart rate — not ours
					{Type: 9, Value: 80, Unit: 0},  // diastolic BP
					{Type: 75, Value: 24, Unit: 0}, // not a Withings meastype
				},
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				want := Measurement{GroupID: 11, MeasuredAt: time.Unix(1700000000, 0).UTC(), WeightKG: 70}
				if !reflect.DeepEqual(m, want) {
					t.Errorf("unknown types leaked: got %+v, want %+v", m, want)
				}
			},
		},
		{
			name: "a repeated type that isn't a reading keeps the earlier one",
			group: RawMeasureGroup{
				GroupID: 21,
				Date:    1700000000,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 7000, Unit: -2},
					{Type: TypeFatRatio, Value: 1834, Unit: -2},
					{Type: TypeWeight, Value: 0, Unit: -2},
					{Type: TypeFatRatio, Value: -1, Unit: 0},
				},
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				if m.WeightKG != 70 {
					t.Errorf("WeightKG = %v, want 70", m.WeightKG)
				}
				if got := deref(t, m.BodyFatPct); got != 18.34 {
					t.Errorf("BodyFatPct = %v, want 18.34", got)
				}
			},
		},
		{
			name: "impossible readings are dropped one by one, the weigh-in is kept",
			group: RawMeasureGroup{
				GroupID: 18,
				Date:    1700000000,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 7000, Unit: -2},
					{Type: TypeFatRatio, Value: 100, Unit: 0},     // 100 %
					{Type: TypeMuscleMass, Value: 7000, Unit: -2}, // = weight
					{Type: TypeBoneMass, Value: 71, Unit: 0},      // > weight
					{Type: TypeWaterMass, Value: 7000, Unit: -2},  // = weight
					{Type: TypeVisceralFat, Value: 100, Unit: 0},
					{Type: TypeBMR, Value: 10000, Unit: 0},
					{Type: TypeMetabolicAge, Value: 150, Unit: 0},
				},
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				want := Measurement{GroupID: 18, MeasuredAt: time.Unix(1700000000, 0).UTC(), WeightKG: 70}
				if !reflect.DeepEqual(m, want) {
					t.Errorf("got %+v, want only the weight kept", m)
				}
			},
		},
		{
			name: "readings just inside the bounds are kept",
			group: RawMeasureGroup{
				GroupID: 19,
				Date:    1700000000,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 7000, Unit: -2},
					{Type: TypeFatRatio, Value: 9999, Unit: -2},
					{Type: TypeMuscleMass, Value: 6999, Unit: -2},
					{Type: TypeBoneMass, Value: 6999, Unit: -2},
					{Type: TypeWaterMass, Value: 6999, Unit: -2},
					{Type: TypeVisceralFat, Value: 99, Unit: 0},
					{Type: TypeBMR, Value: 9999, Unit: 0},
					{Type: TypeMetabolicAge, Value: 149, Unit: 0},
				},
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				for name, p := range map[string]*float64{
					"fat": m.BodyFatPct, "muscle": m.MuscleMassKG, "bone": m.BoneMassKG, "water": m.WaterMassKG,
					"visceral": m.VisceralFat, "bmr": m.BMRKcal, "metabolic age": m.MetabolicAgeYears,
				} {
					if p == nil {
						t.Errorf("%s was dropped", name)
					}
				}
			},
		},
		{
			name: "zero, negative and non-finite values are not readings",
			group: RawMeasureGroup{
				GroupID: 20,
				Date:    1700000000,
				Measures: []RawMeasure{
					{Type: TypeWeight, Value: 7000, Unit: -2},
					{Type: TypeFatRatio, Value: 0, Unit: -2},
					{Type: TypeMuscleMass, Value: -500, Unit: -2},
					{Type: TypeBoneMass, Value: 1, Unit: 400},      // +Inf
					{Type: TypeWaterMass, Value: 0, Unit: 400},     // 0 × Inf = NaN
					{Type: TypeVisceralFat, Value: 5, Unit: -400},  // underflows to 0
					{Type: TypeBMR, Value: 1, Unit: math.MaxInt32}, // a unit no loop should walk
				},
			},
			ok: true,
			check: func(t *testing.T, m Measurement) {
				want := Measurement{GroupID: 20, MeasuredAt: time.Unix(1700000000, 0).UTC(), WeightKG: 70}
				if !reflect.DeepEqual(m, want) {
					t.Errorf("got %+v, want only the weight kept", m)
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

func TestHydrationPct(t *testing.T) {
	tests := []struct {
		name          string
		water, weight float64
		want          float64
		ok            bool
	}{
		{"42 kg of water at 70 kg", 42, 70, 60, true},
		{"water equal to weight", 70, 70, 0, false},
		{"water above weight", 71, 70, 0, false},
		{"no water", 0, 70, 0, false},
		{"negative water", -1, 70, 0, false},
		{"zero weight", 42, 0, 0, false},
		{"NaN water", math.NaN(), 70, 0, false},
		{"NaN weight", 42, math.NaN(), 0, false},
		{"infinite weight", 42, math.Inf(1), 0, false},
		{"infinite water", math.Inf(1), 70, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := HydrationPct(tt.water, tt.weight)
			if ok != tt.ok || math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("HydrationPct(%v, %v) = %v,%v; want %v,%v", tt.water, tt.weight, got, ok, tt.want, tt.ok)
			}
		})
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

	ms, cursor, err := testClient().GetMeasuresSince(context.Background(), "at-1", 1699999999, AllTypes)
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
	// No 75: it is not a Withings meastype.
	if got := form.Get("meastypes"); got != "1,6,76,77,88,170,226,227" {
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

	start, end := time.Unix(1690000000, 0), time.Unix(1700000000, 0)
	if _, err := testClient().GetMeasures(context.Background(), "at-1", start, end, CoreTypes); err != nil {
		t.Fatalf("GetMeasures: %v", err)
	}
	if got := form.Get("startdate"); got != "1690000000" {
		t.Errorf("startdate = %q, want 1690000000", got)
	}
	if got := form.Get("enddate"); got != "1700000000" {
		t.Errorf("enddate = %q, want 1700000000", got)
	}
	if form.Has("lastupdate") {
		t.Errorf("lastupdate must not be sent by GetMeasures: %v", form)
	}
	if got := form.Get("meastypes"); got != "1,6,76,77,88" {
		t.Errorf("meastypes = %q, want the core types", got)
	}
}

func TestGetMeasuresAuthFailedFamily(t *testing.T) {
	// A dead access token arrives as an envelope status inside an HTTP 200;
	// Withings documents 100-102, 200 and 401 as "Authentication failed".
	for _, status := range []int{100, 101, 102, 200, 401} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			serveMeasure(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"status":%d,"error":"invalid_token"}`, status)
			})

			_, _, err := testClient().GetMeasuresSince(context.Background(), "at-1", 0, AllTypes)
			if !errors.Is(err, ErrAuthExpired) {
				t.Fatalf("err = %v, want ErrAuthExpired", err)
			}
			if errors.Is(err, ErrPlanRestricted) {
				t.Fatalf("err = %v must not be a plan restriction", err)
			}
		})
	}
}

func TestGetMeasuresPlanRestricted(t *testing.T) {
	for _, text := range []string{
		"One of the data type requested in this API request is not available in your API plan. Please check https://developer.withings.com/",
		"not available in your api PLAN",
	} {
		serveMeasure(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"status":277,"error":%q}`, text)
		})

		_, _, err := testClient().GetMeasuresSince(context.Background(), "at-1", 0, AllTypes)
		if !errors.Is(err, ErrPlanRestricted) {
			t.Fatalf("error text %q: err = %v, want ErrPlanRestricted", text, err)
		}
		if errors.Is(err, ErrAuthExpired) {
			t.Fatalf("a plan restriction must never read as auth expiry: %v", err)
		}
	}
}

func TestGetMeasures277WithoutPlanTextIsNeitherPlanNorAuth(t *testing.T) {
	// 277 sits in Withings' general "Unauthorized" family; only the text makes
	// it a plan restriction.
	serveMeasure(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":277,"error":"Unauthorized"}`))
	})

	_, _, err := testClient().GetMeasuresSince(context.Background(), "at-1", 0, AllTypes)
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrPlanRestricted) || errors.Is(err, ErrAuthExpired) {
		t.Fatalf("err = %v, want neither plan nor auth", err)
	}
	if !strings.Contains(err.Error(), "277") {
		t.Errorf("error %q should mention status 277", err)
	}
}

func TestGetMeasuresSinceAPIError(t *testing.T) {
	serveMeasure(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":503,"error":"Invalid Params"}`))
	})

	_, _, err := testClient().GetMeasuresSince(context.Background(), "at-1", 0, AllTypes)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q should mention status 503", err)
	}
}

func TestGetMeasuresErrorsNeverCarryTheBody(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"html page", 200, "<html>" + strings.Repeat("x", 5000) + " SECRET-body</html>"},
		{"html 502 from a proxy", 502, "<html>Bad Gateway SECRET-body</html>"},
		{"success with a body of the wrong shape", 200, `{"status":0,"body":{"measuregrps":"SECRET-body"}}`},
		{"success without a body", 200, `{"status":0}`},
		{"json without a status", 200, `{"body":{"updatetime":5,"measuregrps":[]},"note":"SECRET-body"}`},
		{"error status with a body", 200, `{"status":503,"body":{"SECRET-body":1}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveMeasure(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})

			_, _, err := testClient().GetMeasuresSince(context.Background(), "at-SECRET-token", 0, AllTypes)
			if err == nil {
				t.Fatal("expected error")
			}
			for _, leak := range []string{"SECRET", "xxx", "html"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error %q leaks %q", err, leak)
				}
			}
		})
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

func TestTokenErrorsDropWithingsText(t *testing.T) {
	// Nothing promises the token endpoint never echoes what it was sent.
	for _, status := range []int{401, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			serveToken(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"status":%d,"error":"refresh_token=SUPERSECRET-rt is invalid"}`, status)
			})

			_, err := testClient().Refresh(context.Background(), "SUPERSECRET-rt")
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), "SUPERSECRET") {
				t.Fatalf("token material leaked into error: %q", err)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Errorf("error %q should still name status %d", err, status)
			}
			if got := errors.Is(err, ErrAuthExpired); got != (status == 401) {
				t.Errorf("errors.Is(ErrAuthExpired) = %v for status %d", got, status)
			}
		})
	}
}

func TestTokenReplyWithoutTokensIsAnError(t *testing.T) {
	// Accepting it would overwrite the stored pair with empty strings and log
	// the user out; it must not read as auth expiry either, or it would latch.
	for name, body := range map[string]string{
		"no tokens":        `{"status":0,"body":{"userid":"1","expires_in":10800}}`,
		"no refresh token": `{"status":0,"body":{"access_token":"at-new","expires_in":10800}}`,
		"no access token":  `{"status":0,"body":{"refresh_token":"rt-new","expires_in":10800}}`,
		"empty strings":    `{"status":0,"body":{"access_token":"","refresh_token":""}}`,
		"no body":          `{"status":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			serveToken(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			})

			ts, err := testClient().Refresh(context.Background(), "rt-1")
			if err == nil {
				t.Fatalf("Refresh = %+v, want an error", ts)
			}
			if errors.Is(err, ErrAuthExpired) {
				t.Fatalf("a tokenless reply must not classify as ErrAuthExpired: %v", err)
			}
		})
	}
}
