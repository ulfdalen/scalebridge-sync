package webui

// Garmin sign-in. The password is used once, in-process, and is never
// persisted — only the tokens it buys are. MFA is stateful because Garmin's
// verify step must reuse the cookie jar (JSESSIONID / CASTGC) the login step
// filled, so the live *http.Client is parked behind an opaque handle.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/garmin"
	"github.com/ulfdalen/scalebridge-sync/internal/store"
)

type pendingMFA struct {
	client  *http.Client
	email   string
	method  string
	expires time.Time
}

func (s *Server) handleGarminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "email and password are required")
		return
	}

	client, err := garmin.NewHTTPClient()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	res, err := garmin.Login(r.Context(), client, req.Email, req.Password)
	if err != nil {
		writeGarminErr(w, err, garmin.ErrInvalidCredentials, "invalid_credentials")
		return
	}

	if res.MFARequired {
		method := res.MFAMethod
		if method == "" {
			method = "email"
		}
		token, err := s.stashMFA(client, req.Email, method)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mfa_required": true,
			"mfa_method":   method,
			"mfa_token":    token,
		})
		return
	}

	if err := s.finishGarmin(r.Context(), req.Email, res.ServiceTicket); err != nil {
		writeGarminErr(w, err, garmin.ErrInvalidCredentials, "invalid_credentials")
		return
	}
	writeOK(w)
}

func (s *Server) handleGarminVerifyMFA(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MFAToken string `json:"mfa_token"`
		Code     string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "code is required")
		return
	}

	p := s.lookupMFA(req.MFAToken)
	if p == nil {
		writeErr(w, http.StatusGone, "mfa_expired", "that sign-in attempt expired — start again")
		return
	}

	ticket, err := garmin.VerifyMFA(r.Context(), p.client, p.method, strings.TrimSpace(req.Code))
	if err != nil {
		// A wrong code is retryable: keep the parked client so the user can try
		// again without restarting the login.
		writeGarminErr(w, err, garmin.ErrInvalidMFACode, "invalid_mfa_code")
		return
	}
	if err := s.finishGarmin(r.Context(), p.email, ticket); err != nil {
		writeGarminErr(w, err, garmin.ErrInvalidMFACode, "invalid_mfa_code")
		return
	}
	s.dropMFA(req.MFAToken)
	writeOK(w)
}

func (s *Server) handleGarminDisconnect(w http.ResponseWriter, r *http.Request) {
	err := s.sy.Do(func(st *store.Store) error {
		st.State.Garmin = store.Garmin{}
		return st.Save()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	s.sy.AddEvent("info", "Garmin disconnected")
	writeOK(w)
}

// Persists the tokens together with the DI client id that worked: refresh must
// reuse that exact id.
func (s *Server) finishGarmin(ctx context.Context, email, serviceTicket string) error {
	tokens, err := garmin.ExchangeServiceTicket(ctx, serviceTicket)
	if err != nil {
		return err
	}
	if err := s.sy.Do(func(st *store.Store) error {
		g := &st.State.Garmin
		g.Email = email
		g.AccessToken = tokens.AccessToken
		g.RefreshToken = tokens.RefreshToken
		g.ExpiresAt = tokens.ExpiresAt
		g.DIClientID = tokens.DIClientID
		g.ReconnectRequired = false
		return st.Save()
	}); err != nil {
		return err
	}
	s.sy.AddEvent("info", "Garmin connected")
	return nil
}

// Anything that is not a definitive rejection is reported as "unreachable" —
// the honest answer for a timeout, a 5xx or an unexpected body. The detail is
// passed through verbatim: the garmin package already redacts it.
func writeGarminErr(w http.ResponseWriter, err error, rejected error, rejectedCode string) {
	if errors.Is(err, rejected) {
		writeErr(w, http.StatusUnauthorized, rejectedCode, "")
		return
	}
	writeErr(w, http.StatusBadGateway, "garmin_unreachable", err.Error())
}

// ── pending MFA handles ───────────────────────────────

func (s *Server) stashMFA(client *http.Client, email, method string) (string, error) {
	token, err := newNonce()
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.mfa { // opportunistic GC
		if now.After(p.expires) {
			delete(s.mfa, k)
		}
	}
	s.mfa[token] = &pendingMFA{client: client, email: email, method: method, expires: now.Add(mfaTTL)}
	return token, nil
}

func (s *Server) lookupMFA(token string) *pendingMFA {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.mfa[token]
	if !ok {
		return nil
	}
	if time.Now().After(p.expires) {
		delete(s.mfa, token)
		return nil
	}
	return p
}

func (s *Server) dropMFA(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mfa, token)
}
