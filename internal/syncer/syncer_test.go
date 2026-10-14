package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/garmin"
	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/withings"
)

func TestMain(m *testing.M) {
	// The event ring mirrors to slog; keep the test output readable.
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func hoursAgo(n int) time.Time {
	return time.Now().Add(-time.Duration(n) * time.Hour).Truncate(time.Second)
}

// ── 1. happy path ────────────────────────────────────────

func TestTickHappyPath(t *testing.T) {
	h := newHarness(t)
	first, second := hoursAgo(3), hoursAgo(2)
	h.w.add(1, first, 80.5)
	h.w.add(2, second, 79.9)

	h.tick()

	accepted := h.g.accepted()
	if len(accepted) != 2 {
		t.Fatalf("uploads = %d, want 2 (one FIT per measurement)", len(accepted))
	}
	for i, want := range []uint16{8050, 7990} {
		records := parseFIT(t, accepted[i].fit)
		if len(records) != 1 {
			t.Fatalf("upload %d: %d records, want 1", i, len(records))
		}
		if records[0].weight != want {
			t.Errorf("upload %d weight = %d, want %d", i, records[0].weight, want)
		}
	}

	st := h.st.State
	if len(st.Pending) != 0 {
		t.Errorf("pending = %+v, want empty", st.Pending)
	}
	if len(st.Synced) != 2 || st.Synced[0].GroupID != 1 || st.Synced[1].GroupID != 2 {
		t.Fatalf("synced = %+v, want grpids 1,2 oldest first", st.Synced)
	}
	if len(st.Recent) != 2 || !st.Recent[0].Synced || !st.Recent[1].Synced {
		t.Fatalf("recent = %+v, want two synced entries", st.Recent)
	}
	if st.Recent[0].GroupID != 1 || st.Recent[1].GroupID != 2 {
		t.Errorf("recent order = %d,%d, want 1,2 (oldest first)", st.Recent[0].GroupID, st.Recent[1].GroupID)
	}
	if st.Withings.LastUpdate != 2000 {
		t.Errorf("cursor = %d, want 2000 (the getmeas updatetime)", st.Withings.LastUpdate)
	}

	got := h.s.Status()
	if got.Running || got.LastFetched != 2 || got.LastUploaded != 2 || got.LastError != "" {
		t.Errorf("status = %+v", got)
	}
	if got.LastAt.IsZero() {
		t.Error("status.LastAt is zero after a tick")
	}

	// Everything above must have survived to disk.
	saved := h.reopen().State
	if len(saved.Synced) != 2 || len(saved.Pending) != 0 || saved.Withings.LastUpdate != 2000 {
		t.Errorf("state file did not capture the tick: %+v", saved)
	}
}

// TestTickMapsEveryBodyCompositionField walks one weigh-in from the Withings
// wire format into the FIT bytes. The structs on that path spell the same values
// differently (MuscleMassKG vs MuscleKG), so a swap would ship bone as muscle.
func TestTickMapsEveryBodyCompositionField(t *testing.T) {
	h := newHarness(t)
	h.w.addFull(1, hoursAgo(2), 80.5, 18.25, 60.2, 3.17, 48.3, 8, 1685.5, 34)

	h.tick()

	got := parseFIT(t, h.g.accepted()[0].fit)[0]
	want := fitRecord{
		timestamp:    got.timestamp,
		weight:       8050,
		fat:          1825,
		hydration:    6000, // 48.3 kg of water ÷ 80.5 kg, not 48.3 %
		bone:         317,
		muscle:       6020,
		basalMet:     6742, // 1685.5 kcal/day × 4
		metabolicAge: 34,
		visceralFat:  8,
		bmi:          0xFFFF, // Withings has no BMI meastype
	}
	if got != want {
		t.Errorf("FIT record = %+v, want %+v", got, want)
	}

	stored := h.st.State.Recent[0]
	for _, c := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"body fat %", stored.BodyFatPct, 18.25},
		{"muscle kg", stored.MuscleKG, 60.2},
		{"bone kg", stored.BoneKG, 3.17},
		{"water kg", stored.WaterMassKG, 48.3},
		{"visceral fat", stored.VisceralFat, 8},
		{"bmr kcal", stored.BMRKcal, 1685.5},
		{"metabolic age", stored.MetabolicAgeYears, 34},
	} {
		if c.got == nil {
			t.Errorf("stored %s is nil", c.name)
			continue
		}
		if diff := *c.got - c.want; diff > 0.001 || diff < -0.001 {
			t.Errorf("stored %s = %v, want %v", c.name, *c.got, c.want)
		}
	}
}

// TestTickPersistsBeforeUploading pins the crash-safety ordering: a fetched
// weigh-in and its cursor are on disk before the upload is even attempted.
func TestTickPersistsBeforeUploading(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.g.setProbe(func() string {
		st, err := store.Open(h.dir)
		if err != nil {
			return "unreadable"
		}
		return fmt.Sprintf("cursor=%d queued=%d", st.State.Withings.LastUpdate, len(st.State.Pending))
	})

	h.tick()

	attempts := h.g.allAttempts()
	if len(attempts) != 1 {
		t.Fatalf("upload attempts = %d, want 1", len(attempts))
	}
	if want := "cursor=2000 queued=1"; attempts[0].probed != want {
		t.Errorf("state file was %q when the upload went out, want %q", attempts[0].probed, want)
	}
}

// ── 2. nothing new ───────────────────────────────────────

func TestTickNoNewData(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.tick()
	uploadsAfterFirst := len(h.g.allAttempts())

	h.tick()

	if got := len(h.g.allAttempts()); got != uploadsAfterFirst {
		t.Errorf("upload attempts = %d, want %d (nothing new to send)", got, uploadsAfterFirst)
	}
	if got := h.s.Status(); got.LastFetched != 0 || got.LastUploaded != 0 || got.LastError != "" {
		t.Errorf("status = %+v, want an empty successful tick", got)
	}
	if len(h.st.State.Synced) != 1 || len(h.st.State.Pending) != 0 {
		t.Errorf("store changed on an empty tick: %+v", h.st.State)
	}
}

