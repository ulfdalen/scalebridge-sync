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
	"math"
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
	TypeWaterMass    = 77 // body water MASS in kg, not a percentage — see HydrationPct
	TypeBoneMass     = 88
	TypeVisceralFat  = 170
	TypeBMR          = 226
	TypeMetabolicAge = 227 // in the API-reference enum, but absent from the device-compatibility list — may never arrive
)

// CoreTypes make an uploadable weigh-in on their own. The app's Withings API
// plan can refuse the advanced types in AllTypes (ErrPlanRestricted), so the
// syncer falls back to CoreTypes.
var (
	CoreTypes = []int{TypeWeight, TypeFatRatio, TypeMuscleMass, TypeWaterMass, TypeBoneMass}
	AllTypes  = []int{TypeWeight, TypeFatRatio, TypeMuscleMass, TypeWaterMass, TypeBoneMass,
		TypeVisceralFat, TypeBMR, TypeMetabolicAge}
)

// maxWeightKG is the most Garmin's FIT weight field holds (0xFFFD × 0.01 kg);
// no scale reading comes near it.
const maxWeightKG = 655.33

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

// ── errors ───────────────────────────────────────────────

// ErrAuthExpired means Withings rejected the token: refresh and retry, or ask
// the user to reconnect.
var ErrAuthExpired = errors.New("withings auth expired")

// ErrPlanRestricted means the app's Withings API plan refused one of the
// requested meastypes; Withings doesn't say which. Refreshing or reconnecting
// can't fix it, asking for fewer types can.
var ErrPlanRestricted = errors.New("withings api plan restriction")

// envelope is the wrapper around every Withings response. Body is decoded only
// once Status says it is a success payload: error replies don't promise its
// shape.
type envelope struct {
	Status *int            `json:"status"`
	Body   json.RawMessage `json:"body"`
	Error  string          `json:"error"`
}

// post sends one form-encoded call and decodes the envelope. Raw bodies never
// go into errors: token replies carry tokens in plaintext and measure replies
// carry health data.
func (c *Client) post(ctx context.Context, endpoint, accessToken string, form url.Values) (envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return envelope{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return envelope{}, err
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Status == nil {
		return envelope{}, fmt.Errorf("response is not a Withings envelope (HTTP %d)", resp.StatusCode)
	}
	return env, nil
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
	env, err := c.post(ctx, tokenURL, "", form)
	if err != nil {
		return nil, fmt.Errorf("withings token: %w", err)
	}
	// Withings' error text stays out: nothing promises this endpoint never
	// echoes what it was sent. Envelope status 401 is the only one meaning "the
	// user must authorize again"; every other non-zero status is transient or a
	// bad request.
	if *env.Status == 401 {
		return nil, fmt.Errorf("withings token: status=401: %w", ErrAuthExpired)
	}
	if *env.Status != 0 {
		return nil, fmt.Errorf("withings token: status=%d", *env.Status)
	}

	var body tokenBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return nil, errors.New("withings token: response body has an unexpected shape")
	}
	// Storing an empty pair would overwrite the refresh token we still hold
	// and leave the connection unrecoverable.
	if body.AccessToken == "" || body.RefreshToken == "" {
		return nil, errors.New("withings token: response carries no tokens")
	}

	userID, _ := coerceString(body.UserID)
	return &TokenSet{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(body.ExpiresIn) * time.Second),
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

// Measurement is a normalized weigh-in: weight, muscle, bone and water in kg,
// fat in %. Optional values are nil when Withings didn't send them or sent
// something that can't be a reading.
type Measurement struct {
	GroupID           int64
	MeasuredAt        time.Time
	WeightKG          float64
	BodyFatPct        *float64
	MuscleMassKG      *float64
	BoneMassKG        *float64
	WaterMassKG       *float64 // hydration % is derived from it, see HydrationPct
	VisceralFat       *float64 // unitless index, roughly 1–30
	BMRKcal           *float64 // kcal/day
	MetabolicAgeYears *float64 // years
	DeviceID          string
}

// GetMeasures returns real (category=1) measurements of the given types over
// the inclusive window [start, end].
func (c *Client) GetMeasures(ctx context.Context, accessToken string, start, end time.Time, types []int) ([]Measurement, error) {
	form := measureForm(types)
	form.Set("startdate", strconv.FormatInt(start.Unix(), 10))
	form.Set("enddate", strconv.FormatInt(end.Unix(), 10))

	measurements, _, err := c.getmeas(ctx, accessToken, form)
	return measurements, err
}

// GetMeasuresSince returns everything recorded or edited after the lastUpdate
// cursor, plus the next cursor, which comes from the server's clock rather than
// ours. lastUpdate=0 means everything.
func (c *Client) GetMeasuresSince(ctx context.Context, accessToken string, lastUpdate int64, types []int) ([]Measurement, int64, error) {
	form := measureForm(types)
	form.Set("lastupdate", strconv.FormatInt(lastUpdate, 10))

	return c.getmeas(ctx, accessToken, form)
}

func measureForm(types []int) url.Values {
	codes := make([]string, len(types))
	for i, t := range types {
		codes[i] = strconv.Itoa(t)
	}
	form := url.Values{}
	form.Set("action", "getmeas")
	form.Set("meastypes", strings.Join(codes, ","))
	form.Set("category", "1")
	return form
}

func (c *Client) getmeas(ctx context.Context, accessToken string, form url.Values) ([]Measurement, int64, error) {
	env, err := c.post(ctx, measureURL, accessToken, form)
	if err != nil {
		return nil, 0, fmt.Errorf("withings getmeas: %w", err)
	}
	switch status := *env.Status; status {
	case 0:
	case 100, 101, 102, 200, 401: // Withings' "Authentication failed" family
		return nil, 0, fmt.Errorf("withings getmeas: status=%d: %w", status, ErrAuthExpired)
	case 277:
		// 277 belongs to Withings' general "Unauthorized" family; only the
		// error text tells a plan restriction apart.
		if strings.Contains(strings.ToLower(env.Error), "api plan") {
			return nil, 0, fmt.Errorf("withings getmeas: status=277 error=%q: %w", truncate(env.Error, 300), ErrPlanRestricted)
		}
		fallthrough
	default:
		return nil, 0, fmt.Errorf("withings getmeas: status=%d error=%q", status, truncate(env.Error, 300))
	}

	var body measureBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return nil, 0, errors.New("withings getmeas: response body has an unexpected shape")
	}
	measurements := make([]Measurement, 0, len(body.Measuregrps))
	for _, g := range body.Measuregrps {
		m, ok := normalize(g)
		if !ok {
			continue
		}
		measurements = append(measurements, m)
	}
	return measurements, body.Updatetime, nil
}

