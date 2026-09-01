package webui

// Everything the dashboard reads, plus the two manual sync triggers.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/syncer"
)

const (
	defaultMeasurementLimit = 50
	defaultEventLimit       = 20
)

type statusResp struct {
	Version       string             `json:"version"`
	SetupComplete bool               `json:"setup_complete"`
	ConfigPath    string             `json:"config_path"`
	Withings      withingsStatusResp `json:"withings"`
	Garmin        garminStatusResp   `json:"garmin"`
	Sync          syncStatusResp     `json:"sync"`
	Backfill      backfillResp       `json:"backfill"`
	Update        updateInfo         `json:"update"`
}

type withingsStatusResp struct {
	CredsSet          bool   `json:"creds_set"`
	Connected         bool   `json:"connected"`
	ReconnectRequired bool   `json:"reconnect_required"`
	UserID            string `json:"user_id"`
}

type garminStatusResp struct {
	Connected         bool   `json:"connected"`
	ReconnectRequired bool   `json:"reconnect_required"`
	Email             string `json:"email"`
}

type syncStatusResp struct {
	Running         bool       `json:"running"`
	LastAt          *time.Time `json:"last_at"`
	LastError       string     `json:"last_error"`
	LastFetched     int        `json:"last_fetched"`
	LastUploaded    int        `json:"last_uploaded"`
	NextAt          *time.Time `json:"next_at"`
	IntervalMinutes int        `json:"interval_minutes"`
}

type backfillResp struct {
	Running bool `json:"running"`
	Done    int  `json:"done"`
	Total   int  `json:"total"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	live := s.sy.Status()
	out := statusResp{
		Version: s.version,
		Sync: syncStatusResp{
			Running:      live.Running,
			LastAt:       timePtr(live.LastAt),
			LastError:    live.LastError,
			LastFetched:  live.LastFetched,
			LastUploaded: live.LastUploaded,
			NextAt:       timePtr(live.NextAt),
		},
		Backfill: backfillResp{Running: live.Backfill.Running, Done: live.Backfill.Done, Total: live.Backfill.Total},
	}

	_ = s.sy.Do(func(st *store.Store) error {
		state := st.State
		out.SetupComplete = state.SetupComplete
		out.ConfigPath = st.FilePath()
		out.Sync.IntervalMinutes = state.Settings.IntervalMinutes
		out.Withings = withingsStatusResp{
			CredsSet:          state.Withings.ClientID != "" && state.Withings.ClientSecret != "",
			Connected:         state.Withings.AccessToken != "",
			ReconnectRequired: state.Withings.ReconnectRequired,
			UserID:            state.Withings.UserID,
		}
		out.Garmin = garminStatusResp{
			Connected:         state.Garmin.AccessToken != "",
			ReconnectRequired: state.Garmin.ReconnectRequired,
			Email:             state.Garmin.Email,
		}
		return nil
	})

	// The cached answer only: the dashboard polls every 5s, so this handler must
	// never reach out to GitHub.
	s.mu.Lock()
	out.Update = s.update
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, out)
}

// Spells out the nulls: store.Measurement omits empty pointers, and the API
// contract promises the keys are always present.
type measurementResp struct {
	MeasuredAt        time.Time `json:"measured_at"`
	WeightKG          float64   `json:"weight_kg"`
	BodyFatPct        *float64  `json:"body_fat_pct"`
	MuscleKG          *float64  `json:"muscle_kg"`
	BoneKG            *float64  `json:"bone_kg"`
	HydrationPct      *float64  `json:"hydration_pct"`
	BMI               *float64  `json:"bmi"`
	VisceralFat       *float64  `json:"visceral_fat"`
	BMRKcal           *float64  `json:"bmr_kcal"`
	MetabolicAgeYears *float64  `json:"metabolic_age_years"`
	Synced            bool      `json:"synced"`
	SyncError         string    `json:"sync_error"`
}

func (s *Server) handleMeasurements(w http.ResponseWriter, r *http.Request) {
	limit := queryLimit(r, defaultMeasurementLimit)

	items := []measurementResp{}
	_ = s.sy.Do(func(st *store.Store) error {
		recent := st.State.Recent
		// Recent is oldest-first on disk; the dashboard wants newest first.
		for i := len(recent) - 1; i >= 0 && len(items) < limit; i-- {
			m := recent[i]
			items = append(items, measurementResp{
				MeasuredAt:        m.MeasuredAt,
				WeightKG:          m.WeightKG,
				BodyFatPct:        m.BodyFatPct,
				MuscleKG:          m.MuscleKG,
				BoneKG:            m.BoneKG,
				HydrationPct:      m.HydrationPct,
				BMI:               m.BMI,
				VisceralFat:       m.VisceralFat,
				BMRKcal:           m.BMRKcal,
				MetabolicAgeYears: m.MetabolicAgeYears,
				Synced:            m.Synced,
				SyncError:         m.SyncError,
			})
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events := s.sy.Events(queryLimit(r, defaultEventLimit))
	if events == nil {
		events = []syncer.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": events})
}

func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	if !s.sy.TriggerSync() {
		writeErr(w, http.StatusConflict, "already_running", "a sync is already in progress")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"started": true})
}

func (s *Server) handleSyncBackfill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Range string `json:"range"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	switch req.Range {
	case "30d", "1y", "all":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "range must be 30d, 1y or all")
		return
	}
	// The range is already known-good, so a refusal can only mean "busy".
	if !s.sy.StartBackfill(req.Range) {
		writeErr(w, http.StatusConflict, "already_running", "a sync is already in progress")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"started": true})
}

// ── small helpers ─────────────────────────────────────

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func queryLimit(r *http.Request, fallback int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
