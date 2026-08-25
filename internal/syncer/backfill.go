package syncer

// One-shot historical import: an absolute window (getmeas startdate/enddate)
// uploaded as multi-record FIT files — ten years is ~37 uploads, not ~3650.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ulfdalen/scalebridge-sync/internal/garmin"
	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/withings"
)

// backfillBatch is how many weigh-ins go into one FIT file.
const backfillBatch = 100

// StartBackfill imports history in the background. rng is "30d", "1y" or "all".
// It returns false immediately — never blocking, never queueing — if the range
// is unknown or a tick or backfill is already in flight.
func (s *Syncer) StartBackfill(rng string) bool {
	since, ok := backfillSince(rng)
	if !ok {
		return false
	}
	if !s.begin() {
		return false
	}
	s.setBackfill(BackfillStatus{Running: true})
	go func() {
		defer s.end()
		defer s.finishBackfill()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.backfill(context.Background(), since)
	}()
	return true
}

// runBackfill is the synchronous form of StartBackfill, used by tests.
func (s *Syncer) runBackfill(ctx context.Context, rng string) bool {
	since, ok := backfillSince(rng)
	if !ok {
		return false
	}
	if !s.begin() {
		return false
	}
	defer s.end()
	s.setBackfill(BackfillStatus{Running: true})
	defer s.finishBackfill()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backfill(ctx, since)
	return true
}

func backfillSince(rng string) (time.Time, bool) {
	switch rng {
	case "30d":
		return time.Now().AddDate(0, 0, -30), true
	case "1y":
		return time.Now().AddDate(-1, 0, 0), true
	case "all":
		return time.Unix(0, 0), true
	}
	return time.Time{}, false
}

// backfill fetches the window, drops what is already synced, and uploads the
// rest in batches. Caller holds mu.
func (s *Syncer) backfill(ctx context.Context, since time.Time) {
	start := time.Now()

	fetched, err := s.backfillFetch(ctx, since)
	if err != nil {
		s.AddEvent("error", "Backfill failed: "+err.Error())
		return
	}

	todo := make([]store.Measurement, 0, len(fetched))
	for _, m := range fetched {
		if s.isSynced(m.GroupID) {
			continue
		}
		todo = append(todo, toStore(m))
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i].MeasuredAt.Before(todo[j].MeasuredAt) })
	if len(todo) == 0 {
		// Cursor still moves: everything in the window is already on Garmin.
		s.st.State.Withings.LastUpdate = start.Unix()
		s.save()
		s.AddEvent("info", fmt.Sprintf("Backfill: nothing new in %d measurement(s) from Withings", len(fetched)))
		return
	}

	batches := chunk(todo, backfillBatch)
	s.setBackfill(BackfillStatus{Running: true, Total: len(batches)})

	var uploaded []store.Measurement
	var abortErr error
	for i, batch := range batches {
		if i > 0 {
			if err := s.wait(ctx, s.interBatchDelay, 0); err != nil {
				abortErr = err
				break
			}
		}
		// Retry-After only accompanies a non-OK result; writeBatch handles it.
		res, _, werr := s.writeBatch(ctx, batch)
		if res != garmin.WriteOK {
			abortErr = werr
			break
		}
		// Persist each accepted batch before the next goes out: a crash
		// mid-backfill must not re-upload what Garmin already took.
		s.recordBatch(batch)
		uploaded = append(uploaded, batch...)
		s.setBackfillDone(i + 1)
	}

	s.recordBackfill(uploaded, start, abortErr == nil)

	switch {
	case abortErr != nil:
		s.AddEvent("warn", fmt.Sprintf("Backfill stopped after %d of %d measurement(s): %s — run it again to finish the rest",
			len(uploaded), len(todo), abortErr.Error()))
	default:
		s.AddEvent("info", fmt.Sprintf("Backfill: uploaded %d measurement%s in %d batch%s",
			len(uploaded), plural(len(uploaded)), len(batches), batchPlural(len(batches))))
	}
	slog.Debug("backfill done", "uploaded", len(uploaded), "batches", len(batches), "took", time.Since(start))
}

