// Package syncer runs the Withings → Garmin sync: an interval scheduler, the
// tick itself (fetch → merge → upload), the token wrappers, and the one-shot
// backfill in backfill.go. mu serializes every store access and every unit of
// sync work; stateMu guards what the dashboard reads while a tick holds mu.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/garmin"
	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/withings"
)

const (
	// tokenSkew refreshes a token this long before it actually expires.
	tokenSkew = 60 * time.Second

	// maxBackoff caps any in-tick wait, including a server's Retry-After.
	maxBackoff = 2 * time.Minute

	// fallbackWindow is the absolute window used when the cursor is missing.
	fallbackWindow = 7 * 24 * time.Hour

	defaultInterval = 15 * time.Minute

	maxEvents = 100

	// maxRecentMerge mirrors the store's Recent cap: merging more only gives
	// Save more to trim.
	maxRecentMerge = 30
)

// Status is the dashboard's view of the syncer, readable while a tick runs.
type Status struct {
	Running      bool      // a tick or backfill is in flight
	LastAt       time.Time // zero until the first tick
	LastError    string
	LastFetched  int
	LastUploaded int
	NextAt       time.Time // zero when the scheduler is idle (setup incomplete)
	Backfill     BackfillStatus
}

// BackfillStatus counts batches, not measurements.
type BackfillStatus struct {
	Running     bool
	Done, Total int
}

type Event struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"` // "info" | "warn" | "error"
	Message string    `json:"message"`
}

type Syncer struct {
	st *store.Store

	// mu serializes all store access and all sync work.
	mu sync.Mutex

	// stateMu guards everything below, so Status and Events never block on mu.
	stateMu       sync.Mutex
	running       bool
	lastAt        time.Time
	lastError     string
	lastFetched   int
	lastUploaded  int
	nextAt        time.Time
	backfillState BackfillStatus
	events        []Event

	// trigger wakes the scheduler so it re-arms its timer from now.
	trigger chan struct{}

	// Waits, as fields so tests can collapse them to zero.
	backoffShort    time.Duration
	backoffLong     time.Duration
	interBatchDelay time.Duration
	startupDelay    time.Duration
	pollDelay       time.Duration
}

func New(st *store.Store) *Syncer {
	return &Syncer{
		st:              st,
		trigger:         make(chan struct{}, 1),
		backoffShort:    5 * time.Second,
		backoffLong:     30 * time.Second,
		interBatchDelay: 2 * time.Second,
		startupDelay:    2 * time.Second,
		pollDelay:       2 * time.Second,
	}
}

var (
	errWithingsNotConnected = errors.New("Withings is not connected")
	errWithingsReconnect    = errors.New("Withings needs to be reconnected")
	errGarminNotConnected   = errors.New("Garmin is not connected")
	errGarminReconnect      = errors.New("Garmin needs to be reconnected")
)

// ── public surface ───────────────────────────────────────

// Do runs fn with the store locked. Every store access from outside this
// package goes through it, so nothing observes a half-written tick.
func (s *Syncer) Do(fn func(*store.Store) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(s.st)
}

// TriggerSync runs a tick in the background. It returns false immediately —
// never blocking, never queueing — if a tick or backfill is already in flight.
// The tick uses context.Background(): it is not tied to the scheduler's life.
func (s *Syncer) TriggerSync() bool {
	if !s.begin() {
		return false
	}
	go func() {
		defer s.end()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.tick(context.Background())
	}()
	s.poke()
	return true
}

// Status is a snapshot; it never blocks on sync work.
func (s *Syncer) Status() Status {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return Status{
		Running:      s.running,
		LastAt:       s.lastAt,
		LastError:    s.lastError,
		LastFetched:  s.lastFetched,
		LastUploaded: s.lastUploaded,
		NextAt:       s.nextAt,
		Backfill:     s.backfillState,
	}
}

