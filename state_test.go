package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sensu/sensu-plugin-sdk/sensu"
)

func quietInstances(stopping ...bool) []*Instance {
	inst := &Instance{Name: "myapp_prod"}
	for i, s := range stopping {
		inst.Processes = append(inst.Processes, Process{PID: 101 + i, StartedAt: now.Add(-time.Hour), Stopping: s})
	}
	return []*Instance{inst}
}

func TestTrackQuiet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateFile)

	// No quiet process, no state: nothing written.
	if err := trackQuiet(dir, quietInstances(false, false), now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state written without quiet process: %v", err)
	}

	// First seen quiet: now.
	insts := quietInstances(false, true)
	if err := trackQuiet(dir, insts, now); err != nil {
		t.Fatal(err)
	}
	if got := insts[0].Processes[1].QuietSince; !got.Equal(now) {
		t.Errorf("QuietSince = %v, want %v", got, now)
	}
	if !insts[0].Processes[0].QuietSince.IsZero() {
		t.Errorf("QuietSince set on a running process")
	}

	// Next run: the first time is kept.
	insts = quietInstances(false, true)
	if err := trackQuiet(dir, insts, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := insts[0].Processes[1].QuietSince; !got.Equal(now) {
		t.Errorf("QuietSince = %v, want %v", got, now)
	}

	// Same PID, new process (restarted): new time.
	insts = quietInstances(false, true)
	insts[0].Processes[1].StartedAt = now
	if err := trackQuiet(dir, insts, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := insts[0].Processes[1].QuietSince; !got.Equal(now.Add(2 * time.Minute)) {
		t.Errorf("QuietSince after restart = %v", got)
	}

	// No quiet process any more: state emptied.
	if err := trackQuiet(dir, quietInstances(false, false), now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "{}" {
		t.Errorf("state = %q, want {}", b)
	}

	// Corrupted state: start over.
	os.WriteFile(path, []byte("garbage"), 0o644)
	insts = quietInstances(true)
	if err := trackQuiet(dir, insts, now); err != nil || !insts[0].Processes[0].QuietSince.Equal(now) {
		t.Errorf("corrupted state: err %v, QuietSince %v", err, insts[0].Processes[0].QuietSince)
	}
}

func TestTrackQuietUnwritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if err := trackQuiet(dir, quietInstances(false), now); err != nil {
		t.Errorf("error without quiet process: %v", err)
	}
	insts := quietInstances(true)
	err := trackQuiet(dir, insts, now)
	if err == nil || !insts[0].Processes[0].QuietSince.IsZero() {
		t.Fatalf("err %v, QuietSince %v", err, insts[0].Processes[0].QuietSince)
	}

	// No grace: the quiet process is reported, with the state issue.
	r := healthy("myapp_prod", 1)
	r.Instance.Processes[0].Stopping = true
	got := evaluate(Input{Reports: []Report{r}, StateErr: err}, th, now)
	if got.State != sensu.CheckStateWarning ||
		!strings.Contains(got.Message, "sidekiq_myapp_prod@1 quiet (stopping)\n") ||
		!strings.Contains(got.Message, "state WARNING: writing "+filepath.Join(dir, stateFile)) {
		t.Errorf("evaluate() = %d %q", got.State, got.Message)
	}
}
