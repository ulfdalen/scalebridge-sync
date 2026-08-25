package webui

// Withings OAuth: the redirect out, the callback back in, and disconnect.

import (
	"net/http"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/withings"
)

func (s *Server) handleWithingsConnect(w http.ResponseWriter, r *http.Request) {
	// The one state-changing GET: it is a browser navigation target, so the CSRF
	// checks cannot apply. Sec-Fetch-Site separates our own UI ("same-origin")
	// and a typed URL ("none") from an <img> on a hostile page ("cross-site");
	// clients that omit the header (curl) pass.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		writeErr(w, http.StatusForbidden, "csrf", "cross-site request refused")
		return
	}

	var cfg withings.Config
	_ = s.sy.Do(func(st *store.Store) error {
		cfg = withings.Config{
			ClientID:     st.State.Withings.ClientID,
			ClientSecret: st.State.Withings.ClientSecret,
			RedirectURL:  s.callbackURL(),
		}
		return nil
	})
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		writeErr(w, http.StatusConflict, "no_credentials", "save your Withings client id and secret first")
		return
	}

	state, err := s.mintState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// The wizard surfaces last_oauth_error only when the value changes, so two
	// identical failures in a row would otherwise show up once.
	s.setOAuthError("", "")
	http.Redirect(w, r, withings.NewClient(cfg).AuthorizeURL(state), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if !s.consumeState(q.Get("state")) {
		s.setOAuthError("state_mismatch", "")
		renderCallback(w, http.StatusBadRequest, "That link has expired",
			"The authorization link was already used or is older than ten minutes. Start the Withings connection again from the setup window.")
		return
	}
	if e := q.Get("error"); e != "" {
		s.setOAuthError("denied", e)
		renderCallback(w, http.StatusOK, "Not connected",
			"Withings did not grant access. Nothing was changed — you can try again from the setup window.")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.setOAuthError("denied", "no authorization code in the callback")
		renderCallback(w, http.StatusBadRequest, "Not connected",
			"Withings sent us back without an authorization code. You can try again from the setup window.")
		return
	}

	var cfg withings.Config
	_ = s.sy.Do(func(st *store.Store) error {
		cfg = withings.Config{
			ClientID:     st.State.Withings.ClientID,
			ClientSecret: st.State.Withings.ClientSecret,
			RedirectURL:  s.callbackURL(),
		}
		return nil
	})

	// The client's error strings are already token-safe: pass them through.
	tokens, err := withings.NewClient(cfg).ExchangeCode(r.Context(), code)
	if err != nil {
		s.setOAuthError("exchange_failed", err.Error())
		s.sy.AddEvent("error", "Withings authorization failed: "+err.Error())
		renderCallback(w, http.StatusBadGateway, "Could not finish connecting",
			"Withings refused to exchange the authorization code. The Client Secret is the usual culprit: "+err.Error())
		return
	}

	if err := s.sy.Do(func(st *store.Store) error {
		wi := &st.State.Withings
		wi.AccessToken = tokens.AccessToken
		wi.RefreshToken = tokens.RefreshToken
		wi.ExpiresAt = tokens.ExpiresAt
		if tokens.UserID != "" {
			wi.UserID = tokens.UserID
		}
		wi.ReconnectRequired = false
		return st.Save()
	}); err != nil {
		s.setOAuthError("exchange_failed", err.Error())
		renderCallback(w, http.StatusInternalServerError, "Could not finish connecting",
			"The tokens arrived but could not be written to the state file: "+err.Error())
		return
	}

	s.setOAuthError("", "")
	s.sy.AddEvent("info", "Withings connected")
	renderCallback(w, http.StatusOK, "Withings connected",
		"ScaleBridge Sync can now read your weigh-ins.")
}

func (s *Server) handleWithingsDisconnect(w http.ResponseWriter, r *http.Request) {
	err := s.sy.Do(func(st *store.Store) error {
		wi := &st.State.Withings
		// Credentials survive: they are the user's developer app, not a session.
		wi.AccessToken, wi.RefreshToken, wi.UserID = "", "", ""
		wi.ExpiresAt = time.Time{}
		wi.ReconnectRequired = false
		return st.Save()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	s.sy.AddEvent("info", "Withings disconnected")
	writeOK(w)
}

// ── state nonces ──────────────────────────────────────

func (s *Server) mintState() (string, error) {
	nonce, err := newNonce()
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, expires := range s.oauthStates { // opportunistic GC
		if now.After(expires) {
			delete(s.oauthStates, k)
		}
	}
	s.oauthStates[nonce] = now.Add(oauthStateTTL)
	return nonce, nil
}

// Checks and burns a nonce in one step: unknown, expired and already-used are
// indistinguishable, which is the point.
func (s *Server) consumeState(nonce string) bool {
	if nonce == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.oauthStates[nonce]
	if !ok {
		return false
	}
	delete(s.oauthStates, nonce)
	return time.Now().Before(expires)
}

func (s *Server) setOAuthError(code, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastOAuthError, s.lastOAuthDetail = code, detail
}