// Events returns up to limit events, newest first. limit <= 0 means all.
func (s *Syncer) Events(limit int) []Event {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if limit <= 0 || limit > len(s.events) {
		limit = len(s.events)
	}
	out := make([]Event, 0, limit)
	for i := len(s.events) - 1; i >= len(s.events)-limit; i-- {
		out = append(out, s.events[i])
	}
	return out
}

// AddEvent appends to the in-memory event log and mirrors the line to slog.
// Never pass token material: client errors are already redacted, so pass them
// through unchanged.
func (s *Syncer) AddEvent(level, message string) {
	switch level {
	case "error":
		slog.Error(message)
	case "warn":
		slog.Warn(message)
	default:
		slog.Info(message)
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.events = append(s.events, Event{At: time.Now(), Level: level, Message: message})
	if n := len(s.events); n > maxEvents {
		s.events = s.events[n-maxEvents:]
	}
}

// ── scheduler ────────────────────────────────────────────

// Run drives the tick loop until ctx is cancelled. While setup is incomplete it
// idles (NextAt zero), polling cheaply so the first tick lands shortly after the
// wizard finishes rather than a full interval later.
func (s *Syncer) Run(ctx context.Context) {
	first := true
	for {
		interval, ready := s.readSchedule()
		if !ready {
			s.setNextAt(time.Time{})
			first = true
			select {
			case <-ctx.Done():
				return
			case <-s.trigger:
			case <-time.After(s.pollDelay):
			}
			continue
		}

		delay := interval
		if first {
			delay = s.startupDelay
		}
		first = false

		s.setNextAt(time.Now().Add(delay))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.setNextAt(time.Time{})
			return
		case <-timer.C:
			s.runTick(ctx)
		case <-s.trigger:
			// A manual sync is running or just ran; re-arm from now.
			timer.Stop()
		}
	}
}

func (s *Syncer) readSchedule() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.st.State.SetupComplete {
		return 0, false
	}
	d := time.Duration(s.st.State.Settings.IntervalMinutes) * time.Minute
	if d <= 0 {
		d = defaultInterval
	}
	return d, true
}

// runTick runs one tick synchronously. It returns false if the syncer is busy —
// a scheduled tick simply yields to the manual sync or backfill in progress.
func (s *Syncer) runTick(ctx context.Context) bool {
	if !s.begin() {
		return false
	}
	defer s.end()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick(ctx)
	return true
}

// begin claims the busy flag. It is separate from mu so TriggerSync and
// StartBackfill can refuse outright instead of queueing behind a running tick.
func (s *Syncer) begin() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

func (s *Syncer) end() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.running = false
}

func (s *Syncer) poke() {
	select {
	case s.trigger <- struct{}{}:
	default: // already pending — one wakeup is enough
	}
}

func (s *Syncer) setNextAt(t time.Time) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.nextAt = t
}

func (s *Syncer) record(at time.Time, errMsg string, fetched, uploaded int) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.lastAt, s.lastError, s.lastFetched, s.lastUploaded = at, errMsg, fetched, uploaded
}

// ── the tick ─────────────────────────────────────────────

// tick runs one fetch → merge → upload cycle. Caller holds mu.
func (s *Syncer) tick(ctx context.Context) {
	start := time.Now()
	w := &s.st.State.Withings
	if w.AccessToken == "" || w.ReconnectRequired {
		slog.Debug("sync tick skipped", "reason", "withings not connected")
		s.record(start, "", 0, 0)
		return
	}

	prevCursor := w.LastUpdate
	measurements, cursor, err := s.fetch(ctx, start)
	if err != nil {
		s.AddEvent("error", "Fetch failed: "+err.Error())
		s.record(start, err.Error(), 0, 0)
		return
	}

	// Measurements hit disk, and only then the cursor that would stop us
	// re-fetching them, before anything is uploaded.
	added := s.merge(measurements)
	if cursor > 0 {
		w.LastUpdate = cursor
	}
	if added > 0 || w.LastUpdate != prevCursor {
		s.save()
	}

	uploaded, upErr := s.uploadPending(ctx)

	errMsg := ""
	if upErr != nil {
		errMsg = upErr.Error()
		s.AddEvent("warn", "Upload stopped: "+errMsg)
	}
	if uploaded > 0 {
		s.AddEvent("info", fmt.Sprintf("Synced %d measurement%s to Garmin", uploaded, plural(uploaded)))
	}
	slog.Debug("sync tick done", "fetched", len(measurements), "new", added, "uploaded", uploaded, "took", time.Since(start))
	s.record(start, errMsg, len(measurements), uploaded)
}

