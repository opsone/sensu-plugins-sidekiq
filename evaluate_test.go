package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sensu/sensu-plugin-sdk/sensu"
)

var (
	now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	th  = Thresholds{
		MinProcesses: 1, BeatMax: time.Minute, LatencyWarn: 5 * time.Minute, LatencyCrit: 15 * time.Minute,
		QueuedWarn: 100, QueuedCrit: 1000, RetryWarn: 10, RetryCrit: 100, DeadWarn: 50, DeadCrit: 500,
		BusyWarn: 80, BusyCrit: 100, RSSWarn: 800 << 20, RSSCrit: 1000 << 20,
	}
)

// healthy returns an instance of n template unit processes, up for one hour, with a fresh heartbeat
// and an empty queue.
func healthy(name string, n int) Report {
	r := Report{
		Instance: &Instance{Name: name},
		Redis:    map[int]RedisProcess{},
		Queues:   map[string]QueueStats{"default": {}, "mailers": {}},
	}
	for i := 1; i <= n; i++ {
		pid := 100 + i
		r.Instance.Processes = append(r.Instance.Processes, Process{
			PID: pid, StartedAt: now.Add(-time.Hour), RSS: 300 << 20, Version: "8.0.10",
			Tag: name + "_" + string(rune('0'+i)), Unit: "sidekiq_" + name + "@" + string(rune('0'+i)) + ".service",
			Busy: 1, Concurrency: 10,
		})
		r.Redis[pid] = RedisProcess{PID: pid, Alive: true, Beat: now.Add(-5 * time.Second), Queues: []string{"default", "mailers"}}
	}
	return r
}

func db(retry, dead int64) DBReport {
	return DBReport{Label: "127.0.0.1:6379/1", Retry: retry, Dead: dead}
}

func expect(s ...string) []Expectation {
	var es []Expectation
	for _, v := range s {
		e, err := ParseExpectation(v)
		if err != nil {
			panic(err)
		}
		es = append(es, e)
	}
	return es
}

func TestEvaluateHealthy(t *testing.T) {
	got := evaluate(Input{Expect: expect("myapp_prod:2"), Reports: []Report{healthy("myapp_prod", 2)}, DBs: []DBReport{db(2, 15)}}, th, now)
	want := "1 instance(s) OK\n" +
		"myapp_prod OK: 2 process(es) (sidekiq_myapp_prod@1,2), threads 2/20 busy, latency 0s, queued 0, rss max 300MB\n" +
		"redis 127.0.0.1:6379/1 OK: retry 2, dead 15"
	if got.State != sensu.CheckStateOK || got.Message != want {
		t.Errorf("evaluate() = %d %q, want 0 %q", got.State, got.Message, want)
	}
}

