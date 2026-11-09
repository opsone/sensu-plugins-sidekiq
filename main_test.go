package main

import (
	"strings"
	"testing"

	"github.com/sensu/sensu-plugin-sdk/sensu"
)

func TestCheckArgs(t *testing.T) {
	valid := func() Config {
		return Config{
			RedisURLs:    []string{"redis://127.0.0.1:6379, redis://:secret@10.0.0.2:6379/1"},
			Expect:       []string{"myapp_prod:4, *_api"},
			MinProcesses: 1, BeatMax: 60, LatencyWarn: 300, LatencyCrit: 900, Timeout: 5,
		}
	}
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{name: "valid"},
		{name: "redis url required", mutate: func(c *Config) { c.RedisURLs = []string{""} }, wantErr: "--redis-url is required"},
		{name: "invalid redis url", mutate: func(c *Config) { c.RedisURLs = []string{"http://u:secret@x"} }, wantErr: "invalid --redis-url http://u:xxxxx@x"},
		{name: "invalid expect pattern", mutate: func(c *Config) { c.Expect = []string{"myapp_[prod"} }, wantErr: "invalid --expect"},
		{name: "invalid expect count", mutate: func(c *Config) { c.Expect = []string{"myapp_prod:0"} }, wantErr: "invalid process count"},
		{name: "negative threshold", mutate: func(c *Config) { c.RSSCrit = -1 }, wantErr: "--rss-crit must not be negative"},
		{name: "zero timeout", mutate: func(c *Config) { c.Timeout = 0 }, wantErr: "--timeout must be greater than 0"},
	}
	saved := plugin
	t.Cleanup(func() { plugin = saved })

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin = valid()
			if tt.mutate != nil {
				tt.mutate(&plugin)
			}
			state, err := checkArgs(nil)
			if tt.wantErr == "" {
				if err != nil || state != sensu.CheckStateOK {
					t.Fatalf("checkArgs() = %d, %v", state, err)
				}
				// Comma separated lists, as set through environment variables, are split.
				if len(plugin.RedisURLs) != 2 || len(plugin.expectations) != 2 ||
					plugin.expectations[0] != (Expectation{"myapp_prod", 4}) || plugin.expectations[1] != (Expectation{"*_api", 0}) {
					t.Errorf("urls = %q, expectations = %+v", plugin.RedisURLs, plugin.expectations)
				}
				return
			}
			if state != sensu.CheckStateUnknown || err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("checkArgs() = %d, %v, want UNKNOWN with %q", state, err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

func TestSplitList(t *testing.T) {
	got := splitList([]string{"myapp_prod, myapp_staging", "", "other_prod"})
	want := []string{"myapp_prod", "myapp_staging", "other_prod"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitList() = %v, want %v", got, want)
	}
	if got := splitList([]string{""}); got != nil {
		t.Errorf("splitList(empty) = %v, want nil", got)
	}
}