// fetch pulls everything new from Withings. A 401 mid-flight is worth exactly
// one refresh + retry; a second means the fresh token is rejected too.
func (s *Syncer) fetch(ctx context.Context, start time.Time) ([]withings.Measurement, int64, error) {
	token, err := s.withingsToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	ms, cursor, err := s.getMeasures(ctx, token, start)
	if errors.Is(err, withings.ErrAuthExpired) {
		if token, err = s.refreshWithings(ctx); err != nil {
			return nil, 0, err
		}
		ms, cursor, err = s.getMeasures(ctx, token, start)
		if errors.Is(err, withings.ErrAuthExpired) {
			s.latchWithings("Withings rejected a freshly refreshed token")
		}
	}
	if err != nil {
		return nil, 0, err
	}
	return ms, cursor, nil
}

// getMeasures uses the cursor when we have one. LastUpdate == 0 should not
// happen (setup seeds it), so the fallback is a conservative 7-day window.
func (s *Syncer) getMeasures(ctx context.Context, token string, start time.Time) ([]withings.Measurement, int64, error) {
	c := s.withingsClient()
	if s.st.State.Withings.LastUpdate == 0 {
		ms, err := c.GetMeasures(ctx, token, start.Add(-fallbackWindow))
		if err != nil {
			return nil, 0, err
		}
		return ms, start.Unix(), nil
	}
	return c.GetMeasuresSince(ctx, token, s.st.State.Withings.LastUpdate)
}

// merge appends measurements we have neither uploaded nor queued, oldest
// first, to both the upload queue and the dashboard cache.
func (s *Syncer) merge(ms []withings.Measurement) int {
	if len(ms) == 0 {
		return 0
	}
	sorted := append([]withings.Measurement(nil), ms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].MeasuredAt.Before(sorted[j].MeasuredAt) })

	added := 0
	for _, m := range sorted {
		if s.known(m.GroupID) {
			continue
		}
		sm := toStore(m)
		s.st.State.Pending = append(s.st.State.Pending, sm)
		s.st.State.Recent = append(s.st.State.Recent, sm) // Synced=false
		added++
	}
	return added
}

func (s *Syncer) known(groupID int64) bool {
	for _, sy := range s.st.State.Synced {
		if sy.GroupID == groupID {
			return true
		}
	}
	for _, p := range s.st.State.Pending {
		if p.GroupID == groupID {
			return true
		}
	}
	return false
}

// uploadPending pushes the queue to Garmin, oldest first, reporting how many
// went up and the error that stopped the loop. Every accepted upload is
// persisted before the next request goes out, so a crash cannot re-send it.
// Whatever is left stays pending — the sync interval is the outer backoff.
func (s *Syncer) uploadPending(ctx context.Context) (uploaded int, err error) {
	g := &s.st.State.Garmin
	if g.AccessToken == "" || g.ReconnectRequired {
		return 0, nil // pending accumulates until Garmin is connected again
	}

	for len(s.st.State.Pending) > 0 {
		m := s.st.State.Pending[0]
		res, werr := s.uploadOne(ctx, m)
		switch res {
		case garmin.WriteOK:
			s.st.State.Pending = s.st.State.Pending[1:]
			s.st.State.Synced = append(s.st.State.Synced, store.Synced{
				GroupID: m.GroupID, MeasuredAt: m.MeasuredAt, UploadedAt: time.Now(),
			})
			s.markRecent(m.GroupID, true, "")
			uploaded++
			s.save()
		case garmin.WriteBadRequest:
			// Garmin rejects this payload every time; retrying it forever
			// would block every later measurement behind it.
			s.st.State.Pending = s.st.State.Pending[1:]
			s.markRecent(m.GroupID, false, errText(werr))
			s.save()
			s.AddEvent("error", fmt.Sprintf("Garmin rejected the weigh-in from %s: %s",
				m.MeasuredAt.Format("2006-01-02 15:04"), errText(werr)))
		default:
			return uploaded, werr
		}
	}
	return uploaded, nil
}