// ── 3. Withings access token expired mid-flight ──────────

func TestTickWithingsAccessExpiredRefreshesAndRetries(t *testing.T) {
	h := newHarness(t)
	h.watchDisk()
	h.w.add(1, hoursAgo(2), 80)
	h.w.scriptStatuses(401) // first getmeas is rejected

	h.tick()

	if len(h.g.accepted()) != 1 {
		t.Fatalf("uploads = %d, want 1 after the refresh + retry", len(h.g.accepted()))
	}
	access, refresh := h.w.tokens()
	if h.st.State.Withings.AccessToken != access || h.st.State.Withings.RefreshToken != refresh {
		t.Errorf("store holds %q/%q, fake rotated to %q/%q",
			h.st.State.Withings.AccessToken, h.st.State.Withings.RefreshToken, access, refresh)
	}
	if refresh == "w-refresh-0" {
		t.Error("fake never rotated the refresh token")
	}
	if saved := h.reopen().State.Withings; saved.RefreshToken != refresh {
		t.Errorf("rotated refresh token was not persisted: file has %q", saved.RefreshToken)
	}
	// The retry must have gone out with the rotated pair already on disk.
	probes := h.w.probeResults()
	if len(probes) != 2 {
		t.Fatalf("getmeas calls = %d, want 2 (reject then retry)", len(probes))
	}
	if probes[1] != refresh {
		t.Errorf("state file held %q when the retry went out, want the rotated %q — persist before use",
			probes[1], refresh)
	}
	if h.st.State.Withings.ReconnectRequired {
		t.Error("a recoverable 401 must not latch reconnect_required")
	}
}

// ── 4. Garmin 401 on upload ──────────────────────────────

func TestTickGarminUploadAuthExpiredRefreshesAndRetries(t *testing.T) {
	h := newHarness(t)
	h.watchDisk()
	h.w.add(1, hoursAgo(2), 80)
	h.g.scriptStatuses(401)

	h.tick()

	attempts := h.g.allAttempts()
	if len(attempts) != 2 {
		t.Fatalf("upload attempts = %d, want 2 (401 then retry)", len(attempts))
	}
	if len(h.g.accepted()) != 1 {
		t.Fatalf("accepted uploads = %d, want 1", len(h.g.accepted()))
	}
	if got := h.g.refreshCount(); got != 1 {
		t.Errorf("garmin token calls = %d, want exactly 1", got)
	}
	access, refresh := h.g.tokens()
	if h.st.State.Garmin.AccessToken != access || h.st.State.Garmin.RefreshToken != refresh {
		t.Errorf("store holds %q/%q, fake rotated to %q/%q",
			h.st.State.Garmin.AccessToken, h.st.State.Garmin.RefreshToken, access, refresh)
	}
	if saved := h.reopen().State.Garmin; saved.RefreshToken != refresh {
		t.Errorf("rotated refresh token was not persisted: file has %q", saved.RefreshToken)
	}
	// Garmin refresh tokens are single-use: the rotated pair must be on disk
	// before the retry goes out.
	if attempts[1].probed != refresh {
		t.Errorf("state file held %q when the retry went out, want the rotated %q — persist before use",
			attempts[1].probed, refresh)
	}
	if len(h.st.State.Pending) != 0 || len(h.st.State.Synced) != 1 {
		t.Errorf("store = pending %+v synced %+v", h.st.State.Pending, h.st.State.Synced)
	}
}

// ── 5. Garmin refresh is dead ────────────────────────────

func TestTickGarminDeadRefreshLatchesAndKeepsPending(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.g.scriptStatuses(401)
	h.g.killRefresh()

	h.tick()

	if !h.st.State.Garmin.ReconnectRequired {
		t.Fatal("invalid_grant must latch garmin reconnect_required")
	}
	if len(h.st.State.Pending) != 1 {
		t.Fatalf("pending = %+v, want the measurement retained", h.st.State.Pending)
	}
	if len(h.st.State.Recent) != 1 || h.st.State.Recent[0].Synced {
		t.Errorf("recent = %+v, want one unsynced entry", h.st.State.Recent)
	}
	if h.s.Status().LastError == "" {
		t.Error("status.LastError should describe why the upload stopped")
	}
	if !h.reopen().State.Garmin.ReconnectRequired {
		t.Error("reconnect_required was not persisted")
	}
	// Garmin echoes the submitted token in error_description, and so does the
	// fake; none of it may reach the event log or the dashboard.
	if got := h.s.Status().LastError; strings.Contains(got, "g-refresh") {
		t.Errorf("status.LastError leaks token material: %q", got)
	}
	for _, e := range h.s.Events(0) {
		if strings.Contains(e.Message, "g-refresh") {
			t.Errorf("event leaks token material: %q", e.Message)
		}
	}

	// The next tick must be quiet, not an error loop.
	before := len(h.g.allAttempts())
	h.tick()
	if got := len(h.g.allAttempts()); got != before {
		t.Errorf("upload attempts = %d, want %d (garmin is latched off)", got, before)
	}
	if got := h.s.Status().LastError; got != "" {
		t.Errorf("LastError = %q, want empty on a skipped tick", got)
	}
	if len(h.st.State.Pending) != 1 {
		t.Errorf("pending = %+v, want it still queued", h.st.State.Pending)
	}
}

// ── 6. transient failures ────────────────────────────────

func TestTickTransientThenSuccessWithinTick(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.g.scriptStatuses(500)

	h.tick()

	if got := len(h.g.allAttempts()); got != 2 {
		t.Fatalf("upload attempts = %d, want 2 (500 then backoff retry)", got)
	}
	if len(h.st.State.Pending) != 0 || len(h.st.State.Synced) != 1 {
		t.Errorf("store = pending %+v synced %+v", h.st.State.Pending, h.st.State.Synced)
	}
	if got := h.s.Status().LastUploaded; got != 1 {
		t.Errorf("LastUploaded = %d, want 1", got)
	}
}

