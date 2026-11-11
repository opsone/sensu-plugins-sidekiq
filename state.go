// Memory of the quiet processes between two runs: Sidekiq does not record when a process went quiet.
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

// quietKey identifies a process across runs: its PID and start time, so that a reused PID is a new process.
func quietKey(p Process) string {
	return strconv.Itoa(p.PID) + ":" + strconv.FormatInt(p.StartedAt.Unix(), 10)
}

// trackQuiet sets QuietSince on the quiet processes: when they were first seen quiet, now for a new
// one. The state file in dir keeps the quiet processes only. On a state error, returned only when a
// process is quiet, QuietSince stays zero (no grace).
func trackQuiet(dir string, instances []*Instance, now time.Time) error {
	var quiet []*Process
	for _, inst := range instances {
		for i := range inst.Processes {
			if inst.Processes[i].Stopping {
				quiet = append(quiet, &inst.Processes[i])
			}
		}
	}

	path := filepath.Join(dir, stateFile)
	seen := map[string]int64{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		if len(quiet) == 0 {
			return nil
		}
		return fmt.Errorf("reading %s: %w", path, err)
	default:
		if json.Unmarshal(b, &seen) != nil {
			seen = map[string]int64{} // corrupted: start over
		}
	}

	next := make(map[string]int64, len(quiet))
	for _, p := range quiet {
		k := quietKey(*p)
		if since, ok := seen[k]; ok {
			next[k] = since
		} else {
			next[k] = now.Unix()
		}
	}
	if !maps.Equal(seen, next) {
		if err := writeState(path, next); err != nil {
			if len(quiet) == 0 {
				return nil
			}
			return err
		}
	}
	for _, p := range quiet {
		p.QuietSince = time.Unix(next[quietKey(*p)], 0)
	}
	return nil
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
