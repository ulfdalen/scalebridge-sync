package webui

// The wizard's endpoints: the server-derived progress the UI resumes from, the
// Withings credential form, the backfill choice, and the finish line.

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/store"
)

type setupSteps struct {
	WithingsCreds bool `json:"withings_creds"`
	WithingsOAuth bool `json:"withings_oauth"`
	Garmin        bool `json:"garmin"`
	Backfill      bool `json:"backfill"`
}

type setupState struct {
	Port            int        `json:"port"`
	CallbackURL     string     `json:"callback_url"`
	ClientID        string     `json:"client_id"`
	ClientSecretSet bool       `json:"client_secret_set"`
	Steps           setupSteps `json:"steps"`
	LastOAuthError  string     `json:"last_oauth_error"`
	LastOAuthDetail string     `json:"last_oauth_detail"`
}

// The exact string the user registered at Withings, built from the bound port
// and never from the Host header: a mismatch is the most common setup failure.
func (s *Server) callbackURL() string {
	return fmt.Sprintf("http://localhost:%d/callback", s.port)
}

func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	out := setupState{Port: s.port, CallbackURL: s.callbackURL()}

	_ = s.sy.Do(func(st *store.Store) error {
		wi, g := st.State.Withings, st.State.Garmin
		out.ClientID = wi.ClientID
		// The secret is write-only: only ever report that one is stored.
		out.ClientSecretSet = wi.ClientSecret != ""
		out.Steps.WithingsCreds = wi.ClientID != "" && wi.ClientSecret != ""
		out.Steps.WithingsOAuth = wi.AccessToken != ""
		out.Steps.Garmin = g.AccessToken != ""
		return nil
	})

	s.mu.Lock()
	out.LastOAuthError, out.LastOAuthDetail = s.lastOAuthError, s.lastOAuthDetail
	out.Steps.Backfill = s.backfillChoice != ""
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleWithingsCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.ClientSecret = strings.TrimSpace(req.ClientSecret)
	if req.ClientID == "" || req.ClientSecret == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "client_id and client_secret are required")
		return
	}

	err := s.sy.Do(func(st *store.Store) error {
		wi := &st.State.Withings
		if wi.ClientID != req.ClientID {
			// The tokens belong to the old developer app: the new credentials
			// could never refresh them.
			wi.AccessToken, wi.RefreshToken, wi.UserID = "", "", ""
			wi.ExpiresAt = time.Time{}
			wi.ReconnectRequired = false
		}
		wi.ClientID, wi.ClientSecret = req.ClientID, req.ClientSecret
		return st.Save()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	writeOK(w)
}

func (s *Server) handleSetupBackfill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Choice string `json:"choice"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	switch req.Choice {
	case "none", "30d", "1y", "all":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "choice must be none, 30d, 1y or all")
		return
	}

	// In memory only: it drives exactly one action, at setup/complete.
	s.mu.Lock()
	s.backfillChoice = req.Choice
	s.mu.Unlock()
	writeOK(w)
}

func (s *Server) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IntervalMinutes int `json:"interval_minutes"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.IntervalMinutes != 0 && !validInterval(req.IntervalMinutes) {
		writeErr(w, http.StatusBadRequest, "bad_request", intervalRangeMsg)
		return
	}

	s.mu.Lock()
	choice := s.backfillChoice
	s.mu.Unlock()

	err := s.sy.Do(func(st *store.Store) error {
		st.State.SetupComplete = true
		if req.IntervalMinutes != 0 {
			st.State.Settings.IntervalMinutes = req.IntervalMinutes
		}
		if choice == "" || choice == "none" {
			// No history wanted: start the cursor at "now" so the first tick
			// picks up the next weigh-in rather than the last seven days.
			st.State.Withings.LastUpdate = time.Now().Unix()
		}
		return st.Save()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	s.sy.AddEvent("info", "Setup completed")

	switch choice {
	case "30d", "1y", "all":
		s.sy.StartBackfill(choice)
	default:
		s.sy.TriggerSync()
	}
	writeOK(w)
}