func TestTickPersistentTransientKeepsPendingUntilNextTick(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.g.scriptStatuses(500, 500, 500)

	h.tick()

	if got := len(h.g.allAttempts()); got != 3 {
		t.Fatalf("upload attempts = %d, want 3 (initial + two backoffs)", got)
	}
	if len(h.st.State.Pending) != 1 {
		t.Fatalf("pending = %+v, want the measurement retained for the next tick", h.st.State.Pending)
	}
	if h.st.State.Garmin.ReconnectRequired {
		t.Error("a 500 must never latch reconnect_required")
	}
	if h.s.Status().LastError == "" {
		t.Error("status.LastError should describe the stalled upload")
	}
	if saved := h.reopen().State; len(saved.Pending) != 1 {
		t.Errorf("pending was not persisted: %+v", saved.Pending)
	}

	// Garmin recovers; the queued measurement goes up on the next tick even
	// though Withings has nothing new.
	h.tick()

	if len(h.g.accepted()) != 1 {
		t.Fatalf("accepted uploads = %d, want 1 after recovery", len(h.g.accepted()))
	}
	if len(h.st.State.Pending) != 0 || len(h.st.State.Synced) != 1 {
		t.Errorf("store = pending %+v synced %+v", h.st.State.Pending, h.st.State.Synced)
	}
	if !h.st.State.Recent[0].Synced {
		t.Error("recent entry should be marked synced")
	}
}

// ── 7. 429 Retry-After ───────────────────────────────────