// normalize converts one measuregrp into a Measurement, or ok=false when it
// can't be a weigh-in: no date, no weight, or a weight Garmin can't hold (kept,
// it would fail every upload). A value that can't be a reading — a percentage
// of 100 or more, a part of the body heavier than the body — is dropped on its
// own and the weigh-in kept. Values only ever come from this group, so a
// weigh-in never borrows a reading from another one.
func normalize(g RawMeasureGroup) (Measurement, bool) {
	m := Measurement{
		GroupID:    g.GroupID,
		MeasuredAt: time.Unix(g.Date, 0).UTC(),
		DeviceID:   g.DeviceID,
	}

	for _, v := range g.Measures {
		val := reading(v)
		if val == nil {
			continue // so a repeated type can't erase an earlier reading
		}
		switch v.Type {
		case TypeWeight:
			m.WeightKG = *val
		case TypeFatRatio:
			m.BodyFatPct = val
		case TypeMuscleMass:
			m.MuscleMassKG = val
		case TypeWaterMass:
			m.WaterMassKG = val
		case TypeBoneMass:
			m.BoneMassKG = val
		case TypeVisceralFat:
			m.VisceralFat = val
		case TypeBMR:
			m.BMRKcal = val
		case TypeMetabolicAge:
			m.MetabolicAgeYears = val
		}
	}
	if g.Date <= 0 || m.WeightKG == 0 || m.WeightKG > maxWeightKG {
		return m, false
	}
	m.BodyFatPct = below(m.BodyFatPct, 100)
	m.MuscleMassKG = below(m.MuscleMassKG, m.WeightKG)
	m.BoneMassKG = below(m.BoneMassKG, m.WeightKG)
	m.WaterMassKG = below(m.WaterMassKG, m.WeightKG)
	m.VisceralFat = below(m.VisceralFat, 100)             // Withings' index runs roughly 1–30
	m.BMRKcal = below(m.BMRKcal, 10000)                   // several times any human BMR
	m.MetabolicAgeYears = below(m.MetabolicAgeYears, 150) // years
	return m, true
}

// reading decodes value × 10^unit, or nil unless the result is finite and
// positive: every metric we sync is, and a zero would otherwise travel to
// Garmin as if it had been measured.
func reading(v RawMeasure) *float64 {
	val := float64(v.Value) * math.Pow10(v.Unit)
	if !(val > 0) || math.IsInf(val, 1) {
		return nil
	}
	return &val
}

// below keeps v only while it is under max.
func below(v *float64, max float64) *float64 {
	if v != nil && *v < max {
		return v
	}
	return nil
}

// HydrationPct converts body water mass into the hydration percentage Garmin
// expects, using the weight of the same weigh-in: 42 kg of water at 70 kg is
// 60%, not 42%. ok=false — leave hydration out — unless both are finite and
// 0 < water < weight.
func HydrationPct(waterKG, weightKG float64) (float64, bool) {
	if !(waterKG > 0) || !(waterKG < weightKG) || math.IsInf(weightKG, 1) {
		return 0, false
	}
	return waterKG / weightKG * 100, true
}

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

// truncate caps Withings' error text before it goes into an error message.
func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
