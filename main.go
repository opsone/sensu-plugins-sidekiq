package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	corev2 "github.com/sensu/core/v2"
	"github.com/sensu/sensu-plugin-sdk/sensu"
)

// Config represents the check plugin config.
type Config struct {
	sensu.PluginConfig
	RedisURLs    []string
	Expect       []string
	MinProcesses int
	BeatMax      int
	LatencyWarn  int
	LatencyCrit  int
	QueuedWarn   int
	QueuedCrit   int
	RetryWarn    int
	RetryCrit    int
	DeadWarn     int
	DeadCrit     int
	BusyWarn     int
	BusyCrit     int
	RSSWarn      int
	RSSCrit      int
	Timeout      int
	ProcRoot     string

	expectations []Expectation
}

var (
	plugin = Config{
		PluginConfig: sensu.PluginConfig{
			Name:     "check-sidekiq-status",
			Short:    "Check the health of the Sidekiq instances running on the host",
			Keyspace: "sensu.io/plugins/check-sidekiq-status/config",
		},
	}

	options = []sensu.ConfigOption{
		&sensu.SlicePluginConfigOption[string]{
			Path:      "redis-url",
			Env:       "CHECK_SIDEKIQ_REDIS_URL",
			Argument:  "redis-url",
			Shorthand: "r",
			Default:   []string{},
			Secret:    true,
			Usage:     "Redis URL of the Sidekiq data (repeatable or comma separated). Without database number (redis://127.0.0.1:6379), every database is scanned. Required",
			Value:     &plugin.RedisURLs,
		},
		&sensu.SlicePluginConfigOption[string]{
			Path:      "expect",
			Env:       "CHECK_SIDEKIQ_EXPECT",
			Argument:  "expect",
			Shorthand: "e",
			Default:   []string{},
			Usage:     "Instance to check, or pattern (*_prod), with an optional process count (myapp_prod:4) (repeatable or comma separated). A name must be running. Other instances are ignored. Default: every running instance",
			Value:     &plugin.Expect,
		},
		&sensu.PluginConfigOption[int]{
			Path: "min-processes", Env: "CHECK_SIDEKIQ_MIN_PROCESSES", Argument: "min-processes", Default: 1,
			Usage: "Minimum number of processes per instance, when --expect gives no count", Value: &plugin.MinProcesses,
		},
		&sensu.PluginConfigOption[int]{
			Path: "beat-max", Env: "CHECK_SIDEKIQ_BEAT_MAX", Argument: "beat-max", Default: 60,
			Usage: "Seconds without heartbeat in Redis before CRITICAL (0 disables the Redis process checks)", Value: &plugin.BeatMax,
		},
		&sensu.PluginConfigOption[int]{
			Path: "latency-warn", Env: "CHECK_SIDEKIQ_LATENCY_WARN", Argument: "latency-warn", Default: 300,
			Usage: "Queue latency in seconds for WARNING (0 disables)", Value: &plugin.LatencyWarn,
		},
		&sensu.PluginConfigOption[int]{
			Path: "latency-crit", Env: "CHECK_SIDEKIQ_LATENCY_CRIT", Argument: "latency-crit", Default: 900,
			Usage: "Queue latency in seconds for CRITICAL (0 disables)", Value: &plugin.LatencyCrit,
		},
		&sensu.PluginConfigOption[int]{
			Path: "queued-warn", Env: "CHECK_SIDEKIQ_QUEUED_WARN", Argument: "queued-warn", Default: 0,
			Usage: "Jobs waiting in a queue for WARNING (0 disables)", Value: &plugin.QueuedWarn,
		},
		&sensu.PluginConfigOption[int]{
			Path: "queued-crit", Env: "CHECK_SIDEKIQ_QUEUED_CRIT", Argument: "queued-crit", Default: 0,
			Usage: "Jobs waiting in a queue for CRITICAL (0 disables)", Value: &plugin.QueuedCrit,
		},
		&sensu.PluginConfigOption[int]{
			Path: "retry-warn", Env: "CHECK_SIDEKIQ_RETRY_WARN", Argument: "retry-warn", Default: 0,
			Usage: "Jobs in the retry set for WARNING (0 disables)", Value: &plugin.RetryWarn,
		},
		&sensu.PluginConfigOption[int]{
			Path: "retry-crit", Env: "CHECK_SIDEKIQ_RETRY_CRIT", Argument: "retry-crit", Default: 0,
			Usage: "Jobs in the retry set for CRITICAL (0 disables)", Value: &plugin.RetryCrit,
		},
		&sensu.PluginConfigOption[int]{
			Path: "dead-warn", Env: "CHECK_SIDEKIQ_DEAD_WARN", Argument: "dead-warn", Default: 0,
			Usage: "Jobs in the dead set for WARNING (0 disables)", Value: &plugin.DeadWarn,
		},
		&sensu.PluginConfigOption[int]{
			Path: "dead-crit", Env: "CHECK_SIDEKIQ_DEAD_CRIT", Argument: "dead-crit", Default: 0,
			Usage: "Jobs in the dead set for CRITICAL (0 disables)", Value: &plugin.DeadCrit,
		},
		&sensu.PluginConfigOption[int]{
			Path: "busy-warn", Env: "CHECK_SIDEKIQ_BUSY_WARN", Argument: "busy-warn", Default: 0,
			Usage: "Busy threads percentage, all processes of an instance, for WARNING (0 disables)", Value: &plugin.BusyWarn,
		},
		&sensu.PluginConfigOption[int]{
			Path: "busy-crit", Env: "CHECK_SIDEKIQ_BUSY_CRIT", Argument: "busy-crit", Default: 0,
			Usage: "Busy threads percentage, all processes of an instance, for CRITICAL (0 disables)", Value: &plugin.BusyCrit,
		},
		&sensu.PluginConfigOption[int]{
			Path: "rss-warn", Env: "CHECK_SIDEKIQ_RSS_WARN", Argument: "rss-warn", Default: 0,
			Usage: "Process RSS in MB for WARNING (0 disables)", Value: &plugin.RSSWarn,
		},
		&sensu.PluginConfigOption[int]{
			Path: "rss-crit", Env: "CHECK_SIDEKIQ_RSS_CRIT", Argument: "rss-crit", Default: 0,
			Usage: "Process RSS in MB for CRITICAL (0 disables)", Value: &plugin.RSSCrit,
		},
		&sensu.PluginConfigOption[int]{
			Path: "timeout", Env: "CHECK_SIDEKIQ_TIMEOUT", Argument: "timeout", Default: 5,
			Usage: "Redis timeout in seconds", Value: &plugin.Timeout,
		},
		&sensu.PluginConfigOption[string]{
			Path: "proc-root", Env: "CHECK_SIDEKIQ_PROC_ROOT", Argument: "proc-root", Default: "/proc",
			Usage: "procfs mount point", Value: &plugin.ProcRoot,
		},
	}
)