func TestTickHonorsRetryAfter(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.g.retryAfterSecs = 1
	h.g.scriptStatuses(429)

	start := time.Now()
	h.tick()
	elapsed := time.Since(start)

	if len(h.g.accepted()) != 1 {
		t.Fatalf("accepted uploads = %d, want 1 after the retry", len(h.g.accepted()))
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("tick took %v — Retry-After: 1 was ignored (backoff is 0 in tests)", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("tick took %v — waited far longer than Retry-After asked", elapsed)
	}
}

// ── 8. backfill ──────────────────────────────────────────

func TestBackfillBatchesAndDedupes(t *testing.T) {
	h := newHarness(t)
	const total = 250
	oldest := hoursAgo(total + 1)
	for i := 0; i < total; i++ {
		h.w.add(int64(i+1), oldest.Add(time.Duration(i)*time.Hour), 80+float64(i)/100)
	}

	before := time.Now().Unix()
	if !h.s.runBackfill(t.Context(), "all") {
		t.Fatal("runBackfill refused")
	}
	after := time.Now().Unix()

	accepted := h.g.accepted()
	if len(accepted) != 3 {
		t.Fatalf("uploads = %d, want 3 batches for %d measurements", len(accepted), total)
	}
	for i, want := range []int{100, 100, 50} {
		if got := len(parseFIT(t, accepted[i].fit)); got != want {
			t.Errorf("batch %d has %d records, want %d", i, got, want)
		}
	}
	// Records inside a batch must be chronological.
	firstBatch := parseFIT(t, accepted[0].fit)
	for i := 1; i < len(firstBatch); i++ {
		if firstBatch[i].timestamp <= firstBatch[i-1].timestamp {
			t.Fatalf("batch 0 record %d is not after its predecessor", i)
		}
	}

	st := h.st.State
	if len(st.Synced) != total {
		t.Fatalf("synced = %d, want %d", len(st.Synced), total)
	}
	if st.Synced[0].GroupID != 1 || st.Synced[total-1].GroupID != total {
		t.Errorf("synced not oldest first: %d … %d", st.Synced[0].GroupID, st.Synced[total-1].GroupID)
	}
	if len(st.Recent) != maxRecentMerge {
		t.Fatalf("recent = %d, want %d (the store's cap)", len(st.Recent), maxRecentMerge)
	}
	if st.Recent[len(st.Recent)-1].GroupID != total {
		t.Errorf("recent tail = grpid %d, want the newest (%d) — Recent must be oldest first",
			st.Recent[len(st.Recent)-1].GroupID, total)
	}
	for _, m := range st.Recent {
		if !m.Synced {
			t.Fatalf("recent entry %d not marked synced", m.GroupID)
		}
	}
	if st.Withings.LastUpdate < before || st.Withings.LastUpdate > after {
		t.Errorf("cursor = %d, want the backfill start (%d..%d)", st.Withings.LastUpdate, before, after)
	}
	if bf := h.s.Status().Backfill; bf.Running || bf.Done != 3 || bf.Total != 3 {
		t.Errorf("backfill status = %+v, want done 3 of 3, not running", bf)
	}
	if saved := h.reopen().State; len(saved.Synced) != total {
		t.Errorf("state file has %d synced records, want %d", len(saved.Synced), total)
	}

	// Re-running is free: everything is already in synced.
	if !h.s.runBackfill(t.Context(), "all") {
		t.Fatal("second runBackfill refused")
	}
	if got := len(h.g.accepted()); got != 3 {
		t.Errorf("uploads = %d after re-run, want 3 (all deduped)", got)
	}
}

// A backfill can cover something already sitting in the upload queue. Once the
// batch is accepted that queue entry has to go, or the next tick sends it twice.
func TestBackfillClearsQueuedDuplicates(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.st.State.Garmin.ReconnectRequired = true // park it in pending
	h.tick()
	if len(h.st.State.Pending) != 1 {
		t.Fatalf("pending = %+v, want the measurement queued", h.st.State.Pending)
	}
	h.st.State.Garmin.ReconnectRequired = false

	if !h.s.runBackfill(t.Context(), "30d") {
		t.Fatal("runBackfill refused")
	}

	if len(h.st.State.Pending) != 0 {
		t.Errorf("pending = %+v, want it cleared by the backfill", h.st.State.Pending)
	}
	uploads := len(h.g.accepted())
	h.tick()
	if got := len(h.g.accepted()); got != uploads {
		t.Errorf("uploads = %d, want %d — the weigh-in was sent twice", got, uploads)
	}
	if len(h.st.State.Synced) != 1 {
		t.Errorf("synced = %+v, want a single dedupe record", h.st.State.Synced)
	}
}

func TestBackfillTransientAbortLeavesRestForNextRun(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 150; i++ {
		h.w.add(int64(i+1), hoursAgo(200-i), 80)
	}
	// First batch succeeds, second fails twice (initial + one retry).
	h.g.scriptStatuses(200, 500, 500)
	cursorBefore := h.st.State.Withings.LastUpdate

	if !h.s.runBackfill(t.Context(), "all") {
		t.Fatal("runBackfill refused")
	}

	if got := len(h.st.State.Synced); got != 100 {
		t.Errorf("synced = %d, want the 100 that made it", got)
	}
	if len(h.st.State.Pending) != 0 {
		t.Errorf("aborted backfill must not queue anything: %+v", h.st.State.Pending)
	}
	if h.st.State.Withings.LastUpdate != cursorBefore {
		t.Errorf("cursor moved to %d on an aborted backfill (was %d)", h.st.State.Withings.LastUpdate, cursorBefore)
	}
	if bf := h.s.Status().Backfill; bf.Done != 1 || bf.Total != 2 {
		t.Errorf("backfill status = %+v, want 1 of 2", bf)
	}

	// The rest goes up on a re-run.
	if !h.s.runBackfill(t.Context(), "all") {
		t.Fatal("second runBackfill refused")
	}
	if got := len(h.st.State.Synced); got != 150 {
		t.Errorf("synced = %d after the re-run, want 150", got)
	}
}

// ── 9. Withings refresh is dead ──────────────────────────

func TestTickWithingsDeadRefreshLatchesAndBecomesNoop(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.w.killRefresh()
	h.st.State.Withings.ExpiresAt = time.Now().Add(-time.Minute) // forces a refresh

	h.tick()

	if !h.st.State.Withings.ReconnectRequired {
		t.Fatal("a rejected withings refresh must latch reconnect_required")
	}
	if measure, _ := h.w.counts(); measure != 0 {
		t.Errorf("getmeas was called %d times, want 0 — the token never became usable", measure)
	}
	if h.s.Status().LastError == "" {
		t.Error("status.LastError should describe the failed refresh")
	}

	before, _ := h.w.counts()
	h.tick()
	after, _ := h.w.counts()
	if after != before {
		t.Errorf("getmeas calls = %d, want %d — a latched tick must be a no-op", after, before)
	}
	if got := h.s.Status().LastError; got != "" {
		t.Errorf("LastError = %q, want empty on a skipped tick", got)
	}
}

func TestTickGarminTransientRefreshFailureDoesNotLatch(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.g.scriptStatuses(401)
	h.g.expectClientID("GARMIN_ROTATED_THE_IDS") // → invalid_client, not invalid_grant

	h.tick()

	if h.st.State.Garmin.ReconnectRequired {
		t.Fatal("only invalid_grant may latch garmin reconnect_required")
	}
	if len(h.st.State.Pending) != 1 {
		t.Errorf("pending = %+v, want the measurement retained", h.st.State.Pending)
	}
	if h.s.Status().LastError == "" {
		t.Error("the failure should still be visible in status")
	}
}

func TestTickTransientRefreshFailureDoesNotLatch(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.st.State.Withings.ClientSecret = "wrong" // fake answers envelope 503
	h.st.State.Withings.ExpiresAt = time.Now().Add(-time.Minute)

	h.tick()

	if h.st.State.Withings.ReconnectRequired {
		t.Fatal("a non-401 refresh failure must not latch reconnect_required")
	}
	if h.s.Status().LastError == "" {
		t.Error("the failure should still be visible in status")
	}
}

// ── 10. defensive cursor path ────────────────────────────

func TestTickCursorZeroUsesAbsoluteWindow(t *testing.T) {
	h := newHarness(t)
	h.st.State.Withings.LastUpdate = 0
	h.w.add(1, hoursAgo(2), 80)

	before := time.Now().Unix()
	h.tick()
	after := time.Now().Unix()

	form := h.w.form()
	if form.Get("startdate") == "" {
		t.Error("cursor 0 must fall back to an absolute window (startdate)")
	}
	if form.Has("lastupdate") {
		t.Errorf("lastupdate must not be sent when the cursor is 0: %v", form)
	}
	if got := h.st.State.Withings.LastUpdate; got < before || got > after {
		t.Errorf("cursor = %d, want the tick start (%d..%d)", got, before, after)
	}
	if len(h.g.accepted()) != 1 {
		t.Errorf("uploads = %d, want 1", len(h.g.accepted()))
	}
}

// ── 11. Withings API plan ────────────────────────────────

const (
	allTypesParam  = "1,6,76,77,88,170,226,227"
	coreTypesParam = "1,6,76,77,88"
)

func (h *harness) refuseAdvanced() {
	h.w.refuseTypes(withings.TypeVisceralFat, withings.TypeBMR, withings.TypeMetabolicAge)
}

func countEvents(h *harness, level, substr string) int {
	n := 0
	for _, e := range h.s.Events(0) {
		if e.Level == level && strings.Contains(e.Message, substr) {
			n++
		}
	}
	return n
}

func meastypes(forms []url.Values) []string {
	out := make([]string, len(forms))
	for i, f := range forms {
		out[i] = f.Get("meastypes")
	}
	return out
}

func TestTickPlanRefusalFallsBackToCoreTypes(t *testing.T) {
	h := newHarness(t)
	h.w.addFull(1, hoursAgo(2), 80.5, 18.25, 60.2, 3.17, 48.3, 8, 1685.5, 34)
	h.refuseAdvanced()

	h.tick()

	forms := h.w.allForms()
	if got := meastypes(forms); len(got) != 2 || got[0] != allTypesParam || got[1] != coreTypesParam {
		t.Fatalf("getmeas meastypes = %q, want exactly [all, core]", got)
	}
	for i, f := range forms {
		if got := f.Get("lastupdate"); got != "1000" {
			t.Errorf("request %d lastupdate = %q, want the same cursor (1000) on both", i, got)
		}
	}

	accepted := h.g.accepted()
	if len(accepted) != 1 {
		t.Fatalf("uploads = %d, want the weigh-in uploaded with its core values", len(accepted))
	}
	got := parseFIT(t, accepted[0].fit)[0]
	want := fitRecord{
		timestamp: got.timestamp, weight: 8050, fat: 1825, hydration: 6000, bone: 317, muscle: 6020,
		basalMet: 0xFFFF, metabolicAge: 0xFF, visceralFat: 0xFF, bmi: 0xFFFF,
	}
	if got != want {
		t.Errorf("FIT record = %+v, want %+v", got, want)
	}
	if len(h.st.State.Synced) != 1 || len(h.st.State.Pending) != 0 || h.st.State.Withings.LastUpdate != 2000 {
		t.Errorf("store = synced %+v pending %+v cursor %d", h.st.State.Synced, h.st.State.Pending, h.st.State.Withings.LastUpdate)
	}

	if _, token := h.w.counts(); token != 0 {
		t.Errorf("token calls = %d, want 0 — a plan refusal is not an auth problem", token)
	}
	if h.st.State.Withings.ReconnectRequired || h.reopen().State.Withings.ReconnectRequired {
		t.Error("a plan refusal must never latch reconnect_required")
	}
	if got := h.s.Status().LastError; got != "" {
		t.Errorf("LastError = %q, want a clean tick", got)
	}
	if n := countEvents(h, "warn", "API plan refused visceral fat"); n != 1 {
		t.Errorf("plan warnings = %d, want 1: %+v", n, h.s.Events(0))
	}
}

// The core-only reply has to be retried with the token the refresh produced,
// not the dead one — a second refresh would burn a rotation for nothing.
func TestTickPlanRefusalAfterAuthRetryReusesTheFreshToken(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.refuseAdvanced()
	h.w.scriptStatuses(401)

	h.tick()

	if got := meastypes(h.w.allForms()); len(got) != 3 || got[2] != coreTypesParam {
		t.Fatalf("getmeas meastypes = %q, want [all (401), all (277), core]", got)
	}
	if _, token := h.w.counts(); token != 1 {
		t.Errorf("token calls = %d, want exactly the one refresh the 401 needed", token)
	}
	if len(h.g.accepted()) != 1 {
		t.Errorf("uploads = %d, want 1", len(h.g.accepted()))
	}
}

func TestTickPlanRefusalIsRememberedForSixHours(t *testing.T) {
	h := newHarness(t)
	h.refuseAdvanced()
	h.tick()
	if n := len(h.w.allForms()); n != 2 {
		t.Fatalf("first tick made %d getmeas, want 2", n)
	}

	// Within 6 h: core types only, one request, no new warning.
	h.w.mu.Lock()
	h.w.updateTime = 3000 // a new weigh-in moves the server's cursor
	h.w.mu.Unlock()
	h.w.add(1, hoursAgo(1), 80)
	h.tick()
	forms := h.w.allForms()
	if got := meastypes(forms[2:]); len(got) != 1 || got[0] != coreTypesParam {
		t.Fatalf("second tick meastypes = %q, want one core-only request", got)
	}
	if len(h.g.accepted()) != 1 {
		t.Errorf("uploads = %d, want the new weigh-in synced", len(h.g.accepted()))
	}

	// After 6 h the advanced types are asked for again; still refused, so one
	// more fallback — and still a single warning in the log.
	h.s.advancedRefusedAt = time.Now().Add(-advancedRetryAfter - time.Minute)
	h.tick()
	forms = h.w.allForms()
	if got := meastypes(forms[3:]); len(got) != 2 || got[0] != allTypesParam || got[1] != coreTypesParam {
		t.Fatalf("tick after 6 h meastypes = %q, want [all, core]", got)
	}
	if time.Since(h.s.advancedRefusedAt) > time.Minute {
		t.Errorf("advancedRefusedAt = %v, want it renewed by the new refusal", h.s.advancedRefusedAt)
	}
	if n := countEvents(h, "warn", "API plan refused"); n != 1 {
		t.Errorf("plan warnings = %d, want 1 — events only when the state flips", n)
	}

	// The plan now allows them: one all-types request clears the flag, once.
	h.w.refuseTypes()
	h.s.advancedRefusedAt = time.Now().Add(-advancedRetryAfter - time.Minute)
	h.tick()
	h.tick()
	forms = h.w.allForms()
	if got := meastypes(forms[5:]); len(got) != 2 || got[0] != allTypesParam || got[1] != allTypesParam {
		t.Fatalf("ticks after the plan changed: meastypes = %q, want [all, all]", got)
	}
	if !h.s.advancedRefusedAt.IsZero() {
		t.Error("a full success must clear advancedRefusedAt")
	}
	if n := countEvents(h, "info", "allows visceral fat"); n != 1 {
		t.Errorf("recovery events = %d, want 1: %+v", n, h.s.Events(0))
	}
}

func TestTickPlanRefusingCoreTypesTooIsAFetchError(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.w.refuseTypes(withings.TypeWeight, withings.TypeVisceralFat)

	h.tick()

	if measure, token := h.w.counts(); measure != 2 || token != 0 {
		t.Errorf("getmeas %d, token %d; want 2 getmeas (all, core) and no refresh", measure, token)
	}
	if !h.s.advancedRefusedAt.IsZero() {
		t.Error("a failed fallback must not set the flag")
	}
	if h.st.State.Withings.ReconnectRequired || h.reopen().State.Withings.ReconnectRequired {
		t.Error("a plan refusal must never latch reconnect_required")
	}
	if len(h.g.allAttempts()) != 0 || h.st.State.Withings.LastUpdate != 1000 {
		t.Errorf("nothing may move: uploads %d, cursor %d", len(h.g.allAttempts()), h.st.State.Withings.LastUpdate)
	}
	if countEvents(h, "error", "Fetch failed") != 1 || countEvents(h, "warn", "API plan") != 0 {
		t.Errorf("events = %+v, want one fetch error and no plan warning", h.s.Events(0))
	}
	if got := h.s.Status().LastError; !strings.Contains(got, "277") {
		t.Errorf("LastError = %q, want the 277 refusal", got)
	}
}

func TestBackfillPlanRefusalFallsBackToCoreTypes(t *testing.T) {
	h := newHarness(t)
	for i := range 3 {
		h.w.addFull(int64(i+1), hoursAgo(30-i), 80.5, 18.25, 60.2, 3.17, 48.3, 8, 1685.5, 34)
	}
	h.refuseAdvanced()

	if !h.s.runBackfill(t.Context(), "30d") {
		t.Fatal("runBackfill refused")
	}

	forms := h.w.allForms()
	if got := meastypes(forms); len(got) != 2 || got[0] != allTypesParam || got[1] != coreTypesParam {
		t.Fatalf("getmeas meastypes = %q, want exactly [all, core]", got)
	}
	for _, key := range []string{"startdate", "enddate"} {
		if forms[0].Get(key) == "" || forms[0].Get(key) != forms[1].Get(key) {
			t.Errorf("%s = %q then %q, want the same window on both", key, forms[0].Get(key), forms[1].Get(key))
		}
	}
	accepted := h.g.accepted()
	if len(accepted) != 1 {
		t.Fatalf("uploads = %d, want one batch", len(accepted))
	}
	for i, r := range parseFIT(t, accepted[0].fit) {
		if r.weight != 8050 || r.hydration != 6000 || r.visceralFat != 0xFF {
			t.Errorf("record %d = %+v, want core values with derived hydration, no visceral fat", i, r)
		}
	}
	if len(h.st.State.Synced) != 3 {
		t.Errorf("synced = %d, want 3", len(h.st.State.Synced))
	}
	if n := countEvents(h, "warn", "API plan refused"); n != 1 {
		t.Errorf("plan warnings = %d, want 1", n)
	}
}

func TestTickCoreOnlyRefusedInsideTheWindowKeepsTheFlag(t *testing.T) {
	h := newHarness(t)
	refusedAt := time.Now().Add(-time.Hour)
	h.s.advancedRefusedAt = refusedAt
	h.w.add(1, hoursAgo(2), 80)
	h.w.refuseTypes(withings.TypeWeight)

	h.tick()

	if got := meastypes(h.w.allForms()); len(got) != 1 || got[0] != coreTypesParam {
		t.Fatalf("getmeas meastypes = %q, want one core-only request", got)
	}
	if !h.s.advancedRefusedAt.Equal(refusedAt) {
		t.Errorf("advancedRefusedAt = %v, want it left at %v", h.s.advancedRefusedAt, refusedAt)
	}
	if _, token := h.w.counts(); token != 0 || h.st.State.Withings.ReconnectRequired {
		t.Errorf("token calls %d, reconnect %v; a plan refusal must neither refresh nor latch", token, h.st.State.Withings.ReconnectRequired)
	}
	if countEvents(h, "error", "Fetch failed") != 1 {
		t.Errorf("events = %+v, want one fetch error", h.s.Events(0))
	}
}

// ── 12. defensive data paths ─────────────────────────────

// A weigh-in FIT can't hold (here dated before 1998, as from a scale with an
// unset clock) would otherwise fail its whole batch on every re-run.
func TestBackfillSkipsUnencodableWeighIns(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, time.Unix(800000000, 0), 80) // 1995
	h.w.add(2, hoursAgo(3), 81)
	h.w.add(3, hoursAgo(2), 82)

	if !h.s.runBackfill(t.Context(), "all") {
		t.Fatal("runBackfill refused")
	}

	accepted := h.g.accepted()
	if len(accepted) != 1 {
		t.Fatalf("uploads = %d, want one batch", len(accepted))
	}
	if records := parseFIT(t, accepted[0].fit); len(records) != 2 || records[0].weight != 8100 || records[1].weight != 8200 {
		t.Errorf("batch = %+v, want the two encodable weigh-ins", records)
	}
	if got := ids(h.st.State.Recent); len(h.st.State.Synced) != 2 || len(got) != 2 {
		t.Errorf("synced %+v recent %v, want grpids 2 and 3 only", h.st.State.Synced, got)
	}
	if countEvents(h, "error", "Backfill skipped the weigh-in from 1995") != 1 {
		t.Errorf("events = %+v, want one skip naming the weigh-in", h.s.Events(0))
	}
	if countEvents(h, "info", "uploaded 2 measurements") != 1 {
		t.Errorf("events = %+v, want the backfill to complete", h.s.Events(0))
	}
}

