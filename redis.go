// Sidekiq data in Redis: process heartbeats, queues, retry and dead sets (what the Web UI shows).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// RedisProcess is the heartbeat entry of a Sidekiq process (key "hostname:pid:nonce" of the
// "processes" set). The hash expires 60 seconds after the last heartbeat: a member without hash is a
// process that stopped beating (hung, or killed without cleanup).
type RedisProcess struct {
	Identity string
	PID      int
	Alive    bool // heartbeat hash present
	Beat     time.Time
	Quiet    bool
	Queues   []string
}

// QueueStats describes one queue: pending jobs and age of the oldest one.
type QueueStats struct {
	Size    int64
	Latency time.Duration
}

// Database is a Redis database holding Sidekiq data for processes of this host.
type Database struct {
	Label     string // host:port/db, without credentials
	client    *redis.Client
	processes map[int][]RedisProcess // local processes, by PID
}

var keyspaceDB = regexp.MustCompile(`(?m)^db(\d+):keys=`)

type quietLogger struct{}

func (quietLogger) Printf(context.Context, string, ...any) {}

func init() {
	redis.SetLogger(quietLogger{}) // errors are reported in the check output, not on stderr
}

// openDatabases connects to the Redis URLs and reads the heartbeats of the Sidekiq processes of
// hostname. A URL without database number is scanned: every database holding a "processes" set is
// used. Databases without process of this host are dropped. Failures are returned as issues.
func openDatabases(ctx context.Context, urls []string, hostname string, timeout time.Duration) ([]*Database, []string) {
	var dbs []*Database
	var issues []string
	for _, raw := range urls {
		found, err := openURL(ctx, raw, hostname, timeout)
		if err != nil {
			issues = append(issues, fmt.Sprintf("%s: %v", redactURL(raw), err))
		}
		dbs = append(dbs, found...)
	}
	return dbs, issues
}

func openURL(ctx context.Context, raw, hostname string, timeout time.Duration) ([]*Database, error) {
	opts, err := redis.ParseURL(raw)
	if err != nil {
		return nil, err
	}
	opts.Protocol = 2 // RESP2: no HELLO, works on every Redis version
	opts.DisableIdentity = true
	opts.MaintNotificationsConfig = &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}
	opts.MaxRetries = -1
	opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout = timeout, timeout, timeout
	opts.PoolSize = 1

	indexes := []int{opts.DB}
	if scanned(raw) {
		c := redis.NewClient(opts)
		info, err := c.Info(ctx, "keyspace").Result()
		c.Close()
		if err != nil {
			return nil, err
		}
		indexes = parseKeyspace(info)
	}

	var dbs []*Database
	var errs []error
	for _, i := range indexes {
		o := *opts
		o.DB = i
		db := &Database{Label: fmt.Sprintf("%s/%d", opts.Addr, i), client: redis.NewClient(&o)}
		if err := db.loadProcesses(ctx, hostname); err != nil {
			db.client.Close()
			errs = append(errs, fmt.Errorf("db %d: %w", i, err))
			continue
		}
		if len(db.processes) == 0 {
			db.client.Close()
			continue
		}
		dbs = append(dbs, db)
	}
	return dbs, errors.Join(errs...)
}

// scanned reports whether a Redis URL has no database number: every database is then looked at.
func scanned(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.Trim(u.Path, "/") == "" && u.Query().Get("db") == ""
}

// parseKeyspace returns the database numbers listed by INFO keyspace.
func parseKeyspace(info string) []int {
	var dbs []int
	for _, m := range keyspaceDB.FindAllStringSubmatch(info, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			dbs = append(dbs, n)
		}
	}
	slices.Sort(dbs)
	return dbs
}

// redactURL hides the password of a Redis URL.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid redis URL"
	}
	return u.Redacted()
}

func (db *Database) Close() { db.client.Close() }