func main() {
	check := sensu.NewGoCheck(&plugin.PluginConfig, options, checkArgs, executeCheck, false)
	check.Execute()
}

func checkArgs(event *corev2.Event) (int, error) {
	for name, v := range map[string]int{
		"min-processes": plugin.MinProcesses, "beat-max": plugin.BeatMax,
		"latency-warn": plugin.LatencyWarn, "latency-crit": plugin.LatencyCrit,
		"queued-warn": plugin.QueuedWarn, "queued-crit": plugin.QueuedCrit,
		"retry-warn": plugin.RetryWarn, "retry-crit": plugin.RetryCrit,
		"dead-warn": plugin.DeadWarn, "dead-crit": plugin.DeadCrit,
		"busy-warn": plugin.BusyWarn, "busy-crit": plugin.BusyCrit,
		"rss-warn": plugin.RSSWarn, "rss-crit": plugin.RSSCrit,
	} {
		if v < 0 {
			return sensu.CheckStateUnknown, fmt.Errorf("--%s must not be negative", name)
		}
	}
	if plugin.Timeout <= 0 {
		return sensu.CheckStateUnknown, fmt.Errorf("--timeout must be greater than 0")
	}
	// Environment variables hold comma separated lists.
	plugin.RedisURLs = splitList(plugin.RedisURLs)
	plugin.Expect = splitList(plugin.Expect)
	if len(plugin.RedisURLs) == 0 {
		return sensu.CheckStateUnknown, fmt.Errorf("--redis-url is required")
	}
	for _, u := range plugin.RedisURLs {
		if !strings.HasPrefix(u, "redis://") && !strings.HasPrefix(u, "rediss://") && !strings.HasPrefix(u, "unix://") {
			return sensu.CheckStateUnknown, fmt.Errorf("invalid --redis-url %s: redis://, rediss:// or unix:// expected", redactURL(u))
		}
	}
	plugin.expectations = nil
	for _, e := range plugin.Expect {
		exp, err := ParseExpectation(e)
		if err != nil {
			return sensu.CheckStateUnknown, fmt.Errorf("invalid --expect: %w", err)
		}
		plugin.expectations = append(plugin.expectations, exp)
	}
	return sensu.CheckStateOK, nil
}

