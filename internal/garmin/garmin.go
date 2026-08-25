// Package garmin implements Garmin Connect's reverse-engineered mobile SSO
// flow and the weight-service upload. The constants below are exact values
// from the mobile app: change one and auth fails with an unhelpful error.
// Fix source when Garmin breaks it: github.com/cyberjunky/python-garminconnect.
package garmin

import (
	"net/http"
	"net/http/cookiejar"
	"time"

	"golang.org/x/net/publicsuffix"
)

const (
	SSOClientID = "GCM_IOS_DARK"
	SSOService  = "https://mobile.integration.garmin.com/gcm/ios"
	SSOLocale   = "en-US"

	iOSUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148"
	ssoOrigin    = "https://sso.garmin.com"
)

// Endpoints are vars for test overrides.
var (
	loginURL  = "https://sso.garmin.com/mobile/api/login"
	mfaURL    = "https://sso.garmin.com/mobile/api/mfa/verifyCode"
	tokenURL  = "https://diauth.garmin.com/di-oauth2-service/oauth/token"
	uploadURL = "https://connectapi.garmin.com/upload-service/upload"
)

// SetEndpointsForTest points the token and upload endpoints at test servers and
// returns a func that restores the real ones.
func SetEndpointsForTest(token, upload string) func() {
	prevToken, prevUpload := tokenURL, uploadURL
	tokenURL, uploadURL = token, upload
	return func() { tokenURL, uploadURL = prevToken, prevUpload }
}

// SetSSOEndpointsForTest points the login and MFA endpoints at test servers and
// returns a func that restores the real ones.
func SetSSOEndpointsForTest(login, mfa string) func() {
	prevLogin, prevMFA := loginURL, mfaURL
	loginURL, mfaURL = login, mfa
	return func() { loginURL, mfaURL = prevLogin, prevMFA }
}

// DIClientIDs is the token-exchange fallback chain, tried in order — first
// accept wins. Persist the winning ID: refresh must reuse the same one.
var DIClientIDs = []string{
	"GARMIN_CONNECT_MOBILE_ANDROID_DI_2025Q2",
	"GARMIN_CONNECT_MOBILE_ANDROID_DI_2024Q4",
	"GARMIN_CONNECT_MOBILE_ANDROID_DI",
	"GARMIN_CONNECT_MOBILE_IOS_DI",
}

// NewHTTPClient returns a client with a cookie jar. Login and VerifyMFA must
// share it — the MFA step needs the session cookies login sets.
func NewHTTPClient() (*http.Client, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
	}, nil
}

func setMobileHeaders(req *http.Request) {
	req.Header.Set("User-Agent", iOSUserAgent)
	req.Header.Set("Origin", ssoOrigin)
	req.Header.Set("Accept", "application/json")
}
