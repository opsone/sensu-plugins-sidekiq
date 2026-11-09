package main

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sensu/sensu-plugin-sdk/sensu"
)

// Thresholds of the check. A zero value disables the corresponding test.
type Thresholds struct {
	MinProcesses int
	BeatMax      time.Duration
	LatencyWarn  time.Duration
	LatencyCrit  time.Duration
	QueuedWarn   int64
	QueuedCrit   int64
	RetryWarn    int64
	RetryCrit    int64
	DeadWarn     int64
	DeadCrit     int64
	BusyWarn     int // % of busy threads
	BusyCrit     int
	RSSWarn      uint64 // bytes, per process
	RSSCrit      uint64
}

// Expectation is an --expect entry: an instance name or pattern, with an optional minimum number of
// processes (name:N).
type Expectation struct {
	Pattern string
	Min     int // 0: --min-processes
}

// ParseExpectation parses "name" or "name:N".
func ParseExpectation(s string) (Expectation, error) {
	pattern, n, found := strings.Cut(s, ":")
	e := Expectation{Pattern: pattern}
	if _, err := path.Match(pattern, ""); err != nil || pattern == "" {
		return e, fmt.Errorf("invalid pattern %q", s)
	}
	if found {
		min, err := strconv.Atoi(n)
		if err != nil || min < 1 {
			return e, fmt.Errorf("invalid process count in %q", s)
		}
		e.Min = min
	}
	return e, nil
}