// backfillFetch mirrors the tick's fetch: one refresh + retry on a 401.
func (s *Syncer) backfillFetch(ctx context.Context, since time.Time) ([]withings.Measurement, error) {
	token, err := s.withingsToken(ctx)
	if err != nil {
		return nil, err
	}
	ms, err := s.withingsClient().GetMeasures(ctx, token, since)
	if errors.Is(err, withings.ErrAuthExpired) {
		if token, err = s.refreshWithings(ctx); err != nil {
			return nil, err
		}
		ms, err = s.withingsClient().GetMeasures(ctx, token, since)
		if errors.Is(err, withings.ErrAuthExpired) {
			s.latchWithings("Withings rejected a freshly refreshed token")
		}
	}
	return ms, err
}

// writeBatch uploads one multi-record FIT: refresh + retry once on a 401,
// one backoff retry on a transient failure, then give up on this batch.
func (s *Syncer) writeBatch(ctx context.Context, batch []store.Measurement) (garmin.WriteResult, time.Duration, error) {
	token, err := s.garminToken(ctx)
	if err != nil {
		return garmin.WriteAuthExpired, 0, err
	}
	ms := make([]garmin.Measurement, len(batch))
	for i, m := range batch {
		ms[i] = toGarmin(m)
	}
	fit := garmin.EncodeWeightFITs(ms)

	res, retryAfter, werr := garmin.WriteFIT(ctx, token, fit)
	if res == garmin.WriteAuthExpired {
		if token, err = s.refreshGarmin(ctx); err != nil {
			return res, 0, err
		}
		res, retryAfter, werr = garmin.WriteFIT(ctx, token, fit)
		if res == garmin.WriteAuthExpired {
			s.latchGarmin("Garmin rejected a freshly refreshed token")
			return res, retryAfter, werr
		}
	}
	if res == garmin.WriteTransient {
		if err := s.wait(ctx, s.backoffShort, retryAfter); err != nil {
			return res, retryAfter, err
		}
		res, retryAfter, werr = garmin.WriteFIT(ctx, token, fit)
	}
	return res, retryAfter, werr
}

// recordBatch turns one accepted batch into dedupe records, drops those groups
// from the pending queue so the next tick skips them, and persists immediately.
// Caller holds mu.
func (s *Syncer) recordBatch(batch []store.Measurement) {
	now := time.Now()
	for _, m := range batch {
		s.st.State.Synced = append(s.st.State.Synced, store.Synced{
			GroupID: m.GroupID, MeasuredAt: m.MeasuredAt, UploadedAt: now,
		})
		s.dropPending(m.GroupID)
	}
	s.save()
}

// recordBackfill refreshes the dashboard cache with the newest uploads and moves
// the cursor — but only on a complete run, so an abort leaves the tick's window
// alone and a re-run picks up the rest.
func (s *Syncer) recordBackfill(uploaded []store.Measurement, start time.Time, complete bool) {
	if len(uploaded) == 0 && !complete {
		return
	}
	tail := uploaded
	if len(tail) > maxRecentMerge {
		tail = tail[len(tail)-maxRecentMerge:]
	}
	for _, m := range tail {
		if s.markRecent(m.GroupID, true, "") {
			continue
		}
		m.Synced = true
		s.st.State.Recent = append(s.st.State.Recent, m)
	}

	if complete {
		s.st.State.Withings.LastUpdate = start.Unix()
	}
	s.save()
}

func (s *Syncer) isSynced(groupID int64) bool {
	for _, sy := range s.st.State.Synced {
		if sy.GroupID == groupID {
			return true
		}
	}
	return false
}

func (s *Syncer) setBackfill(b BackfillStatus) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.backfillState = b
}

func (s *Syncer) setBackfillDone(done int) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.backfillState.Done = done
}

// finishBackfill clears the running flag but keeps the counts, so the dashboard
// can still show "50 of 50" after the last batch.
func (s *Syncer) finishBackfill() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.backfillState.Running = false
}

func chunk(ms []store.Measurement, size int) [][]store.Measurement {
	var out [][]store.Measurement
	for i := 0; i < len(ms); i += size {
		end := min(i+size, len(ms))
		out = append(out, ms[i:end])
	}
	return out
}

func batchPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}