func (db *Database) loadProcesses(ctx context.Context, hostname string) error {
	members, err := db.client.SMembers(ctx, "processes").Result()
	if err != nil {
		return err
	}
	var local []RedisProcess
	for _, m := range members {
		if pid, ok := parseIdentity(m, hostname); ok {
			local = append(local, RedisProcess{Identity: m, PID: pid})
		}
	}
	db.processes = map[int][]RedisProcess{}
	if len(local) == 0 {
		return nil
	}

	cmds := make([]*redis.SliceCmd, len(local))
	_, err = db.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, rp := range local {
			cmds[i] = p.HMGet(ctx, rp.Identity, "beat", "quiet", "info")
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	for i, rp := range local {
		v := cmds[i].Val()
		if len(v) == 3 {
			beat, _ := v[0].(string)
			quiet, _ := v[1].(string)
			info, _ := v[2].(string)
			rp = parseHeartbeat(rp, beat, quiet, info)
		}
		db.processes[rp.PID] = append(db.processes[rp.PID], rp)
	}
	return nil
}

// parseIdentity returns the PID of a "hostname:pid:nonce" identity belonging to hostname.
func parseIdentity(identity, hostname string) (int, bool) {
	rest, ok := strings.CutPrefix(identity, hostname+":")
	if !ok {
		return 0, false
	}
	pid, _, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(pid)
	return n, err == nil && n > 0
}

// parseHeartbeat fills a process entry from its hash fields. An entry without beat is not alive.
func parseHeartbeat(rp RedisProcess, beat, quiet, info string) RedisProcess {
	sec, err := strconv.ParseFloat(beat, 64)
	if err != nil || sec <= 0 {
		return rp
	}
	rp.Alive = true
	rp.Beat = epoch(sec)
	rp.Quiet = quiet == "true"
	var i struct {
		Queues []string `json:"queues"`
	}
	if json.Unmarshal([]byte(info), &i) == nil {
		// Sidekiq 6 repeats a queue per weight unit.
		rp.Queues = slices.Compact(slices.Sorted(slices.Values(i.Queues)))
	}
	return rp
}

// epoch converts a Sidekiq timestamp: seconds as a float, or milliseconds as an integer (Sidekiq 8).
func epoch(v float64) time.Time {
	if v > 1e11 {
		return time.UnixMilli(int64(v))
	}
	sec := math.Floor(v)
	return time.Unix(int64(sec), int64(math.Round((v-sec)*1e6))*1e3) // microsecond precision
}

// lookup returns the heartbeat entry of a local process: the live one with the latest beat, or a
// dead entry (identity left without hash), or false if the process is not registered here.
func (db *Database) lookup(pid int) (RedisProcess, bool) {
	entries := db.processes[pid]
	if len(entries) == 0 {
		return RedisProcess{}, false
	}
	best := entries[0]
	for _, e := range entries[1:] {
		if e.Alive && (!best.Alive || e.Beat.After(best.Beat)) {
			best = e
		}
	}
	return best, true
}

// queueStats returns size and latency of the given queues.
func (db *Database) queueStats(ctx context.Context, queues []string, now time.Time) (map[string]QueueStats, error) {
	lens := make([]*redis.IntCmd, len(queues))
	oldest := make([]*redis.StringSliceCmd, len(queues))
	_, err := db.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, q := range queues {
			lens[i] = p.LLen(ctx, "queue:"+q)
			oldest[i] = p.LRange(ctx, "queue:"+q, -1, -1) // jobs are pushed on the left
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	stats := make(map[string]QueueStats, len(queues))
	for i, q := range queues {
		s := QueueStats{Size: lens[i].Val()}
		if jobs := oldest[i].Val(); len(jobs) > 0 {
			s.Latency = latency(jobs[0], now)
		}
		stats[q] = s
	}
	return stats, nil
}

// latency returns the time spent in the queue by a job, from its enqueued_at.
func latency(job string, now time.Time) time.Duration {
	var j struct {
		EnqueuedAt float64 `json:"enqueued_at"`
	}
	if json.Unmarshal([]byte(job), &j) != nil || j.EnqueuedAt <= 0 {
		return 0
	}
	return max(now.Sub(epoch(j.EnqueuedAt)), 0)
}

// sets returns the size of the retry and dead sets.
func (db *Database) sets(ctx context.Context) (retry, dead int64, err error) {
	var r, d *redis.IntCmd
	_, err = db.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		r = p.ZCard(ctx, "retry")
		d = p.ZCard(ctx, "dead")
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return r.Val(), d.Val(), nil
}