func TestEvaluateNothing(t *testing.T) {
	got := evaluate(Input{}, th, now)
	if got.State != sensu.CheckStateOK || got.Message != "no sidekiq instance found" {
		t.Errorf("evaluate() = %+v", got)
	}
	got = evaluate(Input{Expect: expect("*_prod"), Reports: []Report{{Instance: &Instance{Name: "myapp_staging"}}}}, th, now)
	if got.State != sensu.CheckStateOK || got.Message != "no sidekiq instance checked" {
		t.Errorf("evaluate() = %+v", got)
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name   string
		expect []string
		mutate func(r *Report)
		dbs    []DBReport
		issues []string
		state  int
		want   string // expected in the output
	}{
		{name: "expected not running", expect: []string{"myapp_prod", "other_prod"}, state: sensu.CheckStateCritical, want: "other_prod CRITICAL: not running"},
		{name: "pattern does not require", expect: []string{"*_prod", "other_*"}, state: sensu.CheckStateOK, want: "1 instance(s) OK"},
		{name: "missing process", expect: []string{"myapp_prod:3"}, state: sensu.CheckStateWarning, want: "myapp_prod WARNING: 2/3 process(es)"},
		{name: "count on pattern", expect: []string{"*_prod:3"}, state: sensu.CheckStateWarning, want: "2/3 process(es)"},
		{name: "global minimum", mutate: func(r *Report) { r.Instance.Processes = r.Instance.Processes[:1] }, state: sensu.CheckStateOK},
		{
			name:   "quiet",
			mutate: func(r *Report) { r.Instance.Processes[1].Stopping = true },
			state:  sensu.CheckStateWarning, want: "myapp_prod WARNING: sidekiq_myapp_prod@2 quiet (stopping)",
		},
		{
			name:   "rss",
			mutate: func(r *Report) { r.Instance.Processes[0].RSS = 900 << 20; r.Instance.Processes[1].RSS = 1100 << 20 },
			state:  sensu.CheckStateCritical, want: "sidekiq_myapp_prod@1 rss 900MB; sidekiq_myapp_prod@2 rss 1100MB",
		},
		{
			name: "busy",
			mutate: func(r *Report) {
				for i := range r.Instance.Processes {
					r.Instance.Processes[i].Busy = 9
				}
			},
			state: sensu.CheckStateWarning, want: "threads 18/20 busy (90%)",
		},
		{
			name: "saturated",
			mutate: func(r *Report) {
				for i := range r.Instance.Processes {
					r.Instance.Processes[i].Busy = 10
				}
			},
			state: sensu.CheckStateCritical, want: "threads 20/20 busy (100%)",
		},
		{
			name:   "heartbeat late",
			mutate: func(r *Report) { r.Redis[102] = RedisProcess{PID: 102, Alive: true, Beat: now.Add(-130 * time.Second)} },
			state:  sensu.CheckStateCritical, want: "sidekiq_myapp_prod@2 no heartbeat for 2m10s",
		},
		{
			name:   "heartbeat expired",
			mutate: func(r *Report) { r.Redis[102] = RedisProcess{PID: 102} },
			state:  sensu.CheckStateCritical, want: "sidekiq_myapp_prod@2 no heartbeat (expired)",
		},
		{
			name:   "not registered",
			mutate: func(r *Report) { delete(r.Redis, 102) },
			state:  sensu.CheckStateWarning, want: "sidekiq_myapp_prod@2 not registered in redis",
		},
		{
			name:   "not registered, redis unreachable",
			mutate: func(r *Report) { delete(r.Redis, 102) },
			issues: []string{"redis://10.0.0.2:6379: connection refused"},
			state:  sensu.CheckStateWarning, want: "1 redis issue(s)\nmyapp_prod OK",
		},
		{
			name: "young process not registered yet",
			mutate: func(r *Report) {
				delete(r.Redis, 102)
				r.Instance.Processes[1].StartedAt = now.Add(-10 * time.Second)
			},
			state: sensu.CheckStateOK,
		},
		{
			name: "heartbeat disabled",
			mutate: func(r *Report) {
				r.Redis = nil
			},
			state: sensu.CheckStateWarning, want: "not registered in redis",
		},
		{
			name:   "latency warning",
			mutate: func(r *Report) { r.Queues["mailers"] = QueueStats{Size: 3, Latency: 6*time.Minute + 12*time.Second} },
			state:  sensu.CheckStateWarning, want: "myapp_prod WARNING: queue mailers latency 6m12s",
		},
		{
			name:   "latency critical",
			mutate: func(r *Report) { r.Queues["default"] = QueueStats{Size: 3, Latency: time.Hour} },
			state:  sensu.CheckStateCritical, want: "queue default latency 1h0m0s",
		},
		{
			name:   "queued",
			mutate: func(r *Report) { r.Queues["default"] = QueueStats{Size: 150} },
			state:  sensu.CheckStateWarning, want: "queue default 150 job(s) queued",
		},
		{
			name:   "queues unavailable",
			mutate: func(r *Report) { r.QueueErr = errors.New("127.0.0.1:6379/1: i/o timeout") },
			state:  sensu.CheckStateWarning, want: "queues unavailable: 127.0.0.1:6379/1: i/o timeout",
		},
		{name: "retry", dbs: []DBReport{db(12, 0)}, state: sensu.CheckStateWarning, want: "1 instance(s) OK, 1 redis issue(s)\n"},
		{name: "retry critical", dbs: []DBReport{db(120, 600)}, state: sensu.CheckStateCritical, want: "redis 127.0.0.1:6379/1 CRITICAL: retry 120; dead 600"},
		{name: "dead", dbs: []DBReport{db(0, 60)}, state: sensu.CheckStateWarning, want: "redis 127.0.0.1:6379/1 WARNING: dead 60"},
		{name: "db error", dbs: []DBReport{{Label: "127.0.0.1:6379/1", Err: errors.New("timeout")}}, state: sensu.CheckStateWarning, want: "redis 127.0.0.1:6379/1 WARNING: timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := healthy("myapp_prod", 2)
			if tt.mutate != nil {
				tt.mutate(&r)
			}
			if tt.name == "heartbeat disabled" {
				// BeatMax 0 disables the Redis process checks.
				th0 := th
				th0.BeatMax = 0
				if got := evaluate(Input{Reports: []Report{r}}, th0, now); got.State != sensu.CheckStateOK {
					t.Errorf("BeatMax 0: evaluate() = %d %q", got.State, got.Message)
				}
			}
			ex := tt.expect
			if ex == nil {
				ex = []string{"myapp_prod"}
			}
			got := evaluate(Input{Expect: expect(ex...), Reports: []Report{r}, DBs: tt.dbs, Issues: tt.issues}, th, now)
			if got.State != tt.state || !strings.Contains(got.Message, tt.want) {
				t.Errorf("evaluate() = %d\n%s\nwant %d with %q", got.State, got.Message, tt.state, tt.want)
			}
		})
	}
}

