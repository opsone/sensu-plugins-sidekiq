// Memory of the conditions under a grace delay between two runs: Sidekiq records neither when a
// process went quiet nor when an instance lost processes.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const stateFile = "check-sidekiq-status.json"

// Tracker remembers since when each condition (quiet process, missing processes) has been seen.
// The state keeps the conditions seen in the current run only. A nil Tracker, or one whose state is
// unusable, knows no time: no grace.
type Tracker struct {
	path       string
	now        time.Time
	seen, next map[string]int64
	err        error
	used       bool
}

// LoadTracker reads the state file of dir. A missing or corrupted file is an empty state.
func LoadTracker(dir string, now time.Time) *Tracker {
	t := &Tracker{path: filepath.Join(dir, stateFile), now: now, seen: map[string]int64{}, next: map[string]int64{}}
	b, err := os.ReadFile(t.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		t.err = fmt.Errorf("reading %s: %w", t.path, err)
	default:
		if json.Unmarshal(b, &t.seen) != nil {
			t.seen = map[string]int64{} // corrupted: start over
		}
	}
	return t
}

// Since returns when the condition was first seen, now for a new one, zero if unknown.
func (t *Tracker) Since(key string) time.Time {
	if t == nil {
		return time.Time{}
	}
	t.used = true
	if t.err != nil {
		return time.Time{}
	}
	s, ok := t.seen[key]
	if !ok {
		s = t.now.Unix()
	}
	t.next[key] = s
	return time.Unix(s, 0)
}

// Err returns the state error, when the state was needed.
func (t *Tracker) Err() error {
	if t == nil || !t.used {
		return nil
	}
	return t.err
}

// Save writes the conditions seen in this run, when they changed. On failure the tracker knows no
// time any more: the evaluation is to be done again, without grace.
func (t *Tracker) Save() error {
	if t == nil || t.err != nil || maps.Equal(t.seen, t.next) {
		return nil
	}
	if err := writeState(t.path, t.next); err != nil {
		t.err = err
		return err
	}
	return nil
}

// quietKey identifies a quiet process: its PID and start time, so that a reused PID is a new process.
func quietKey(p Process) string {
	return "quiet:" + strconv.Itoa(p.PID) + ":" + strconv.FormatInt(p.StartedAt.Unix(), 10)
}

// missingKey identifies an instance running fewer processes than required, none included.
func missingKey(instance string) string {
	return "missing:" + instance
}

// writeState replaces the state file atomically.
func writeState(path string, state map[string]int64) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), stateFile+".*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