func TestTickTokenlessRefreshKeepsStoredTokens(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	h.w.refreshWithoutTokens()
	h.st.State.Withings.ExpiresAt = time.Now().Add(-time.Minute) // forces a refresh
	before := h.st.State.Withings

	h.tick()

	for name, w := range map[string]store.Withings{"memory": h.st.State.Withings, "disk": h.reopen().State.Withings} {
		if w.AccessToken != before.AccessToken || w.RefreshToken != before.RefreshToken {
			t.Errorf("%s holds %q/%q, want the untouched %q/%q", name, w.AccessToken, w.RefreshToken,
				before.AccessToken, before.RefreshToken)
		}
		if w.ReconnectRequired {
			t.Errorf("%s: a tokenless reply must not latch reconnect_required", name)
		}
	}
	if measure, _ := h.w.counts(); measure != 0 {
		t.Errorf("getmeas calls = %d, want 0 without a usable token", measure)
	}
	if h.s.Status().LastError == "" {
		t.Error("the failed refresh should be visible in status")
	}
}

func TestTickNeverUploadsLegacyHydration(t *testing.T) {
	h := newHarness(t)
	// A pending entry exactly as a pre-v0.0.4 binary left it: hydration_pct
	// held the water mass in kg.
	path := filepath.Join(h.dir, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["pending"] = []any{map[string]any{
		"grpid": 7, "measured_at": hoursAgo(3).Format(time.RFC3339), "weight_kg": 70,
		"hydration_pct": 42, "bmi": 24.1,
	}}
	if raw, err = json.Marshal(doc); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h.st.State = h.reopen().State

	h.tick()

	accepted := h.g.accepted()
	if len(accepted) != 1 {
		t.Fatalf("uploads = %d, want the legacy weigh-in uploaded", len(accepted))
	}
	got := parseFIT(t, accepted[0].fit)[0]
	if got.weight != 7000 || got.hydration != 0xFFFF || got.bmi != 0xFFFF {
		t.Errorf("FIT record = %+v, want weight 7000 with hydration and bmi invalid", got)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "hydration_pct") || strings.Contains(string(raw), `"bmi"`) {
		t.Errorf("state file still carries the legacy keys:\n%s", raw)
	}
}

func TestTickUnencodableMeasurementIsDroppedWithoutUploading(t *testing.T) {
	h := newHarness(t)
	// Older binaries queued dateless weigh-ins; FIT would wrap the zero time
	// into 2042 or 2106.
	bad := store.Measurement{GroupID: 7, MeasuredAt: time.Unix(0, 0).UTC(), WeightKG: 70}
	h.st.State.Pending = []store.Measurement{bad}
	h.st.State.Recent = []store.Measurement{bad}
	h.w.add(1, hoursAgo(2), 80)

	h.tick()

	accepted := h.g.accepted()
	if len(h.g.allAttempts()) != 1 || len(accepted) != 1 || parseFIT(t, accepted[0].fit)[0].weight != 8000 {
		t.Fatalf("upload attempts = %d, want only the valid weigh-in sent", len(h.g.allAttempts()))
	}
	if len(h.st.State.Pending) != 0 {
		t.Errorf("pending = %+v, want the unencodable entry dropped", h.st.State.Pending)
	}
	if r := h.st.State.Recent[0]; r.GroupID != 7 || r.Synced || r.SyncError == "" {
		t.Errorf("recent = %+v, want the dropped entry marked with a reason", r)
	}
	if countEvents(h, "error", "timestamp") != 1 {
		t.Errorf("events = %+v, want one error naming the timestamp", h.s.Events(0))
	}
}

func TestWriteBatchUnencodableIsABadRequest(t *testing.T) {
	h := newHarness(t)
	res, _, err := h.s.writeBatch(t.Context(), []store.Measurement{
		{GroupID: 1, MeasuredAt: hoursAgo(3), WeightKG: 80},
		{GroupID: 2, MeasuredAt: hoursAgo(2), WeightKG: 0},
	})
	if res != garmin.WriteBadRequest || err == nil {
		t.Errorf("writeBatch = %v, %v; want WriteBadRequest and an error", res, err)
	}
	if n := len(h.g.allAttempts()); n != 0 {
		t.Errorf("upload attempts = %d, want 0", n)
	}
}

// ── merge invariants ─────────────────────────────────────

func TestMergeDedupesAndKeepsOldestFirst(t *testing.T) {
	h := newHarness(t)
	queued := hoursAgo(5)
	h.st.State.Pending = []store.Measurement{{GroupID: 7, MeasuredAt: queued, WeightKG: 80}}
	h.st.State.Synced = []store.Synced{{GroupID: 8, MeasuredAt: hoursAgo(6)}}

	added := h.s.merge([]withings.Measurement{
		{GroupID: 9, MeasuredAt: hoursAgo(1), WeightKG: 81},  // newest, listed first
		{GroupID: 7, MeasuredAt: queued, WeightKG: 80},       // already pending
		{GroupID: 8, MeasuredAt: hoursAgo(6), WeightKG: 79},  // already synced
		{GroupID: 10, MeasuredAt: hoursAgo(3), WeightKG: 82}, // new, older than 9
	})

	if added != 2 {
		t.Fatalf("added = %d, want 2", added)
	}
	want := []int64{7, 10, 9}
	if len(h.st.State.Pending) != len(want) {
		t.Fatalf("pending = %+v, want grpids %v", h.st.State.Pending, want)
	}
	for i, id := range want {
		if h.st.State.Pending[i].GroupID != id {
			t.Fatalf("pending = %v, want %v (oldest first, newest appended last)", ids(h.st.State.Pending), want)
		}
	}
	if len(h.st.State.Recent) != 2 || h.st.State.Recent[0].GroupID != 10 || h.st.State.Recent[1].GroupID != 9 {
		t.Errorf("recent = %v, want [10 9]", ids(h.st.State.Recent))
	}
}

func ids(ms []store.Measurement) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.GroupID
	}
	return out
}

