// Discovery of the Sidekiq processes running on the host, from their process title.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, the unit of the process start time in /proc/<pid>/stat.
// It is 100 on every mainstream Linux architecture (no sysconf without CGO).
const clockTicks = 100

// untagged names the instance of the Sidekiq processes started without tag.
const untagged = "untagged"

var (
	// sidekiq 7.3.9 myapp_prod [3 of 10 busy]
	// sidekiq 7.3.9 myapp_prod [0 of 10 busy] stopping
	// Sidekiq rewrites its title at each heartbeat; before the first one, the title is the command line.
	title = regexp.MustCompile(`^sidekiq (\S+) (?:(\S+) )?\[(\d+) of (\d+) busy\]( stopping)?$`)
	// sidekiq_myapp_prod@2.service: instance of a systemd template unit.
	templateUnit = regexp.MustCompile(`^(.+)@([^@]+)\.service$`)
)

// Process is a Sidekiq process seen in /proc.
type Process struct {
	PID         int
	StartedAt   time.Time
	RSS         uint64 // bytes
	Version     string
	Tag         string // as set with -g, "" if none
	Unit        string // systemd unit, "" if none
	Busy        int
	Concurrency int
	Stopping    bool // quiet (TSTP received), no new job fetched
}

// Label names the process in messages: its systemd unit, or its PID.
func (p Process) Label() string {
	if p.Unit != "" {
		return strings.TrimSuffix(p.Unit, ".service")
	}
	return fmt.Sprintf("pid %d", p.PID)
}

// Instance groups the Sidekiq processes of one application environment.
type Instance struct {
	Name      string
	Processes []Process
}

// InstanceName derives the instance of a process from its tag. The processes of a systemd template
// unit (name@N.service) carry the index in their tag (-g myapp_prod_%i): the "_N" segment is removed,
// wherever it is, so that they form one instance (myapp_prod_2 → myapp_prod,
// myapp_searchkick_2_prod → myapp_searchkick_prod).
func InstanceName(tag, unit string) string {
	if tag == "" {
		return untagged
	}
	m := templateUnit.FindStringSubmatch(unit)
	if m == nil {
		return tag
	}
	parts := strings.Split(tag, "_")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == m[2] && len(parts) > 1 {
			return strings.Join(append(parts[:i:i], parts[i+1:]...), "_")
		}
	}
	return tag
}

// Discover scans procRoot (normally /proc) for Sidekiq processes, grouped by instance and sorted.
// Processes vanishing during the scan are ignored.
func Discover(procRoot string) ([]*Instance, error) {
	bootTime, err := readBootTime(procRoot)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}

	byName := map[string]*Instance{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		t, err := readTitle(procRoot, pid)
		if err != nil {
			continue
		}
		m := title.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		p, err := readProcess(procRoot, pid, bootTime)
		if err != nil {
			continue
		}
		p.Version, p.Tag, p.Stopping = m[1], m[2], m[5] != ""
		p.Busy, _ = strconv.Atoi(m[3])
		p.Concurrency, _ = strconv.Atoi(m[4])
		p.Unit = readUnit(procRoot, pid)

		name := InstanceName(p.Tag, p.Unit)
		if byName[name] == nil {
			byName[name] = &Instance{Name: name}
		}
		byName[name].Processes = append(byName[name].Processes, p)
	}

	instances := make([]*Instance, 0, len(byName))
	for _, inst := range byName {
		sort.Slice(inst.Processes, func(i, j int) bool {
			a, b := inst.Processes[i], inst.Processes[j]
			if a.Unit != b.Unit {
				return unitLess(a.Unit, b.Unit)
			}
			return a.PID < b.PID
		})
		instances = append(instances, inst)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Name < instances[j].Name })
	return instances, nil
}

// unitLess sorts the instances of a template unit by numeric index: x@2 before x@10.
func unitLess(a, b string) bool {
	ma, mb := templateUnit.FindStringSubmatch(a), templateUnit.FindStringSubmatch(b)
	if ma != nil && mb != nil && ma[1] == mb[1] {
		ia, errA := strconv.Atoi(ma[2])
		ib, errB := strconv.Atoi(mb[2])
		if errA == nil && errB == nil {
			return ia < ib
		}
	}
	return a < b
}

// readTitle returns the process title as set by Sidekiq (argv rewritten, NUL separated and padded).
func readTitle(procRoot string, pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "", err
	}
	b = bytes.ReplaceAll(b, []byte{0}, []byte{' '})
	return string(bytes.TrimRight(b, " ")), nil
}

// readUnit returns the systemd service of the process from its cgroup, "" if none. The last
// ".service" component wins: user units live under user@<uid>.service.
func readUnit(procRoot string, pid int) string {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	unit := ""
	for line := range strings.SplitSeq(string(b), "\n") {
		// cgroup v2: "0::/path"; v1: "<id>:name=systemd:/path", the other hierarchies are ignored.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || (parts[1] != "" && parts[1] != "name=systemd") {
			continue
		}
		for c := range strings.SplitSeq(parts[2], "/") {
			if strings.HasSuffix(c, ".service") {
				unit = c
			}
		}
		if unit != "" {
			return unit
		}
	}
	return unit
}

func readProcess(procRoot string, pid int, bootTime time.Time) (Process, error) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))

	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return Process{}, err
	}
	// The command name (field 2) may contain spaces: fields are counted after its closing parenthesis.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return Process{}, fmt.Errorf("malformed %s/stat", dir)
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 20 {
		return Process{}, fmt.Errorf("malformed %s/stat", dir)
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64) // field 22: starttime
	if err != nil {
		return Process{}, fmt.Errorf("malformed %s/stat: %w", dir, err)
	}
	started := bootTime.Add(time.Duration(ticks) * time.Second / clockTicks)

	rss, err := readRSS(filepath.Join(dir, "status"))
	if err != nil {
		return Process{}, err
	}
	return Process{PID: pid, StartedAt: started, RSS: rss}, nil
}

func readRSS(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "VmRSS:"); ok {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("malformed VmRSS in %s: %w", path, err)
			}
			return kb * 1024, nil
		}
	}
	return 0, s.Err() // no VmRSS: zombie or kernel thread
}

func readBootTime(procRoot string) (time.Time, error) {
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("malformed btime in %s/stat: %w", procRoot, err)
			}
			return time.Unix(sec, 0), nil
		}
	}
	if err := s.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, fmt.Errorf("no btime in %s/stat", procRoot)
}