func (e Expectation) wildcard() bool { return strings.ContainsAny(e.Pattern, `*?[\`) }

func (e Expectation) match(name string) bool {
	ok, _ := path.Match(e.Pattern, name)
	return ok
}

// Report gathers what is known about one instance before evaluation.
type Report struct {
	Instance *Instance
	Redis    map[int]RedisProcess // by PID, processes registered in Redis
	Queues   map[string]QueueStats
	QueueErr error
}

// DBReport describes a Redis database used by the checked processes.
type DBReport struct {
	Label       string
	Retry, Dead int64
	Err         error
}

// Input is everything the evaluation needs.
type Input struct {
	Expect  []Expectation
	Reports []Report
	DBs     []DBReport
	Issues  []string // Redis URLs unusable
}

// Result is the outcome of the evaluation.
type Result struct {
	State   int
	Message string
}

type finding struct {
	state int
	text  string
}

// Selected reports whether an instance is checked: no --expect, or a matching name or pattern.
func Selected(expect []Expectation, name string) bool {
	if len(expect) == 0 {
		return true
	}
	for _, e := range expect {
		if e.match(name) {
			return true
		}
	}
	return false
}

// minProcesses returns the process count required for an instance: the first --expect entry
// matching it with a count, or the global minimum.
func minProcesses(expect []Expectation, name string, def int) int {
	for _, e := range expect {
		if e.Min > 0 && e.match(name) {
			return e.Min
		}
	}
	return def
}

// evaluate turns the discovered instances into a check result: one summary line, one line per
// checked instance, one per Redis database, the Redis issues, then the ignored instances.
func evaluate(in Input, th Thresholds, now time.Time) Result {
	byName := map[string]Report{}
	var ignored []string
	for _, r := range in.Reports {
		if !Selected(in.Expect, r.Instance.Name) {
			ignored = append(ignored, r.Instance.Name)
			continue
		}
		byName[r.Instance.Name] = r
	}
	names := slices.Collect(maps.Keys(byName))
	for _, e := range in.Expect {
		if !e.wildcard() {
			names = append(names, e.Pattern)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	slices.Sort(ignored)

	if len(names) == 0 && len(in.Issues) == 0 {
		msg := "no sidekiq instance found"
		if len(ignored) > 0 {
			msg = "no sidekiq instance checked, ignored: " + strings.Join(ignored, ", ")
		}
		return Result{sensu.CheckStateOK, msg}
	}

	worst, inTrouble := sensu.CheckStateOK, 0
	var lines []string
	for _, name := range names {
		state, line := sensu.CheckStateCritical, name+" CRITICAL: not running"
		if r, found := byName[name]; found {
			state, line = evaluateInstance(r, minProcesses(in.Expect, name, th.MinProcesses), th, now, len(in.Issues) == 0)
		}
		worst = max(worst, state)
		if state != sensu.CheckStateOK {
			inTrouble++
		}
		lines = append(lines, line)
	}

	redisTrouble := len(in.Issues)
	for _, db := range in.DBs {
		state, line := evaluateDB(db, th)
		worst = max(worst, state)
		if state != sensu.CheckStateOK {
			redisTrouble++
		}
		lines = append(lines, line)
	}
	for _, issue := range in.Issues {
		worst = max(worst, sensu.CheckStateWarning)
		lines = append(lines, "redis WARNING: "+issue)
	}

	summary := fmt.Sprintf("%d instance(s) OK", len(names))
	if inTrouble > 0 {
		summary = fmt.Sprintf("%d/%d instance(s) in trouble", inTrouble, len(names))
	}
	if redisTrouble > 0 {
		summary += fmt.Sprintf(", %d redis issue(s)", redisTrouble)
	}
	if len(ignored) > 0 {
		lines = append(lines, "ignored: "+strings.Join(ignored, ", "))
	}
	return Result{worst, summary + "\n" + strings.Join(lines, "\n")}
}

// evaluateInstance checks one instance. redisComplete tells whether every Redis URL could be read:
// a process missing from Redis is only reported then.
func evaluateInstance(r Report, minProcs int, th Thresholds, now time.Time, redisComplete bool) (int, string) {
	inst := r.Instance
	var findings []finding
	add := func(state int, format string, args ...any) {
		findings = append(findings, finding{state, fmt.Sprintf(format, args...)})
	}

	if n := len(inst.Processes); minProcs > 0 && n < minProcs {
		add(sensu.CheckStateWarning, "%d/%d process(es)", n, minProcs)
	}

	var busy, concurrency int
	var maxRSS uint64
	for _, p := range inst.Processes {
		busy += p.Busy
		concurrency += p.Concurrency
		maxRSS = max(maxRSS, p.RSS)

		if p.Stopping {
			add(sensu.CheckStateWarning, "%s quiet (stopping)", p.Label())
		}
		switch {
		case th.RSSCrit > 0 && p.RSS >= th.RSSCrit:
			add(sensu.CheckStateCritical, "%s rss %s", p.Label(), mb(p.RSS))
		case th.RSSWarn > 0 && p.RSS >= th.RSSWarn:
			add(sensu.CheckStateWarning, "%s rss %s", p.Label(), mb(p.RSS))
		}

		// Heartbeat, every 5 (Sidekiq 6) or 10 seconds. A young process may not have beaten yet.
		if th.BeatMax <= 0 || now.Sub(p.StartedAt) < th.BeatMax {
			continue
		}
		rp, registered := r.Redis[p.PID]
		switch {
		case !registered:
			if redisComplete {
				add(sensu.CheckStateWarning, "%s not registered in redis", p.Label())
			}
		case !rp.Alive:
			add(sensu.CheckStateCritical, "%s no heartbeat (expired)", p.Label())
		case now.Sub(rp.Beat) > th.BeatMax:
			add(sensu.CheckStateCritical, "%s no heartbeat for %s", p.Label(), now.Sub(rp.Beat).Round(time.Second))
		}
	}

	if concurrency > 0 {
		ratio := busy * 100 / concurrency
		switch {
		case th.BusyCrit > 0 && ratio >= th.BusyCrit:
			add(sensu.CheckStateCritical, "threads %d/%d busy (%d%%)", busy, concurrency, ratio)
		case th.BusyWarn > 0 && ratio >= th.BusyWarn:
			add(sensu.CheckStateWarning, "threads %d/%d busy (%d%%)", busy, concurrency, ratio)
		}
	}

	summary := []string{fmt.Sprintf("%d process(es)", len(inst.Processes))}
	if units := unitList(inst.Processes); units != "" {
		summary[0] += " (" + units + ")"
	}
	summary = append(summary, fmt.Sprintf("threads %d/%d busy", busy, concurrency))

	if r.QueueErr != nil {
		add(sensu.CheckStateWarning, "queues unavailable: %v", r.QueueErr)
	} else if len(r.Queues) > 0 {
		var maxLatency time.Duration
		var queued int64
		for _, q := range slices.Sorted(maps.Keys(r.Queues)) {
			s := r.Queues[q]
			maxLatency = max(maxLatency, s.Latency)
			queued += s.Size
			switch {
			case th.LatencyCrit > 0 && s.Latency >= th.LatencyCrit:
				add(sensu.CheckStateCritical, "queue %s latency %s", q, s.Latency.Round(time.Second))
			case th.LatencyWarn > 0 && s.Latency >= th.LatencyWarn:
				add(sensu.CheckStateWarning, "queue %s latency %s", q, s.Latency.Round(time.Second))
			}
			switch {
			case th.QueuedCrit > 0 && s.Size >= th.QueuedCrit:
				add(sensu.CheckStateCritical, "queue %s %d job(s) queued", q, s.Size)
			case th.QueuedWarn > 0 && s.Size >= th.QueuedWarn:
				add(sensu.CheckStateWarning, "queue %s %d job(s) queued", q, s.Size)
			}
		}
		summary = append(summary, "latency "+maxLatency.Round(time.Second).String(), fmt.Sprintf("queued %d", queued))
	}
	if maxRSS > 0 {
		summary = append(summary, "rss max "+mb(maxRSS))
	}

	state := sensu.CheckStateOK
	texts := make([]string, 0, len(findings))
	for _, f := range findings {
		state = max(state, f.state)
		texts = append(texts, f.text)
	}
	if state == sensu.CheckStateOK {
		return state, fmt.Sprintf("%s OK: %s", inst.Name, strings.Join(summary, ", "))
	}
	return state, fmt.Sprintf("%s %s: %s", inst.Name, stateLabel(state), strings.Join(texts, "; "))
}

func evaluateDB(db DBReport, th Thresholds) (int, string) {
	prefix := "redis " + db.Label
	if db.Err != nil {
		return sensu.CheckStateWarning, fmt.Sprintf("%s WARNING: %v", prefix, db.Err)
	}
	state := sensu.CheckStateOK
	var texts []string
	for _, s := range []struct {
		name       string
		v          int64
		warn, crit int64
	}{{"retry", db.Retry, th.RetryWarn, th.RetryCrit}, {"dead", db.Dead, th.DeadWarn, th.DeadCrit}} {
		switch {
		case s.crit > 0 && s.v >= s.crit:
			state = max(state, sensu.CheckStateCritical)
			texts = append(texts, fmt.Sprintf("%s %d", s.name, s.v))
		case s.warn > 0 && s.v >= s.warn:
			state = max(state, sensu.CheckStateWarning)
			texts = append(texts, fmt.Sprintf("%s %d", s.name, s.v))
		}
	}
	if state == sensu.CheckStateOK {
		return state, fmt.Sprintf("%s OK: retry %d, dead %d", prefix, db.Retry, db.Dead)
	}
	return state, fmt.Sprintf("%s %s: %s", prefix, stateLabel(state), strings.Join(texts, "; "))
}

// unitList names the systemd units of the processes, template instances grouped:
// sidekiq_myapp_prod@1,2,3, sidekiq_myapp_mailers.
func unitList(ps []Process) string {
	var out []string
	indexes := map[string][]string{}
	for _, p := range ps {
		if p.Unit == "" {
			continue
		}
		if m := templateUnit.FindStringSubmatch(p.Unit); m != nil {
			if indexes[m[1]] == nil {
				out = append(out, m[1]+"@")
			}
			indexes[m[1]] = append(indexes[m[1]], m[2])
			continue
		}
		out = append(out, strings.TrimSuffix(p.Unit, ".service"))
	}
	for i, u := range out {
		if base, ok := strings.CutSuffix(u, "@"); ok {
			out[i] = u + strings.Join(slices.Compact(indexes[base]), ",")
		}
	}
	return strings.Join(out, ", ")
}

func mb(b uint64) string {
	return fmt.Sprintf("%dMB", b/(1024*1024))
}

func stateLabel(state int) string {
	switch state {
	case sensu.CheckStateOK:
		return "OK"
	case sensu.CheckStateWarning:
		return "WARNING"
	case sensu.CheckStateCritical:
		return "CRITICAL"
	default:
		return "UNKNOWN"
	}
}