// ── rejected payloads ────────────────────────────────────

func TestTickBadRequestDropsMeasurementForGood(t *testing.T) {
	h := newHarness(t)
	at := hoursAgo(2)
	h.w.add(1, at, 80)
	h.g.scriptStatuses(400)

	h.tick()

	if got := len(h.g.allAttempts()); got != 1 {
		t.Fatalf("upload attempts = %d, want 1 — a rejected payload is never retried", got)
	}
	if len(h.st.State.Pending) != 0 {
		t.Errorf("pending = %+v, want the rejected measurement dropped", h.st.State.Pending)
	}
	if len(h.st.State.Synced) != 0 {
		t.Errorf("synced = %+v, want empty", h.st.State.Synced)
	}
	if len(h.st.State.Recent) != 1 || h.st.State.Recent[0].Synced || h.st.State.Recent[0].SyncError == "" {
		t.Fatalf("recent = %+v, want one failed entry with a reason", h.st.State.Recent)
	}
	found := false
	for _, e := range h.s.Events(0) {
		if e.Level == "error" && strings.Contains(e.Message, at.Format("2006-01-02")) {
			found = true
		}
	}
	if !found {
		t.Errorf("no error event naming the rejected weigh-in's date: %+v", h.s.Events(0))
	}

	before := len(h.g.allAttempts())
	h.tick()
	if got := len(h.g.allAttempts()); got != before {
		t.Errorf("upload attempts = %d, want %d — the drop must be permanent", got, before)
	}
}

