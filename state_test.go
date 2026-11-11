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

// tracker returns an in-memory tracker: conditions first seen ago.
func tracker(ago map[string]time.Duration) *Tracker {
	t := &Tracker{now: now, seen: map[string]int64{}, next: map[string]int64{}}
	for k, d := range ago {
		t.seen[k] = now.Add(-d).Unix()
	}
	return t
}

func TestEvaluateGrace(t *testing.T) {
	quiet := func(r *Report) { r.Instance.Processes[1].Stopping = true }
	one := func(r *Report) { r.Instance.Processes = r.Instance.Processes[:1] }
	qk := quietKey(healthy("myapp_prod", 2).Instance.Processes[1])
	tests := []struct {
		name    string
		expect  []string
		mutate  func(r *Report)
		absent  bool // instance not running
		ago     map[string]time.Duration
		noState bool
		state   int
		want    string
	}{
		{name: "quiet, new", mutate: quiet, state: sensu.CheckStateOK, want: "sidekiq_myapp_prod@2 quiet (stopping) for 0s, within grace"},
		{name: "quiet within grace", mutate: quiet, ago: map[string]time.Duration{qk: 299 * time.Second}, state: sensu.CheckStateOK, want: "quiet (stopping) for 4m59s, within grace"},
		{name: "quiet past grace", mutate: quiet, ago: map[string]time.Duration{qk: 6 * time.Minute}, state: sensu.CheckStateWarning, want: "myapp_prod WARNING: sidekiq_myapp_prod@2 quiet (stopping) for 6m0s"},
		{name: "quiet, no state", mutate: quiet, noState: true, state: sensu.CheckStateWarning, want: "myapp_prod WARNING: sidekiq_myapp_prod@2 quiet (stopping)"},
		{name: "missing, new", expect: []string{"myapp_prod:2"}, mutate: one, state: sensu.CheckStateOK, want: "1/2 process(es) for 0s, within grace"},
		{name: "missing past grace", expect: []string{"myapp_prod:2"}, mutate: one, ago: map[string]time.Duration{"missing:myapp_prod": 10 * time.Minute}, state: sensu.CheckStateWarning, want: "myapp_prod WARNING: 1/2 process(es) for 10m0s"},
		{name: "missing, no state", expect: []string{"myapp_prod:2"}, mutate: one, noState: true, state: sensu.CheckStateWarning, want: "myapp_prod WARNING: 1/2 process(es)"},
		{name: "not running, new", expect: []string{"myapp_prod"}, absent: true, state: sensu.CheckStateOK, want: "myapp_prod OK: not running for 0s, within grace"},
		{name: "not running past grace", expect: []string{"myapp_prod"}, absent: true, ago: map[string]time.Duration{"missing:myapp_prod": 5 * time.Minute}, state: sensu.CheckStateCritical, want: "myapp_prod CRITICAL: not running for 5m0s"},
		{name: "not running, no state", expect: []string{"myapp_prod"}, absent: true, noState: true, state: sensu.CheckStateCritical, want: "myapp_prod CRITICAL: not running\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := healthy("myapp_prod", 2)
			if tt.mutate != nil {
				tt.mutate(&r)
			}
			in := Input{Expect: expect(tt.expect...), Tracker: tracker(tt.ago)}
			if !tt.absent {
				in.Reports = []Report{r}
			}
			if tt.noState {
				in.Tracker.err = errors.New("no state")
			}
			got := evaluate(in, th, now)
			if got.State != tt.state || !strings.Contains(got.Message+"\n", tt.want) {
				t.Errorf("evaluate() = %d %q, want %d with %q", got.State, got.Message, tt.state, tt.want)
			}
			if tt.noState && !strings.Contains(got.Message, "state WARNING: no state (no grace)") {
				t.Errorf("state error not reported: %q", got.Message)
			}
		})
	}
}

func TestTracker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateFile)

	// Nothing seen, no state: nothing written.
	tr := LoadTracker(dir, now)
	if err := tr.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state written without condition: %v", err)
	}

	// First seen: now; the next runs keep it.
	tr = LoadTracker(dir, now)
	if got := tr.Since("missing:a"); !got.Equal(now) {
		t.Errorf("Since = %v, want %v", got, now)
	}
	if err := tr.Save(); err != nil {
		t.Fatal(err)
	}
	tr = LoadTracker(dir, now.Add(time.Minute))
	if got := tr.Since("missing:a"); !got.Equal(now) {
		t.Errorf("Since next run = %v, want %v", got, now)
	}
	tr.Since("quiet:1:2")
	if err := tr.Save(); err != nil {
		t.Fatal(err)
	}

	// Condition gone: dropped; seen again later: new time.
	tr = LoadTracker(dir, now.Add(2*time.Minute))
	tr.Since("quiet:1:2")
	tr.Save()
	tr = LoadTracker(dir, now.Add(3*time.Minute))
	if got := tr.Since("missing:a"); !got.Equal(now.Add(3 * time.Minute)) {
		t.Errorf("Since after recovery = %v", got)
	}

	// Corrupted state: start over.
	os.WriteFile(path, []byte("garbage"), 0o644)
	tr = LoadTracker(dir, now)
	if got := tr.Since("missing:a"); !got.Equal(now) || tr.Err() != nil {
		t.Errorf("corrupted state: Since %v, err %v", got, tr.Err())
	}

	// Nil tracker: no time.
	var none *Tracker
	if !none.Since("x").IsZero() || none.Err() != nil || none.Save() != nil {
		t.Error("nil tracker")
	}
}

func TestTrackerUnwritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	tr := LoadTracker(dir, now)
	if err := tr.Save(); err != nil || tr.Err() != nil {
		t.Errorf("error without condition: %v, %v", err, tr.Err())
	}

	// Within grace at first, then evaluated again without grace once the state cannot be kept.
	r := healthy("myapp_prod", 1)
	r.Instance.Processes[0].Stopping = true
	in := Input{Reports: []Report{r}, Tracker: LoadTracker(dir, now)}
	if got := evaluate(in, th, now); got.State != sensu.CheckStateOK {
		t.Errorf("first evaluate() = %d %q", got.State, got.Message)
	}
	if in.Tracker.Save() == nil {
		t.Fatal("Save() succeeded in a missing directory")
	}
	got := evaluate(in, th, now)
	if got.State != sensu.CheckStateWarning ||
		!strings.Contains(got.Message, "sidekiq_myapp_prod@1 quiet (stopping)\n") ||
		!strings.Contains(got.Message, "state WARNING: writing "+filepath.Join(dir, stateFile)) {
		t.Errorf("evaluate() = %d %q", got.State, got.Message)
	}
}