// uploadOne uploads a single measurement, applying the auth retry and the
// in-tick transient backoff.
func (s *Syncer) uploadOne(ctx context.Context, m store.Measurement) (garmin.WriteResult, error) {
	token, err := s.garminToken(ctx)
	if err != nil {
		return garmin.WriteAuthExpired, err
	}
	fit := garmin.EncodeWeightFITs([]garmin.Measurement{toGarmin(m)})

	res, retryAfter, werr := garmin.WriteFIT(ctx, token, fit)
	if res == garmin.WriteAuthExpired {
		if token, err = s.refreshGarmin(ctx); err != nil {
			return res, err
		}
		res, retryAfter, werr = garmin.WriteFIT(ctx, token, fit)
		if res == garmin.WriteAuthExpired {
			s.latchGarmin("Garmin rejected a freshly refreshed token")
			return res, werr
		}
	}

	// Two retries: short, then long. Both honor Retry-After when it is longer.
	for _, base := range []time.Duration{s.backoffShort, s.backoffLong} {
		if res != garmin.WriteTransient {
			break
		}
		if err := s.wait(ctx, base, retryAfter); err != nil {
			return res, err
		}
		res, retryAfter, werr = garmin.WriteFIT(ctx, token, fit)
	}
	return res, werr
}

// markRecent updates the dashboard entry for a group, newest match first, and
// reports whether one was found.
func (s *Syncer) markRecent(groupID int64, synced bool, syncErr string) bool {
	recent := s.st.State.Recent
	for i := len(recent) - 1; i >= 0; i-- {
		if recent[i].GroupID == groupID {
			recent[i].Synced = synced
			recent[i].SyncError = syncErr
			return true
		}
	}
	return false
}

func (s *Syncer) dropPending(groupID int64) {
	for i, p := range s.st.State.Pending {
		if p.GroupID == groupID {
			s.st.State.Pending = append(s.st.State.Pending[:i], s.st.State.Pending[i+1:]...)
			return
		}
	}
}

// ── token wrappers ───────────────────────────────────────

// withingsClient is built per use: the user can change their client id/secret
// in Settings at any time.
func (s *Syncer) withingsClient() *withings.Client {
	w := s.st.State.Withings
	return withings.NewClient(withings.Config{ClientID: w.ClientID, ClientSecret: w.ClientSecret})
}

// withingsToken returns a usable access token, refreshing when it is about to
// expire. Caller holds mu.
func (s *Syncer) withingsToken(ctx context.Context) (string, error) {
	w := s.st.State.Withings
	switch {
	case w.ReconnectRequired:
		return "", errWithingsReconnect
	case w.AccessToken == "" && w.RefreshToken == "":
		return "", errWithingsNotConnected
	case w.AccessToken != "" && time.Until(w.ExpiresAt) > tokenSkew:
		return w.AccessToken, nil
	}
	return s.refreshWithings(ctx)
}

// refreshWithings rotates the Withings pair and persists it BEFORE the new token
// is used. Only a definitive rejection latches reconnect_required — a timeout or
// a 5xx must not, or one night offline logs the user out.
func (s *Syncer) refreshWithings(ctx context.Context) (string, error) {
	w := &s.st.State.Withings
	if w.RefreshToken == "" {
		return "", errWithingsNotConnected
	}
	ts, err := s.withingsClient().Refresh(ctx, w.RefreshToken)
	if err != nil {
		if errors.Is(err, withings.ErrAuthExpired) {
			s.latchWithings("Withings refresh was rejected: " + err.Error())
		}
		return "", fmt.Errorf("withings refresh: %w", err)
	}

	w.AccessToken = ts.AccessToken
	w.RefreshToken = ts.RefreshToken
	w.ExpiresAt = ts.ExpiresAt
	if ts.UserID != "" {
		w.UserID = ts.UserID
	}
	if err := s.st.Save(); err != nil {
		return "", fmt.Errorf("persist refreshed withings tokens: %w", err)
	}
	slog.Debug("withings token refreshed", "expires_at", ts.ExpiresAt)
	return ts.AccessToken, nil
}