func TestEvaluateSelection(t *testing.T) {
	reports := []Report{healthy("myapp_prod", 1), healthy("myapp_staging", 1), healthy("redmine", 1)}
	reports[1].Instance.Processes[0].Stopping = true // ignored: no alert

	got := evaluate(Input{Expect: expect("*_prod", "redmine"), Reports: reports}, th, now)
	want := "2 instance(s) OK\nmyapp_prod OK: 1 process(es) (sidekiq_myapp_prod@1), threads 1/10 busy, latency 0s, queued 0, rss max 300MB\n" +
		"redmine OK: 1 process(es) (sidekiq_redmine@1), threads 1/10 busy, latency 0s, queued 0, rss max 300MB"
	if got.State != sensu.CheckStateOK || got.Message != want {
		t.Errorf("evaluate() = %d\n%s\nwant\n%s", got.State, got.Message, want)
	}

	// Without --expect, every instance is checked.
	if got := evaluate(Input{Reports: reports}, th, now); got.State != sensu.CheckStateWarning ||
		!strings.Contains(got.Message, "1/3 instance(s) in trouble") {
		t.Errorf("evaluate() = %d %q", got.State, got.Message)
	}
}

func TestEvaluateIssues(t *testing.T) {
	got := evaluate(Input{Expect: expect("myapp_prod"), Reports: []Report{healthy("myapp_prod", 1)},
		Issues: []string{"redis://10.0.0.2:6379: dial tcp 10.0.0.2:6379: connection refused"}}, th, now)
	if got.State != sensu.CheckStateWarning ||
		!strings.HasSuffix(got.Message, "\nredis WARNING: redis://10.0.0.2:6379: dial tcp 10.0.0.2:6379: connection refused") {
		t.Errorf("evaluate() = %d %q", got.State, got.Message)
	}
}

func TestParseExpectation(t *testing.T) {
	for in, want := range map[string]Expectation{
		"myapp_prod":   {"myapp_prod", 0},
		"myapp_prod:4": {"myapp_prod", 4},
		"*_prod:2":     {"*_prod", 2},
	} {
		if got, err := ParseExpectation(in); err != nil || got != want {
			t.Errorf("ParseExpectation(%q) = %+v, %v, want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", ":2", "myapp_prod:", "myapp_prod:0", "myapp_prod:x", "myapp_[prod"} {
		if _, err := ParseExpectation(in); err == nil {
			t.Errorf("ParseExpectation(%q): want an error", in)
		}
	}
}

func TestUnitList(t *testing.T) {
	ps := []Process{
		{Unit: "sidekiq_goasso_prod.service"},
		{Unit: "sidekiq_goasso_searchkick_prod@1.service"},
		{Unit: "sidekiq_goasso_searchkick_prod@2.service"},
		{},
		{Unit: "sidekiq_goasso_searchkick1_prod.service"},
	}
	want := "sidekiq_goasso_prod, sidekiq_goasso_searchkick_prod@1,2, sidekiq_goasso_searchkick1_prod"
	if got := unitList(ps); got != want {
		t.Errorf("unitList() = %q, want %q", got, want)
	}
}
