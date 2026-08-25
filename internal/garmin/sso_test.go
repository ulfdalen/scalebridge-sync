package garmin

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyTokenError(t *testing.T) {
	// Garmin echoes the submitted refresh token in the invalid_grant
	// description — the classifier must not leak it.
	const secretToken = "eyJhbGciOiJIUzI1NiJ.SECRETREFRESHTOKEN.abc123"
	invalidGrantBody := []byte(`{"error":"invalid_grant","error_description":"Invalid refresh token: ` + secretToken + `"}`)

	err := classifyTokenError(400, invalidGrantBody, "GARMIN_CONNECT_MOBILE_ANDROID_DI_2025Q2")
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("invalid_grant must map to ErrAuthExpired, got %v", err)
	}
	if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "SECRETREFRESHTOKEN") {
		t.Fatalf("SECURITY: error leaks the refresh token: %q", err.Error())
	}

	// Other errors keep only the code, not the description.
	other := classifyTokenError(400, []byte(`{"error":"invalid_client","error_description":"bad client secret xyz"}`), "cid")
	if errors.Is(other, ErrAuthExpired) {
		t.Fatal("invalid_client should not be ErrAuthExpired")
	}
	if strings.Contains(other.Error(), "bad client secret xyz") {
		t.Fatalf("error leaks the description: %q", other.Error())
	}
	if !strings.Contains(other.Error(), "invalid_client") {
		t.Fatalf("error should include the code: %q", other.Error())
	}

	// Unparseable body must not panic or leak.
	garbage := classifyTokenError(500, []byte("<html>secret-in-html</html>"), "cid")
	if strings.Contains(garbage.Error(), "secret-in-html") {
		t.Fatalf("error leaks raw body: %q", garbage.Error())
	}
}

func TestTruncateForLogRedactsTokens(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	body := []byte(`{"error":"invalid_grant","error_description":"Invalid refresh token: ` + jwt + `"}`)
	out := truncateForLog(body)
	if strings.Contains(out, jwt) || strings.Contains(out, "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV") {
		t.Fatalf("truncateForLog leaked the token: %q", out)
	}
	if !strings.Contains(out, "<redacted-token>") {
		t.Fatalf("expected redaction marker, got %q", out)
	}
	// Non-token content is preserved.
	if !strings.Contains(out, "invalid_grant") {
		t.Fatalf("over-redacted: %q", out)
	}
}
