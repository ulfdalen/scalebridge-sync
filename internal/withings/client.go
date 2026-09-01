// Package withings reads weigh-ins from the Withings Health API.
// Two quirks: the OAuth endpoint needs an action=requesttoken form field, and
// every response wraps its payload in {"status":0,"body":{…}} — a non-zero
// status is an error even under HTTP 200. https://developer.withings.com/api-reference
package withings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Endpoints are vars for test overrides.
var (
	authorizeURL = "https://account.withings.com/oauth2_user/authorize2"
	tokenURL     = "https://wbsapi.withings.net/v2/oauth2"
	measureURL   = "https://wbsapi.withings.net/measure"
)

// SetEndpointsForTest points the token and measure endpoints at test servers
// and returns a func that restores the real ones.
func SetEndpointsForTest(token, measure string) func() {
	prevToken, prevMeasure := tokenURL, measureURL
	tokenURL, measureURL = token, measure
	return func() { tokenURL, measureURL = prevToken, prevMeasure }
}

// Scope covering weight and body composition.
const scopeMetrics = "user.metrics"

// Measurement type codes for the meastypes parameter.
// https://developer.withings.com/developer-guide/v3/data-api/all-available-health-data/
const (
	TypeWeight       = 1
	TypeFatRatio     = 6
	TypeMuscleMass   = 76
	TypeHydration    = 77
	TypeBoneMass     = 88
	TypeBMI          = 75
	TypeVisceralFat  = 170
	TypeBMR          = 226
	TypeMetabolicAge = 227 // in the API-reference enum, but absent from the device-compatibility list — may never arrive
)

type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

type Client struct {
	cfg  Config
	http *http.Client
}

func NewClient(cfg Config) *Client {
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

// AuthorizeURL builds the user-facing OAuth redirect.
func (c *Client) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("state", state)
	q.Set("scope", scopeMetrics)
	q.Set("redirect_uri", c.cfg.RedirectURL)
	return authorizeURL + "?" + q.Encode()
}

// ── token types ──────────────────────────────────────────

type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	UserID       string // Withings account id
}

type tokenBody struct {
	UserID       any    `json:"userid"` // arrives as a string or a number
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
}

type apiResponse[T any] struct {
	Status int    `json:"status"`
	Body   T      `json:"body"`
	Error  string `json:"error"`
}

// ExchangeCode trades an authorization code for a token set.
func (c *Client) ExchangeCode(ctx context.Context, code string) (*TokenSet, error) {
	form := url.Values{}
	form.Set("action", "requesttoken")
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURL)
	return c.postToken(ctx, form)
}

// Refresh rotates the token pair. Withings mints a new refresh token on every
// call and the old one survives only a short grace period, so persist the
// returned pair.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*TokenSet, error) {
	form := url.Values{}
	form.Set("action", "requesttoken")
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	form.Set("refresh_token", refreshToken)
	return c.postToken(ctx, form)
}

func (c *Client) postToken(ctx context.Context, form url.Values) (*TokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out apiResponse[tokenBody]
	if err := json.Unmarshal(raw, &out); err != nil {
		// Never embed the body: this endpoint returns tokens in plaintext.
		return nil, fmt.Errorf("withings token: decode: %w", err)
	}
	// Envelope status 401 is the only one meaning "the user must authorize
	// again"; every other non-zero status is transient or a bad request.
	if out.Status == 401 {
		return nil, fmt.Errorf("withings token: status=401 error=%q: %w", out.Error, ErrAuthExpired)
	}
	if out.Status != 0 {
		return nil, fmt.Errorf("withings token: status=%d error=%q", out.Status, out.Error)
	}

	userID, _ := coerceString(out.Body.UserID)
	return &TokenSet{
		AccessToken:  out.Body.AccessToken,
		RefreshToken: out.Body.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(out.Body.ExpiresIn) * time.Second),
		UserID:       userID,
	}, nil
}

// ── measurements ─────────────────────────────────────────

// RawMeasure is one measurement value as returned by getmeas.
type RawMeasure struct {
	Value int `json:"value"`
	Type  int `json:"type"`
	Unit  int `json:"unit"` // scale: actual = value * 10^unit
}

type RawMeasureGroup struct {
	GroupID  int64        `json:"grpid"`
	Date     int64        `json:"date"`     // epoch seconds
	Category int          `json:"category"` // 1=real, 2=user-entered
	Measures []RawMeasure `json:"measures"`
	DeviceID string       `json:"deviceid"`
}

type measureBody struct {
	Updatetime  int64             `json:"updatetime"` // epoch seconds; the next lastupdate cursor
	TimeZone    string            `json:"timezone"`
	Measuregrps []RawMeasureGroup `json:"measuregrps"`
}

