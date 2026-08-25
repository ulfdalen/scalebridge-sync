package store

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "config"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func ptr(f float64) *float64 { return &f }

func TestDefaultDir(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	if filepath.Base(dir) != "scalebridge-sync" {
		t.Errorf("DefaultDir = %q, want it to end with scalebridge-sync", dir)
	}
}

func TestOpenFreshDefaults(t *testing.T) {
	s := openTemp(t)

	if s.State.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", s.State.SchemaVersion)
	}
	if s.State.Settings.IntervalMinutes != 15 {
		t.Errorf("IntervalMinutes = %d, want 15", s.State.Settings.IntervalMinutes)
	}
	if s.State.Settings.Port != 8723 {
		t.Errorf("Port = %d, want 8723", s.State.Settings.Port)
	}
	if s.State.Settings.UpdateCheck {
		t.Error("UpdateCheck = true, want false")
	}
	if s.State.SetupComplete {
		t.Error("SetupComplete = true, want false")
	}
	if _, err := os.Stat(s.FilePath()); !os.IsNotExist(err) {
		t.Errorf("Open must not create the state file, stat err = %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := openTemp(t)

	measured := time.Date(2026, 3, 14, 6, 30, 0, 123456789, time.UTC)
	want := State{
		SchemaVersion: 1,
		SetupComplete: true,
		Settings:      Settings{IntervalMinutes: 60, Port: 9000, UpdateCheck: true},
		Withings: Withings{
			ClientID:          "cid",
			ClientSecret:      "secret",
			AccessToken:       "wat",
			RefreshToken:      "wrt",
			ExpiresAt:         measured.Add(time.Hour),
			UserID:            "12345",
			LastUpdate:        1772000000,
			ReconnectRequired: true,
		},
		Garmin: Garmin{
			Email:       "user@example.com",
			AccessToken: "gat",
			// zero ExpiresAt must survive the roundtrip too
			DIClientID:        "di-oauth2-client",
			ReconnectRequired: false,
		},
		Pending: []Measurement{{
			GroupID:      1,
			MeasuredAt:   measured,
			WeightKG:     82.35,
			BodyFatPct:   ptr(18.4),
			MuscleKG:     ptr(60.1),
			BoneKG:       ptr(3.2),
			HydrationPct: ptr(55.5),
			BMI:          ptr(24.1),
		}},
		Synced: []Synced{{
			GroupID:    1,
			MeasuredAt: measured,
			UploadedAt: measured.Add(2 * time.Minute),
		}},
		Recent: []Measurement{{
			GroupID:    2,
			MeasuredAt: measured.Add(-24 * time.Hour),
			WeightKG:   83,
			Synced:     true,
		}, {
			GroupID:    3,
			MeasuredAt: measured,
			WeightKG:   82.35,
			SyncError:  "garmin rejected the upload",
		}},
	}
	s.State = want

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reopened, err := Open(filepath.Dir(s.FilePath()))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reflect.DeepEqual(reopened.State, want) {
		t.Errorf("roundtrip mismatch\n got: %+v\nwant: %+v", reopened.State, want)
	}
}

func TestSaveKeepsPreviousGenerationAsBak(t *testing.T) {
	s := openTemp(t)

	s.State.Withings.UserID = "first"
	if err := s.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	s.State.Withings.UserID = "second"
	if err := s.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	backup, err := load(s.FilePath() + ".bak")
	if err != nil {
		t.Fatalf("load .bak: %v", err)
	}
	if backup.Withings.UserID != "first" {
		t.Errorf(".bak UserID = %q, want %q", backup.Withings.UserID, "first")
	}
	if s.State.Withings.UserID != "second" {
		t.Errorf("state UserID = %q, want %q", s.State.Withings.UserID, "second")
	}
}

func TestOpenFallsBackToBak(t *testing.T) {
	s := openTemp(t)

	s.State.Withings.UserID = "from-bak"
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Promote the good file to .bak, then wreck the primary.
	good, err := os.ReadFile(s.FilePath())
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if err := os.WriteFile(s.FilePath()+".bak", good, 0o600); err != nil {
		t.Fatalf("write .bak: %v", err)
	}
	if err := os.WriteFile(s.FilePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt state: %v", err)
	}

	reopened, err := Open(filepath.Dir(s.FilePath()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reopened.State.Withings.UserID != "from-bak" {
		t.Errorf("UserID = %q, want %q", reopened.State.Withings.UserID, "from-bak")
	}
	// The corrupt primary must be quarantined even though .bak saved the day:
	// left in place, the next Save would copy it over the good .bak.
	matches, _ := filepath.Glob(s.FilePath() + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("want the corrupt primary quarantined, got %v", matches)
	}
	if err := reopened.Save(); err != nil {
		t.Fatalf("Save after fallback: %v", err)
	}
	if bak, err := load(s.FilePath() + ".bak"); err != nil {
		t.Errorf(".bak poisoned by the first Save after a fallback: %v", err)
	} else if bak.Withings.UserID != "from-bak" {
		t.Errorf(".bak UserID = %q, want the pre-corruption generation", bak.Withings.UserID)
	}
}

func TestOpenQuarantinesCorruptState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	path := filepath.Join(dir, fileName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	garbage := []byte("\x00\x01 not json at all")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := os.WriteFile(path+".bak", []byte("also garbage"), 0o600); err != nil {
		t.Fatalf("write .bak: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.State.Settings.Port != 8723 || s.State.SchemaVersion != 1 {
		t.Errorf("want fresh defaults, got %+v", s.State)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("corrupt state.json should have been renamed away, stat err = %v", err)
	}

	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("want exactly one quarantine file, got %v (err %v)", matches, err)
	}
	kept, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read quarantine: %v", err)
	}
	if string(kept) != string(garbage) {
		t.Errorf("quarantine content = %q, want %q", kept, garbage)
	}
}

func TestSaveCapsRecentButNeverSynced(t *testing.T) {
	s := openTemp(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 1005 {
		s.State.Synced = append(s.State.Synced, Synced{
			GroupID:    int64(i),
			MeasuredAt: base.Add(time.Duration(i) * time.Minute),
			UploadedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	for i := range 35 {
		s.State.Recent = append(s.State.Recent, Measurement{
			GroupID:    int64(i),
			MeasuredAt: base.Add(time.Duration(i) * time.Hour),
			WeightKG:   80,
		})
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := Open(filepath.Dir(s.FilePath()))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, st := range []*Store{s, reopened} {
		// Synced is the dedupe set: trimming it would let a backfill re-upload
		// old measurements to Garmin as duplicates.
		if got := len(st.State.Synced); got != 1005 {
			t.Fatalf("len(Synced) = %d, want all 1005 kept", got)
		}
		if got := st.State.Synced[0].GroupID; got != 0 {
			t.Errorf("oldest Synced = %d, want 0 (nothing evicted)", got)
		}
		if got := len(st.State.Recent); got != maxRecent {
			t.Fatalf("len(Recent) = %d, want %d", got, maxRecent)
		}
		if got := st.State.Recent[0].GroupID; got != 5 {
			t.Errorf("oldest kept Recent = %d, want 5 (newest 30 of 0..34)", got)
		}
		if got := st.State.Recent[maxRecent-1].GroupID; got != 34 {
			t.Errorf("newest kept Recent = %d, want 34", got)
		}
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte(`{"schema_version":2}`), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	s, err := Open(dir)
	if err == nil {
		t.Fatalf("Open succeeded, want error; state = %+v", s.State)
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Errorf("error = %q, want it to mention that the file is newer than this binary", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("a newer state file must be left untouched, stat err = %v", statErr)
	}
}

func TestSavePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	s := openTemp(t)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(s.FilePath())
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("state.json mode = %o, want 600", got)
	}
	di, err := os.Stat(filepath.Dir(s.FilePath()))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("config dir mode = %o, want 700", got)
	}
}

func TestSaveLeavesNoTempFile(t *testing.T) {
	s := openTemp(t)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(s.FilePath() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("state.json.tmp still present, stat err = %v", err)
	}
}
