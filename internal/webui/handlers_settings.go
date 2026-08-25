package webui

// Settings and the opt-in update check — the only outbound request that is not
// Withings or Garmin. It runs server-side (the browser never talks to
// github.com) and is cached for a day.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/store"
)

const (
	minInterval, maxInterval = 5, 1440
	// Ports below 1024 need privileges we deliberately never have.
	minPort, maxPort = 1024, 65535

	intervalRangeMsg = "interval_minutes must be between 5 and 1440"
)

// A var, not a const, only so tests can point it at an httptest server.
var releasesURL = "https://api.github.com/repos/ulfdalen/scalebridge-sync/releases/latest"

type settingsResp struct {
	IntervalMinutes int  `json:"interval_minutes"`
	Port            int  `json:"port"`
	UpdateCheck     bool `json:"update_check"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	var out settingsResp
	_ = s.sy.Do(func(st *store.Store) error {
		out = settingsResp{
			IntervalMinutes: st.State.Settings.IntervalMinutes,
			Port:            st.State.Settings.Port,
			UpdateCheck:     st.State.Settings.UpdateCheck,
		}
		return nil
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	// Pointers so "absent" and "zero" stay distinguishable: the UI sends one
	// field at a time.
	var req struct {
		IntervalMinutes *int  `json:"interval_minutes"`
		Port            *int  `json:"port"`
		UpdateCheck     *bool `json:"update_check"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.IntervalMinutes != nil && !validInterval(*req.IntervalMinutes) {
		writeErr(w, http.StatusBadRequest, "bad_request", intervalRangeMsg)
		return
	}
	if req.Port != nil && (*req.Port < minPort || *req.Port > maxPort) {
		writeErr(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("port must be between %d and %d", minPort, maxPort))
		return
	}

	restartRequired := false
	err := s.sy.Do(func(st *store.Store) error {
		set := &st.State.Settings
		if req.IntervalMinutes != nil {
			set.IntervalMinutes = *req.IntervalMinutes
		}
		if req.Port != nil && *req.Port != set.Port {
			// The listener is already bound: the new port applies on the next
			// start, and Withings needs the new callback URL first.
			set.Port = *req.Port
			restartRequired = true
		}
		if req.UpdateCheck != nil {
			set.UpdateCheck = *req.UpdateCheck
		}
		return st.Save()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restartRequired})
}

func validInterval(m int) bool { return m >= minInterval && m <= maxInterval }

// ── update check ──────────────────────────────────────

type updateInfo struct {
	Newer  bool   `json:"newer"`
	Latest string `json:"latest"`
	URL    string `json:"url"`
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	// POST is the user pressing "Check now" and always hits the network. A GET
	// may only do so when the daily check is opted in; otherwise it serves the
	// cache and never phones home.
	force := r.Method == http.MethodPost
	if !force {
		var optIn bool
		_ = s.sy.Do(func(st *store.Store) error {
			optIn = st.State.Settings.UpdateCheck
			return nil
		})
		if !optIn {
			s.mu.Lock()
			cached := s.update
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"current": s.version,
				"latest":  cached.Latest,
				"url":     cached.URL,
				"newer":   cached.Newer,
			})
			return
		}
	}
	info, err := s.checkUpdate(r.Context(), force)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "github_unreachable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current": s.version,
		"latest":  info.Latest,
		"url":     info.URL,
		"newer":   info.Newer,
	})
}

func (s *Server) checkUpdate(ctx context.Context, force bool) (updateInfo, error) {
	s.mu.Lock()
	cached, at := s.update, s.updateAt
	s.mu.Unlock()
	if !force && !at.IsZero() && time.Since(at) < updateCacheTTL {
		return cached, nil
	}

	info, err := fetchLatestRelease(ctx, s.version)
	if err != nil {
		return updateInfo{}, err
	}

	s.mu.Lock()
	s.update, s.updateAt = info, time.Now()
	s.mu.Unlock()
	return info, nil
}

func fetchLatestRelease(ctx context.Context, version string) (updateInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return updateInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "scalebridge-sync/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return updateInfo{}, err
	}
	defer resp.Body.Close()
	// 404 means the repo has no releases yet — "nothing newer", not an outage.
	if resp.StatusCode == http.StatusNotFound {
		return updateInfo{}, nil
	}
	if resp.StatusCode >= 400 {
		return updateInfo{}, fmt.Errorf("github returned HTTP %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, maxBody)).Decode(&release); err != nil {
		return updateInfo{}, fmt.Errorf("decode github response: %w", err)
	}

	return updateInfo{
		// A dev build is never "out of date": it is ahead of, or unrelated to,
		// whatever the last tag is.
		Newer:  release.TagName != "" && version != "dev" && !runningRelease(release.TagName, version),
		Latest: release.TagName,
		URL:    release.HTMLURL,
	}, nil
}

// runningRelease reports whether the running version is the tag itself or a
// post-tag build of it (git describe yields e.g. 0.0.1-7-gabc123 on main).
func runningRelease(tag, version string) bool {
	v := "v" + version
	return v == tag || strings.HasPrefix(v, tag+"-")
}