func executeCheck(event *corev2.Event) (int, error) {
	instances, err := Discover(plugin.ProcRoot)
	if err != nil {
		return sensu.CheckStateUnknown, fmt.Errorf("scanning %s: %w", plugin.ProcRoot, err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return sensu.CheckStateUnknown, fmt.Errorf("hostname: %w", err)
	}

	in := Input{Expect: plugin.expectations}
	var checked []*Instance
	for _, inst := range instances {
		if Selected(in.Expect, inst.Name) {
			checked = append(checked, inst)
		} else {
			in.Reports = append(in.Reports, Report{Instance: inst})
		}
	}
	if len(checked) > 0 {
		timeout := time.Duration(plugin.Timeout) * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		reports, dbs, issues := collect(ctx, checked, hostname, timeout, time.Now())
		in.Reports = append(in.Reports, reports...)
		in.DBs, in.Issues = dbs, issues
	}

	result := evaluate(in, thresholds(), time.Now())
	fmt.Printf("%s %s: %s\n", plugin.Name, stateLabel(result.State), result.Message)
	return result.State, nil
}

// collect reads in Redis the heartbeats of the checked processes, the queues they listen to, and the
// retry and dead sets of the databases they use.
func collect(ctx context.Context, checked []*Instance, hostname string, timeout time.Duration, now time.Time) ([]Report, []DBReport, []string) {
	dbs, issues := openDatabases(ctx, plugin.RedisURLs, hostname, timeout)
	defer func() {
		for _, db := range dbs {
			db.Close()
		}
	}()

	used := map[*Database]bool{}
	reports := make([]Report, 0, len(checked))
	for _, inst := range checked {
		r := Report{Instance: inst, Redis: map[int]RedisProcess{}}
		queues := map[*Database][]string{}
		for _, p := range inst.Processes {
			for _, db := range dbs {
				rp, ok := db.lookup(p.PID)
				if !ok {
					continue
				}
				if prev, seen := r.Redis[p.PID]; !seen || (rp.Alive && rp.Beat.After(prev.Beat)) {
					r.Redis[p.PID] = rp
				}
				used[db] = true
				queues[db] = append(queues[db], rp.Queues...)
			}
		}
		for db, qs := range queues {
			slices.Sort(qs)
			stats, err := db.queueStats(ctx, slices.Compact(qs), now)
			if err != nil {
				r.QueueErr = fmt.Errorf("%s: %w", db.Label, err)
				continue
			}
			if r.Queues == nil {
				r.Queues = map[string]QueueStats{}
			}
			for q, s := range stats {
				prev := r.Queues[q]
				r.Queues[q] = QueueStats{Size: prev.Size + s.Size, Latency: max(prev.Latency, s.Latency)}
			}
		}
		reports = append(reports, r)
	}

	var dbReports []DBReport
	for _, db := range dbs {
		if !used[db] {
			continue
		}
		retry, dead, err := db.sets(ctx)
		dbReports = append(dbReports, DBReport{Label: db.Label, Retry: retry, Dead: dead, Err: err})
	}
	return reports, dbReports, issues
}

// splitList flattens repeated and comma separated values, dropping empty items.
func splitList(values []string) []string {
	var items []string
	for _, v := range values {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				items = append(items, t)
			}
		}
	}
	return items
}

func thresholds() Thresholds {
	return Thresholds{
		MinProcesses: plugin.MinProcesses,
		BeatMax:      time.Duration(plugin.BeatMax) * time.Second,
		LatencyWarn:  time.Duration(plugin.LatencyWarn) * time.Second,
		LatencyCrit:  time.Duration(plugin.LatencyCrit) * time.Second,
		QueuedWarn:   int64(plugin.QueuedWarn),
		QueuedCrit:   int64(plugin.QueuedCrit),
		RetryWarn:    int64(plugin.RetryWarn),
		RetryCrit:    int64(plugin.RetryCrit),
		DeadWarn:     int64(plugin.DeadWarn),
		DeadCrit:     int64(plugin.DeadCrit),
		BusyWarn:     plugin.BusyWarn,
		BusyCrit:     plugin.BusyCrit,
		RSSWarn:      uint64(plugin.RSSWarn) * 1024 * 1024,
		RSSCrit:      uint64(plugin.RSSCrit) * 1024 * 1024,
	}
}
