package garmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// LoginResult carries the outcome of the credential step. Exactly one of
// ServiceTicket or MFARequired is set.
type LoginResult struct {
	ServiceTicket string
	MFARequired   bool
	MFAMethod     string // "email" | "sms" | "authenticator"
	Cookies       string // serialized cookies for MFA verify
}

type loginResponse struct {
	ServiceTicketID string `json:"serviceTicketId"`
	ResponseStatus  struct {
		Type string `json:"type"`
	} `json:"responseStatus"`
	CustomerMfaInfo struct {
		MfaLastMethodUsed string `json:"mfaLastMethodUsed"`
	} `json:"customerMfaInfo"`
}

// Login submits credentials and returns a service ticket, or MFARequired when
// Garmin wants a code. The client must have a cookie jar and be reused for
// VerifyMFA.
func Login(ctx context.Context, client *http.Client, email, password string) (*LoginResult, error) {
	body, _ := json.Marshal(map[string]any{
		"username":     email,
		"password":     password,
		"rememberMe":   true,
		"captchaToken": "",
	})

	q := url.Values{}
	q.Set("clientId", SSOClientID)
	q.Set("locale", SSOLocale)
	q.Set("service", SSOService)
	u := loginURL + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	setMobileHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("garmin login: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrInvalidCredentials
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("garmin login: HTTP %d body=%s", resp.StatusCode, truncateForLog(raw))
	}

	var lr loginResponse
	if err := json.Unmarshal(raw, &lr); err != nil {
		return nil, fmt.Errorf("garmin login: decode: %w (body=%s)", err, truncateForLog(raw))
	}

	if lr.ServiceTicketID != "" {
		return &LoginResult{ServiceTicket: lr.ServiceTicketID}, nil
	}
	if lr.ResponseStatus.Type == "MFA_REQUIRED" {
		return &LoginResult{
			MFARequired: true,
			MFAMethod:   lr.CustomerMfaInfo.MfaLastMethodUsed,
		}, nil
	}
	return nil, fmt.Errorf("garmin login: unexpected response body=%s", truncateForLog(raw))
}

// VerifyMFA submits the MFA code. It must run on the same client as Login so
// the SSO session cookies carry over.
func VerifyMFA(ctx context.Context, client *http.Client, method, code string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"mfaMethod":           method,
		"mfaVerificationCode": code,
		"rememberMyBrowser":   true,
		"reconsentList":       []string{},
		"mfaSetup":            false,
	})

	q := url.Values{}
	q.Set("clientId", SSOClientID)
	q.Set("locale", SSOLocale)
	q.Set("service", SSOService)
	u := mfaURL + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", fmt.Sprintf("https://sso.garmin.com/sso/signin?clientId=%s&service=%s", SSOClientID, url.QueryEscape(SSOService)))
	setMobileHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("garmin mfa: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized {
		return "", ErrInvalidMFACode
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("garmin mfa: HTTP %d body=%s", resp.StatusCode, truncateForLog(raw))
	}

	var lr loginResponse
	if err := json.Unmarshal(raw, &lr); err != nil {
		return "", fmt.Errorf("garmin mfa: decode: %w body=%s", err, truncateForLog(raw))
	}
	if lr.ServiceTicketID == "" {
		return "", fmt.Errorf("garmin mfa: no service ticket in response body=%s", truncateForLog(raw))
	}
	return lr.ServiceTicketID, nil
}

// ExchangeServiceTicket trades a service ticket for tokens, trying DIClientIDs
// in order. The returned TokenSet records which one worked.
func ExchangeServiceTicket(ctx context.Context, serviceTicket string) (*TokenSet, error) {
	var lastErr error
	for _, cid := range DIClientIDs {
		tokens, err := diTokenRequest(ctx, map[string]string{
			"client_id":      cid,
			"service_ticket": serviceTicket,
			"grant_type":     "https://connectapi.garmin.com/di-oauth2-service/oauth/grant/service_ticket",
			"service_url":    SSOService, // never echo back the serviceURL from the login response
		}, cid)
		if err == nil {
			tokens.DIClientID = cid
			return tokens, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no DI client IDs tried")
	}
	return nil, fmt.Errorf("exchange service ticket (all clients failed): %w", lastErr)
}

// TokenSet is a token pair plus the DI client ID that produced it. Persist all
// three — refresh needs the same client ID.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	DIClientID   string
}

type diTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func diTokenRequest(ctx context.Context, params map[string]string, clientID string) (*TokenSet, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", iOSUserAgent)
	// Basic auth is the client ID with an empty password — the colon matters.
	auth := base64.StdEncoding.EncodeToString([]byte(clientID + ":"))
	req.Header.Set("Authorization", "Basic "+auth)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("di token request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, classifyTokenError(resp.StatusCode, raw, clientID)
	}

	var tr diTokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("di token: decode: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("di token: empty access_token body=%s", truncateForLog(raw))
	}
	return &TokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// classifyTokenError reports a DI token 4xx using only the OAuth error code:
// Garmin echoes the submitted token in the invalid_grant description, so the
// response body must never reach a log. invalid_grant means the refresh token
// is dead.
func classifyTokenError(status int, body []byte, clientID string) error {
	var oe struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &oe)
	if oe.Error == "invalid_grant" {
		return fmt.Errorf("di token: invalid_grant (client=%s): %w", clientID, ErrAuthExpired)
	}
	code := oe.Error
	if code == "" {
		code = "unknown"
	}
	return fmt.Errorf("di token: HTTP %d error=%s (client=%s)", status, code, clientID)
}

// Refresh rotates the token pair using the DI client ID that originally worked.
// Refresh tokens are single-use: serialize refresh and persist the new pair
// before using it.
func Refresh(ctx context.Context, refreshToken, diClientID string) (*TokenSet, error) {
	tokens, err := diTokenRequest(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     diClientID,
		"refresh_token": refreshToken,
	}, diClientID)
	if err != nil {
		return nil, err
	}
	tokens.DIClientID = diClientID
	return tokens, nil
}

var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrInvalidMFACode     = errors.New("invalid MFA code")
	ErrAuthExpired        = errors.New("garmin auth expired (refresh failed)")
)

// jwtRe matches JWT-shaped tokens (three base64url segments).
var jwtRe = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}`)

// truncateForLog caps a response body and redacts any token in it — Garmin
// echoes submitted tokens back in some error bodies.
func truncateForLog(b []byte) string {
	s := jwtRe.ReplaceAllString(string(b), "<redacted-token>")
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}
