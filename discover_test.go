package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const btime = 1_700_000_000

type fakeProcess struct {
	title  string
	cgroup string
}

// fakeProc writes a minimal procfs: pid → process title and cgroup.
func fakeProc(t *testing.T, procs map[int]fakeProcess) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "stat"), fmt.Sprintf("cpu  1 2 3\nbtime %d\nprocesses 42\n", btime))
	write(filepath.Join(root, "self", "stat"), "not a pid dir")
	for pid, p := range procs {
		dir := filepath.Join(root, fmt.Sprint(pid))
		// Sidekiq rewrites argv: title followed by space padding then NULs.
		write(filepath.Join(dir, "cmdline"), p.title+strings.Repeat(" ", 20)+"\x00\x00")
		// starttime (field 22) = pid seconds after boot, comm with a space and parenthesis.
		write(filepath.Join(dir, "stat"), fmt.Sprintf("%d (ruby (x)) S 1 %s %d 0", pid, strings.Repeat("0 ", 17), pid*clockTicks))
		write(filepath.Join(dir, "status"), fmt.Sprintf("Name:\truby\nVmRSS:\t   %d kB\nThreads:\t5\n", pid))
		if p.cgroup != "" {
			write(filepath.Join(dir, "cgroup"), p.cgroup)
		}
	}
	return root
}

func cg(unit string) string { return "0::/system.slice/" + unit + "\n" }

func TestDiscover(t *testing.T) {
	root := fakeProc(t, map[int]fakeProcess{
		100: {"sidekiq 8.0.10 procivis_prod_1 [3 of 10 busy]",
			"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-sidekiq_procivis_prod.slice/sidekiq_procivis_prod@1.service\n"},
		101: {"sidekiq 8.0.10 procivis_prod_2 [0 of 10 busy] stopping",
			"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-sidekiq_procivis_prod.slice/sidekiq_procivis_prod@2.service\n"},
		110: {"sidekiq 8.0.10 procivis_prod_10 [0 of 10 busy]",
			"0::/user.slice/user-1000.slice/user@1000.service/app.slice/sidekiq_procivis_prod@10.service\n"},
		200: {"sidekiq 7.0.9 goasso_searchkick_1_prod [0 of 5 busy]", cg("sidekiq_goasso_searchkick_prod@1.service")},
		201: {"sidekiq 7.0.9 goasso_prod [1 of 20 busy]", cg("sidekiq_goasso_prod.service")},
		202: {"sidekiq 7.0.9 goasso_prod [0 of 20 busy]", cg("sidekiq_goasso_searchkick1_prod.service")},
		// cgroup v1
		300: {"sidekiq 6.5.12 myapp_staging [0 of 5 busy]", "12:pids:/system.slice/x.service\n1:name=systemd:/system.slice/sidekiq_myapp_staging.service\n"},
		400: {"sidekiq 7.3.9 [0 of 5 busy]", ""},
		500: {"/usr/sbin/nginx -g daemon off;", ""},
		600: {"bundle exec sidekiq -g myapp_prod", ""}, // booting, title not set yet
	})

	got, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	p := func(pid, busy, conc int, version, tag, unit string, stopping bool) Process {
		return Process{PID: pid, StartedAt: time.Unix(btime+int64(pid), 0), RSS: uint64(pid) * 1024,
			Version: version, Tag: tag, Unit: unit, Busy: busy, Concurrency: conc, Stopping: stopping}
	}
	want := []*Instance{
		{Name: "goasso_prod", Processes: []Process{
			p(201, 1, 20, "7.0.9", "goasso_prod", "sidekiq_goasso_prod.service", false),
			p(202, 0, 20, "7.0.9", "goasso_prod", "sidekiq_goasso_searchkick1_prod.service", false),
		}},
		{Name: "goasso_searchkick_prod", Processes: []Process{
			p(200, 0, 5, "7.0.9", "goasso_searchkick_1_prod", "sidekiq_goasso_searchkick_prod@1.service", false),
		}},
		{Name: "myapp_staging", Processes: []Process{
			p(300, 0, 5, "6.5.12", "myapp_staging", "sidekiq_myapp_staging.service", false),
		}},
		{Name: "procivis_prod", Processes: []Process{
			p(100, 3, 10, "8.0.10", "procivis_prod_1", "sidekiq_procivis_prod@1.service", false),
			p(101, 0, 10, "8.0.10", "procivis_prod_2", "sidekiq_procivis_prod@2.service", true),
			p(110, 0, 10, "8.0.10", "procivis_prod_10", "sidekiq_procivis_prod@10.service", false),
		}},
		{Name: "untagged", Processes: []Process{p(400, 0, 5, "7.3.9", "", "", false)}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discover() =\n%+v\nwant\n%+v", deref(got), deref(want))
	}
}

func TestDiscoverNoProc(t *testing.T) {
	if _, err := Discover(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("Discover() on a missing procfs: want an error")
	}
}

func TestInstanceName(t *testing.T) {
	for _, tt := range []struct{ tag, unit, want string }{
		{"procivis_prod_1", "sidekiq_procivis_prod@1.service", "procivis_prod"},
		{"rubypayeur_searchkick_12_prod", "sidekiq_rubypayeur_searchkick_prod@12.service", "rubypayeur_searchkick_prod"},
		{"app_1_prod_1", "sidekiq_app@1.service", "app_1_prod"}, // last segment only
		{"myapp_prod", "sidekiq_myapp_prod@1.service", "myapp_prod"},
		{"myapp_prod", "sidekiq_myapp_prod.service", "myapp_prod"},
		{"myapp_prod_1", "", "myapp_prod_1"},
		{"1", "sidekiq@1.service", "1"},
		{"", "sidekiq_x.service", "untagged"},
	} {
		if got := InstanceName(tt.tag, tt.unit); got != tt.want {
			t.Errorf("InstanceName(%q, %q) = %q, want %q", tt.tag, tt.unit, got, tt.want)
		}
	}
}

func deref(is []*Instance) []Instance {
	out := make([]Instance, len(is))
	for i, in := range is {
		out[i] = *in
	}
	return out
}
