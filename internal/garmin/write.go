package garmin

// Garmin takes weigh-ins as a FIT upload: one multipart POST per file, which
// may hold a single measurement or a whole backfill batch.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// WriteResult tells the caller whether to retry, reconnect, or give up.
type WriteResult int

const (
	WriteOK          WriteResult = iota
	WriteAuthExpired             // 401 — the user must reconnect
	WriteBadRequest              // 4xx non-auth — bad payload, do not retry
	WriteTransient               // 408/429/5xx/network — retry with backoff
)

// WriteFIT uploads an encoded FIT file and returns the outcome plus any
// Retry-After delay.
func WriteFIT(ctx context.Context, accessToken string, fit []byte) (WriteResult, time.Duration, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "weigh-in.fit")
	if err != nil {
		return WriteBadRequest, 0, fmt.Errorf("multipart: %w", err)
	}
	if _, err := part.Write(fit); err != nil {
		return WriteBadRequest, 0, fmt.Errorf("multipart write: %w", err)
	}
	if err := mw.Close(); err != nil {
		return WriteBadRequest, 0, fmt.Errorf("multipart close: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &buf)
	if err != nil {
		return WriteTransient, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", iOSUserAgent)
	req.Header.Set("NK", "NT")

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return WriteTransient, 0, fmt.Errorf("garmin upload: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// Garmin processes the FIT asynchronously and exposes no status
		// endpoint, so an accepted upload is the only confirmation available.
		return WriteOK, 0, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return WriteAuthExpired, 0, fmt.Errorf("garmin upload: 401 body=%s", truncateForLog(raw))
	case resp.StatusCode == http.StatusTooManyRequests:
		return WriteTransient, parseRetryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("429")
	case resp.StatusCode >= 500:
		return WriteTransient, 0, fmt.Errorf("garmin upload: %d body=%s", resp.StatusCode, truncateForLog(raw))
	}
	return WriteBadRequest, 0, fmt.Errorf("garmin upload: %d body=%s", resp.StatusCode, truncateForLog(raw))
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if d, err := time.ParseDuration(h + "s"); err == nil {
		return d
	}
	return 0
}