// ── connection guards ────────────────────────────────────

func TestTickWithoutGarminAccumulatesPending(t *testing.T) {
	h := newHarness(t)
	h.st.State.Garmin = store.Garmin{}
	h.w.add(1, hoursAgo(2), 80)

	h.tick()

	if got := len(h.g.allAttempts()); got != 0 {
		t.Errorf("upload attempts = %d, want 0 without garmin tokens", got)
	}
	if len(h.st.State.Pending) != 1 {
		t.Errorf("pending = %+v, want the measurement queued", h.st.State.Pending)
	}
	if got := h.s.Status(); got.LastError != "" || got.LastFetched != 1 {
		t.Errorf("status = %+v, want a clean fetch-only tick", got)
	}
	if saved := h.reopen().State; len(saved.Pending) != 1 {
		t.Errorf("pending was not persisted: %+v", saved.Pending)
	}
}

func TestTickWithoutWithingsIsANoop(t *testing.T) {
	h := newHarness(t)
	h.st.State.Withings.AccessToken = ""
	h.st.State.Withings.RefreshToken = ""

	h.tick()

	if measure, token := h.w.counts(); measure != 0 || token != 0 {
		t.Errorf("withings was contacted (%d getmeas, %d token) without tokens", measure, token)
	}
	got := h.s.Status()
	if got.LastAt.IsZero() || got.LastError != "" || got.LastFetched != 0 {
		t.Errorf("status = %+v, want a recorded no-op", got)
	}
}