func (s *Syncer) garminToken(ctx context.Context) (string, error) {
	g := s.st.State.Garmin
	switch {
	case g.ReconnectRequired:
		return "", errGarminReconnect
	case g.AccessToken == "" && g.RefreshToken == "":
		return "", errGarminNotConnected
	case g.AccessToken != "" && time.Until(g.ExpiresAt) > tokenSkew:
		return g.AccessToken, nil
	}
	return s.refreshGarmin(ctx)
}

// refreshGarmin rotates the Garmin pair with the DI client id that originally
// worked and persists it BEFORE use: Garmin refresh tokens are single-use, so a
// rotated pair that never reaches disk locks the user out. Only invalid_grant
// latches reconnect_required; transient failures must not.
func (s *Syncer) refreshGarmin(ctx context.Context) (string, error) {
	g := &s.st.State.Garmin
	if g.RefreshToken == "" {
		return "", errGarminNotConnected
	}
	ts, err := garmin.Refresh(ctx, g.RefreshToken, g.DIClientID)
	if err != nil {
		if errors.Is(err, garmin.ErrAuthExpired) {
			s.latchGarmin("Garmin refresh was rejected: " + err.Error())
		}
		return "", fmt.Errorf("garmin refresh: %w", err)
	}

	g.AccessToken = ts.AccessToken
	g.RefreshToken = ts.RefreshToken
	g.ExpiresAt = ts.ExpiresAt
	if ts.DIClientID != "" {
		g.DIClientID = ts.DIClientID
	}
	if err := s.st.Save(); err != nil {
		return "", fmt.Errorf("persist refreshed garmin tokens: %w", err)
	}
	slog.Debug("garmin token refreshed", "expires_at", ts.ExpiresAt)
	return ts.AccessToken, nil
}

func (s *Syncer) latchWithings(reason string) {
	s.st.State.Withings.ReconnectRequired = true
	s.save()
	s.AddEvent("error", reason+" — reconnect Withings to resume syncing")
}

func (s *Syncer) latchGarmin(reason string) {
	s.st.State.Garmin.ReconnectRequired = true
	s.save()
	s.AddEvent("error", reason+" — sign in to Garmin again to resume syncing")
}

// ── small helpers ────────────────────────────────────────

func (s *Syncer) save() {
	if err := s.st.Save(); err != nil {
		s.AddEvent("error", "Could not write the state file: "+err.Error())
	}
}

// wait sleeps for the longer of base and retryAfter, capped, and returns early
// if ctx is cancelled.
func (s *Syncer) wait(ctx context.Context, base, retryAfter time.Duration) error {
	d := base
	if retryAfter > d {
		d = retryAfter
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return sleep(ctx, d)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func toStore(m withings.Measurement) store.Measurement {
	return store.Measurement{
		GroupID:           m.GroupID,
		MeasuredAt:        m.MeasuredAt,
		WeightKG:          m.WeightKG,
		BodyFatPct:        m.BodyFatPct,
		MuscleKG:          m.MuscleMassKG,
		BoneKG:            m.BoneMassKG,
		HydrationPct:      m.HydrationPct,
		BMI:               m.BMI,
		VisceralFat:       m.VisceralFat,
		BMRKcal:           m.BMRKcal,
		MetabolicAgeYears: m.MetabolicAgeYears,
	}
}

func toGarmin(m store.Measurement) garmin.Measurement {
	return garmin.Measurement{
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
	}
}

func errText(err error) string {
	if err == nil {
		return "rejected"
	}
	return err.Error()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
