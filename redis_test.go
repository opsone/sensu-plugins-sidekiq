package main

import (
	"reflect"
	"testing"
	"time"
)

func TestParseIdentity(t *testing.T) {
	for _, tt := range []struct {
		identity string
		pid      int
		ok       bool
	}{
		{"ns3128780:1892518:5756d33e27df", 1892518, true},
		{"ns3128780-2:1892518:5756d33e27df", 0, false}, // other host sharing the prefix
		{"other:12:abc", 0, false},
		{"ns3128780:x:abc", 0, false},
		{"ns3128780:12", 0, false},
	} {
		pid, ok := parseIdentity(tt.identity, "ns3128780")
		if pid != tt.pid || ok != tt.ok {
			t.Errorf("parseIdentity(%q) = %d, %v, want %d, %v", tt.identity, pid, ok, tt.pid, tt.ok)
		}
	}
}

func TestParseHeartbeat(t *testing.T) {
	info := `{"hostname":"h","pid":12,"tag":"procivis_prod_1","concurrency":10,"queues":["mailers","default","default"]}`
	got := parseHeartbeat(RedisProcess{PID: 12}, "1794242653.5", "false", info)
	want := RedisProcess{PID: 12, Alive: true, Beat: time.Unix(1794242653, 5e8), Queues: []string{"default", "mailers"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseHeartbeat() = %+v, want %+v", got, want)
	}
	if got := parseHeartbeat(RedisProcess{PID: 12}, "true", "true", info); got.Quiet {
		t.Errorf("quiet = %v, want false", got.Quiet)
	}
	if got := parseHeartbeat(RedisProcess{PID: 12}, "", "", ""); got.Alive {
		t.Error("expired hash: want not alive")
	}
	if got := parseHeartbeat(RedisProcess{PID: 12}, "1794242653", "true", "{}"); !got.Quiet || !got.Alive {
		t.Errorf("quiet process = %+v", got)
	}
}

func TestLatency(t *testing.T) {
	now := time.Unix(1794242700, 0)
	for job, want := range map[string]time.Duration{
		`{"class":"X","enqueued_at":1794242640.25}`: 59*time.Second + 750*time.Millisecond, // Sidekiq ≤ 7: float seconds
		`{"class":"X","enqueued_at":1794242640250}`: 59*time.Second + 750*time.Millisecond, // Sidekiq 8: integer milliseconds
		`{"class":"X","enqueued_at":1794242800}`:    0,                                     // clock skew
		`{"class":"X"}`:                             0,
		`not json`:                                  0,
	} {
		if got := latency(job, now); got != want {
			t.Errorf("latency(%s) = %s, want %s", job, got, want)
		}
	}
}

func TestParseKeyspace(t *testing.T) {
	info := "# Keyspace\r\ndb10:keys=183,expires=179,avg_ttl=8\r\ndb1:keys=2715,expires=2710,avg_ttl=9\r\n"
	if got := parseKeyspace(info); !reflect.DeepEqual(got, []int{1, 10}) {
		t.Errorf("parseKeyspace() = %v", got)
	}
}

func TestScanned(t *testing.T) {
	for url, want := range map[string]bool{
		"redis://127.0.0.1:6379":          true,
		"redis://127.0.0.1:6379/":         true,
		"redis://:secret@127.0.0.1:6379":  true,
		"redis://127.0.0.1:6379/0":        false,
		"redis://127.0.0.1:6379/10":       false,
		"redis://127.0.0.1:6379/?db=2":    false,
		"unix:///run/redis/redis.sock":    false,
		"unix:///run/redis/redis.sock?db": false,
	} {
		if got := scanned(url); got != want {
			t.Errorf("scanned(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("redis://user:secret@10.0.0.1:6379/1"); got != "redis://user:xxxxx@10.0.0.1:6379/1" {
		t.Errorf("redactURL() = %q", got)
	}
}