// ── status, events, concurrency ──────────────────────────

func TestEventsNewestFirstAndCapped(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < maxEvents+50; i++ {
		h.s.AddEvent("info", fmt.Sprintf("event-%d", i))
	}

	all := h.s.Events(0)
	if len(all) != maxEvents {
		t.Fatalf("events = %d, want the ring capped at %d", len(all), maxEvents)
	}
	if all[0].Message != "event-149" {
		t.Errorf("first event = %q, want the newest (event-149)", all[0].Message)
	}
	if all[len(all)-1].Message != "event-50" {
		t.Errorf("last event = %q, want event-50", all[len(all)-1].Message)
	}
	if got := h.s.Events(3); len(got) != 3 || got[2].Message != "event-147" {
		t.Errorf("Events(3) = %+v", got)
	}

	h.s.AddEvent("error", "Garmin connected")
	if e := h.s.Events(1)[0]; e.Level != "error" || e.Message != "Garmin connected" || e.At.IsZero() {
		t.Errorf("event = %+v", e)
	}
}

func TestTriggerSyncRefusesWhileBusy(t *testing.T) {
	h := newHarness(t)
	h.w.add(1, hoursAgo(2), 80)
	release := h.w.blockMeasure()
	defer release()

	if !h.s.TriggerSync() {
		t.Fatal("first TriggerSync = false, want true")
	}
	if !h.s.Status().Running {
		t.Fatal("status.Running = false while a tick is in flight")
	}
	if h.s.TriggerSync() {
		t.Error("second TriggerSync = true, want false while busy")
	}
	if h.s.StartBackfill("30d") {
		t.Error("StartBackfill = true, want false while a tick is running")
	}

	release()
	waitFor(t, 5*time.Second, "the tick to finish", func() bool { return !h.s.Status().Running })
	if len(h.g.accepted()) != 1 {
		t.Errorf("uploads = %d, want 1", len(h.g.accepted()))
	}
	if h.s.Status().LastUploaded != 1 {
		t.Errorf("status = %+v, want LastUploaded 1", h.s.Status())
	}
}

func TestStartBackfillRejectsUnknownRange(t *testing.T) {
	h := newHarness(t)
	if h.s.StartBackfill("forever") {
		t.Fatal("StartBackfill(\"forever\") = true, want false")
	}
	if h.s.Status().Running {
		t.Error("a rejected range must not leave the syncer marked busy")
	}
}

func TestDoRunsUnderTheSyncLock(t *testing.T) {
	h := newHarness(t)

	if err := h.s.Do(func(st *store.Store) error {
		st.State.Settings.IntervalMinutes = 60
		return st.Save()
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := h.reopen().State.Settings.IntervalMinutes; got != 60 {
		t.Errorf("interval = %d, want 60", got)
	}

	want := errors.New("boom")
	if got := h.s.Do(func(*store.Store) error { return want }); !errors.Is(got, want) {
		t.Errorf("Do error = %v, want %v", got, want)
	}
}

// ── scheduler ────────────────────────────────────────────

func TestRunIdlesUntilSetupCompletesThenTicks(t *testing.T) {
	h := newHarness(t)
	h.st.State.SetupComplete = false
	h.st.State.Settings.IntervalMinutes = 15
	h.w.add(1, hoursAgo(2), 80)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.s.Run(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	if got := h.s.Status(); !got.NextAt.IsZero() || !got.LastAt.IsZero() {
		t.Fatalf("status = %+v, want an idle scheduler before setup completes", got)
	}

	if err := h.s.Do(func(st *store.Store) error {
		st.State.SetupComplete = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 5*time.Second, "the first tick after setup", func() bool {
		return !h.s.Status().LastAt.IsZero()
	})
	waitFor(t, 5*time.Second, "the next run to be armed", func() bool {
		return h.s.Status().NextAt.After(time.Now())
	})
	if len(h.g.accepted()) != 1 {
		t.Errorf("uploads = %d, want the scheduled tick to have synced 1", len(h.g.accepted()))
	}
	if next := h.s.Status().NextAt; next.After(time.Now().Add(16 * time.Minute)) {
		t.Errorf("NextAt = %v, further out than the 15 minute interval", next)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if !h.s.Status().NextAt.IsZero() {
		t.Error("NextAt should be cleared when the scheduler stops")
	}
}
