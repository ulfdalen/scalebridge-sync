package main

import (
	"strings"
	"testing"
)

func TestParsePublicURL(t *testing.T) {
	good := map[string]string{
		"":                               "",
		"https://scale.example.com":      "https://scale.example.com",
		"https://scale.example.com/":     "https://scale.example.com",
		" https://scale.example.com ":    "https://scale.example.com",
		"HTTPS://Scale.Example.com:443/": "https://scale.example.com", // browsers write origins this way
		"https://scale.example.com:":     "https://scale.example.com",
		"http://nas.local:80":            "http://nas.local",
		"http://nas.local:8080":          "http://nas.local:8080",
		"http://192.168.1.5:8080":        "http://192.168.1.5:8080",
		"https://[::1]:8443":             "https://[::1]:8443",
	}
	for in, want := range good {
		u, err := parsePublicURL(in)
		if err != nil {
			t.Fatalf("parsePublicURL(%q): %v", in, err)
		}
		got := ""
		if u != nil {
			got = u.String()
		}
		if got != want {
			t.Fatalf("parsePublicURL(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		"scale.example.com", // no scheme
		"192.168.1.5:8080",
		"ftp://scale.example.com",
		"https://scale.example.com/app", // a path would not survive into /callback
		"https://scale.example.com?x=1",
		"https://scale.example.com?", // ForceQuery: String() would keep the "?"
		"https://scale.example.com#frag",
		"https://user:pw@scale.example.com",
		"https://scale.example.com:0",
		"https://scale.example.com:99999",
		"https://bücher.example", // url.URL would percent-escape it
		"https://",
	} {
		if _, err := parsePublicURL(in); err == nil {
			t.Fatalf("parsePublicURL(%q): want an error", in)
		}
	}

	// The message names both ways of setting it and stays friendly for host:port.
	_, err := parsePublicURL("192.168.1.5:8080")
	if err == nil || !strings.Contains(err.Error(), "SCALEBRIDGE_PUBLIC_URL") || !strings.Contains(err.Error(), "http://") {
		t.Fatalf("error for host:port input = %v", err)
	}
}

func TestServiceArgs(t *testing.T) {
	if got := strings.Join(serviceArgs("/cfg", nil), " "); got != "run --config /cfg" {
		t.Fatalf("serviceArgs without public URL = %q", got)
	}
	u, _ := parsePublicURL("https://scale.example.com")
	if got := strings.Join(serviceArgs("/cfg", u), " "); got != "run --config /cfg --public-url https://scale.example.com" {
		t.Fatalf("serviceArgs with public URL = %q", got)
	}
}

// A bad SCALEBRIDGE_PUBLIC_URL must not break commands that never use it.
func TestBadPublicURLEnvIgnoredByOtherCommands(t *testing.T) {
	t.Setenv("SCALEBRIDGE_PUBLIC_URL", "garbage")
	if code := run([]string{"version"}); code != 0 {
		t.Fatalf("version with a bad SCALEBRIDGE_PUBLIC_URL: exit %d, want 0", code)
	}
}
