// Package store keeps all scalebridge-sync state in one JSON file,
// <dir>/state.json. A Store is not safe for concurrent use by design: one
// syncer holds a single mutex around every read, mutation and Save. The file
// holds OAuth tokens in plaintext, 0600 in a 0700 dir - see README "Security".
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	schemaVersion = 1
	fileName      = "state.json"

	defaultIntervalMinutes = 15
	defaultPort            = 8723

	maxRecent = 30
)

var errFutureSchema = errors.New("state file is newer than this binary")

// Store owns one state file. Read and mutate State directly, then call Save.
type Store struct {
	State State

	dir string
}

type State struct {
	SchemaVersion int           `json:"schema_version"`
	SetupComplete bool          `json:"setup_complete"`
	Settings      Settings      `json:"settings"`
	Withings      Withings      `json:"withings"`
	Garmin        Garmin        `json:"garmin"`
	Pending       []Measurement `json:"pending"` // fetched from Withings, not yet uploaded
	// Synced is the dedupe set of every uploaded GroupID; never trim it, or a
	// backfill re-uploads history. Synced and Recent are both oldest first:
	// append at the end, and Save trims Recent from the front.
	Synced []Synced      `json:"synced"` // unbounded
	Recent []Measurement `json:"recent"` // dashboard cache, capped at maxRecent
}

type Settings struct {
	IntervalMinutes int  `json:"interval_minutes"`
	Port            int  `json:"port"`
	UpdateCheck     bool `json:"update_check"`
}

type Withings struct {
	ClientID          string    `json:"client_id"`
	ClientSecret      string    `json:"client_secret"`
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	ExpiresAt         time.Time `json:"expires_at"`
	UserID            string    `json:"user_id"`
	LastUpdate        int64     `json:"last_update"` // getmeas cursor, epoch seconds
	ReconnectRequired bool      `json:"reconnect_required"`
}

type Garmin struct {
	Email             string    `json:"email"` // display only
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	ExpiresAt         time.Time `json:"expires_at"`
	DIClientID        string    `json:"di_client_id"` // must be reused on refresh
	ReconnectRequired bool      `json:"reconnect_required"`
}

type Measurement struct {
	GroupID      int64     `json:"grpid"`
	MeasuredAt   time.Time `json:"measured_at"`
	WeightKG     float64   `json:"weight_kg"`
	BodyFatPct   *float64  `json:"body_fat_pct,omitempty"`
	MuscleKG     *float64  `json:"muscle_kg,omitempty"`
	BoneKG       *float64  `json:"bone_kg,omitempty"`
	HydrationPct *float64  `json:"hydration_pct,omitempty"`
	BMI          *float64  `json:"bmi,omitempty"`
	Synced       bool      `json:"synced,omitempty"`     // meaningful in Recent only
	SyncError    string    `json:"sync_error,omitempty"` // meaningful in Recent only
}

type Synced struct {
	GroupID    int64     `json:"grpid"`
	MeasuredAt time.Time `json:"measured_at"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// DefaultDir is the per-user config directory for scalebridge-sync.
func DefaultDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "scalebridge-sync"), nil
}

// Open creates dir if needed and loads its state file, falling back to the
// backup and finally to defaults. A state file from a newer binary is an error:
// that is a downgrade, not corruption, so the file is left untouched.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	s := &Store{State: newState(), dir: dir}
	path := s.FilePath()

	state, err := load(path)
	switch {
	case err == nil:
		s.State = state
		return s, nil
	case errors.Is(err, fs.ErrNotExist):
		return s, nil // first run
	case errors.Is(err, errFutureSchema):
		return nil, err
	}

	// Quarantine the unreadable primary before falling back to .bak: left in
	// place, the next Save would copy its bytes over the last good generation.
	quarantine := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if err := os.Rename(path, quarantine); err != nil {
		return nil, fmt.Errorf("quarantine unreadable state file: %w", err)
	}

	if backup, err := load(path + ".bak"); err == nil {
		s.State = backup
	}
	return s, nil
}

// FilePath is the full path of the state file.
func (s *Store) FilePath() string {
	return filepath.Join(s.dir, fileName)
}

// Save enforces the Recent cap and writes the state atomically - tmp, fsync,
// rename - keeping the previous generation as state.json.bak.
func (s *Store) Save() error {
	if n := len(s.State.Recent); n > maxRecent {
		s.State.Recent = s.State.Recent[n-maxRecent:]
	}
	s.State.SchemaVersion = schemaVersion

	path := s.FilePath()
	if previous, err := os.ReadFile(path); err == nil {
		os.WriteFile(path+".bak", previous, 0o600) // best effort
	}

	data, err := json.MarshalIndent(s.State, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	// Atomic on POSIX; MoveFileEx replace semantics on Windows.
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

func newState() State {
	return State{
		SchemaVersion: schemaVersion,
		Settings: Settings{
			IntervalMinutes: defaultIntervalMinutes,
			Port:            defaultPort,
			UpdateCheck:     false,
		},
	}
}

func load(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if state.SchemaVersion > schemaVersion {
		return State{}, fmt.Errorf("%s: %w: schema_version %d, this binary understands up to %d - upgrade scalebridge-sync",
			path, errFutureSchema, state.SchemaVersion, schemaVersion)
	}
	return state, nil
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