// Measurement is a normalized weigh-in: weight, muscle and bone in kg, fat and
// hydration in %.
type Measurement struct {
	GroupID           int64
	MeasuredAt        time.Time
	WeightKG          float64
	BodyFatPct        *float64
	MuscleMassKG      *float64
	BoneMassKG        *float64
	HydrationPct      *float64
	BMI               *float64
	VisceralFat       *float64 // unitless index, roughly 1–30
	BMRKcal           *float64 // kcal/day
	MetabolicAgeYears *float64 // years
	DeviceID          string
}

// GetMeasures returns real (category=1) measurements from since until now, over
// an inclusive absolute window. Used for backfill.
func (c *Client) GetMeasures(ctx context.Context, accessToken string, since time.Time) ([]Measurement, error) {
	form := measureForm()
	form.Set("startdate", strconv.FormatInt(since.Unix(), 10))
	form.Set("enddate", strconv.FormatInt(time.Now().Unix(), 10))

	measurements, _, err := c.getmeas(ctx, accessToken, form)
	return measurements, err
}

// GetMeasuresSince returns everything recorded or edited after the lastUpdate
// cursor, plus the next cursor, which comes from the server's clock rather than
// ours. lastUpdate=0 means everything.
func (c *Client) GetMeasuresSince(ctx context.Context, accessToken string, lastUpdate int64) ([]Measurement, int64, error) {
	form := measureForm()
	form.Set("lastupdate", strconv.FormatInt(lastUpdate, 10))

	return c.getmeas(ctx, accessToken, form)
}

func measureForm() url.Values {
	form := url.Values{}
	form.Set("action", "getmeas")
	form.Set("meastypes", fmt.Sprintf("%d,%d,%d,%d,%d,%d,%d,%d,%d",
		TypeWeight, TypeFatRatio, TypeMuscleMass, TypeHydration, TypeBoneMass, TypeBMI,
		TypeVisceralFat, TypeBMR, TypeMetabolicAge))
	form.Set("category", "1")
	return form
}

func (c *Client) getmeas(ctx context.Context, accessToken string, form url.Values) ([]Measurement, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, measureURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out apiResponse[measureBody]
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, 0, fmt.Errorf("withings getmeas decode: %w (raw=%s)", err, truncate(raw, 200))
	}
	if out.Status == 401 {
		return nil, 0, ErrAuthExpired
	}
	if out.Status != 0 {
		return nil, 0, fmt.Errorf("withings getmeas: status=%d error=%q", out.Status, out.Error)
	}

	measurements := make([]Measurement, 0, len(out.Body.Measuregrps))
	for _, g := range out.Body.Measuregrps {
		m, ok := normalize(g)
		if !ok {
			continue
		}
		measurements = append(measurements, m)
	}
	return measurements, out.Body.Updatetime, nil
}

// ErrAuthExpired means Withings rejected the token: refresh and retry, or ask
// the user to reconnect.
var ErrAuthExpired = errors.New("withings auth expired")

// normalize converts one measuregrp into a Measurement. Groups without a weight
// reading are dropped (ok=false).
func normalize(g RawMeasureGroup) (Measurement, bool) {
	m := Measurement{
		GroupID:    g.GroupID,
		MeasuredAt: time.Unix(g.Date, 0).UTC(),
		DeviceID:   g.DeviceID,
	}

	hasWeight := false
	for _, v := range g.Measures {
		val := float64(v.Value) * pow10(v.Unit)
		switch v.Type {
		case TypeWeight:
			m.WeightKG = val
			hasWeight = true
		case TypeFatRatio:
			m.BodyFatPct = floatPtr(val)
		case TypeMuscleMass:
			m.MuscleMassKG = floatPtr(val)
		case TypeHydration:
			m.HydrationPct = floatPtr(val)
		case TypeBoneMass:
			m.BoneMassKG = floatPtr(val)
		case TypeBMI:
			m.BMI = floatPtr(val)
		case TypeVisceralFat:
			m.VisceralFat = floatPtr(val)
		case TypeBMR:
			m.BMRKcal = floatPtr(val)
		case TypeMetabolicAge:
			m.MetabolicAgeYears = floatPtr(val)
		}
	}
	return m, hasWeight
}

func pow10(n int) float64 {
	p := 1.0
	if n >= 0 {
		for i := 0; i < n; i++ {
			p *= 10
		}
		return p
	}
	for i := 0; i < -n; i++ {
		p /= 10
	}
	return p
}

func floatPtr(v float64) *float64 { return &v }

func coerceString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatInt(int64(x), 10), true
	case int:
		return strconv.Itoa(x), true
	default:
		return "", false
	}
}

// truncate caps a response body before it goes into an error message: getmeas
// bodies hold no secrets, but a whole one is a measurement history.
func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
