package main

// Host vitals: what the box itself is doing, served so a client can see starvation coming.
//
// The failure this exists for (#77) is not a crash. Thirteen deliberate sessions, each holding an
// agent, starved an 8 GB host: load average 61 on 2 vCPUs, SSH timing out, tailscale unanswered,
// and nothing OOM-killed. Azure and the VPN control plane both still reported the machine healthy,
// because from the outside it was. Nothing in this tool could say otherwise, and finding out where
// 4.2 GB had gone meant walking pgrep -> parent bash -> parent dtach -> socket name by hand.
//
// Two properties of that outage decided what this file reports.
//
// **Free memory is not the signal.** The box showed 592 MB available of 7938 MB. Tight, but
// MemAvailable cannot tell healthy page cache from thrashing, which is exactly why the failure was
// silent. Pressure stall information measures the thing directly -- what share of the last ten
// seconds some task spent blocked waiting on memory -- so it is reported alongside the totals and
// is the number a client should lead with.
//
// **Load average is meaningless without the divisor.** 61 is catastrophic on 2 vCPUs and merely
// bad on 64, so cpuCount ships in the same document rather than leaving a client to guess.
//
// Everything here is os.ReadFile plus strconv plus one statfs. No new dependency: "stdlib, a pty
// and a websocket library" is a property this repo has turned down a YAML parser to keep, and a
// process-metrics library would be a far larger surrender for a panel.

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The nullable groups are pointers, and a field that could not be read is null rather than absent
// or zero -- the same contract Session.MarshalJSON publishes, for the same reason. Zero is a real
// value for most of these: swapTotalBytes is legitimately 0 on this box, and memory pressure of
// 0.00 is the normal reading on a healthy one. A client that could not distinguish "no pressure"
// from "this kernel has no PSI" would report calm for a box it knows nothing about, which is the
// mistake agentStatus's null-versus-clear split exists to prevent.
type hostLoad struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

type hostMemory struct {
	TotalBytes     int64 `json:"totalBytes"`
	AvailableBytes int64 `json:"availableBytes"`
	// UsedBytes is TotalBytes - AvailableBytes, computed here so two clients cannot derive it
	// differently. This is also what procps-ng's `free` prints in its "used" column -- it
	// stopped using the total-minus-free-minus-cache formula years ago -- so the two agree and
	// an operator cross-checking one against the other should find no discrepancy. An earlier
	// version of this comment and of the served schema claimed otherwise.
	UsedBytes      int64 `json:"usedBytes"`
	SwapTotalBytes int64 `json:"swapTotalBytes"`
	SwapFreeBytes  int64 `json:"swapFreeBytes"`
}

type hostPressure struct {
	MemorySome10 float64 `json:"memorySome10"`
	MemoryFull10 float64 `json:"memoryFull10"`
	// CPU and IO come from separate files, so either can be missing while memory is present.
	// Pointers rather than zeros: 0.00 is the healthy reading, so a fabricated one would claim
	// a measurement that was never taken.
	CPUSome10 *float64 `json:"cpuSome10"`
	IOSome10  *float64 `json:"ioSome10"`
}

type hostDisk struct {
	Path           string `json:"path"`
	TotalBytes     int64  `json:"totalBytes"`
	AvailableBytes int64  `json:"availableBytes"`
}

// hostSession is one session's share of memory. Name matches Session.name exactly, so a client
// holding a session list can join the two without a second lookup.
type hostSession struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
	// RSSBytes sums resident set size across the session's process tree. Shared pages are
	// counted once per process that maps them, so a tree of forked processes reads high and
	// the figure is an upper bound on real footprint. That is the same number ps and top show
	// and the same one #77 was diagnosed with, which is worth more than a more accurate figure
	// nobody can cross-check. Pss from smaps_rollup is the exact answer and is deliberately not
	// used: the kernel walks every VMA to produce it, which is one to two orders of magnitude
	// more expensive per process, on a document meant to be polled.
	RSSBytes     int64 `json:"rssBytes"`
	ProcessCount int   `json:"processCount"`
}

type hostReport struct {
	At            time.Time     `json:"at"`
	UptimeSeconds *float64      `json:"uptimeSeconds"`
	CPUCount      int           `json:"cpuCount"`
	Load          *hostLoad     `json:"load"`
	Memory        *hostMemory   `json:"memory"`
	Pressure      *hostPressure `json:"pressure"`
	Disk          *hostDisk     `json:"disk"`
	// A nil slice marshals to null, which is what makes "the session dir could not be read"
	// distinguishable from "there are no sessions". An empty non-nil slice is the second.
	Sessions      []hostSession `json:"sessions"`
	TotalRSSBytes *int64        `json:"totalRssBytes"`
}

// hostProbe is every impure thing this file needs, in one place, so the assembly above can be
// tested against fixture bytes instead of against whatever the machine running the tests happens
// to be doing. Same shape and same reason as checkSurvival's injected readers: a parser tested
// only against live /proc asserts nothing, because it cannot make the kernel report a starving
// box on demand.
type hostProbe struct {
	readFile func(string) ([]byte, error)
	// readDir lists a directory's entry names, for /proc/<pid>/task. Separate from readFile
	// because the thread list cannot be read as a file, and injected for the same reason: a
	// fixture has to be able to describe a process with more than one thread.
	readDir  func(string) ([]string, error)
	statfs   func(path string) (total, available int64, err error)
	pageSize int
	cpuCount int
}

// diskPath is the filesystem reported as `disk`.
//
// The root, deliberately, and not WT_DIR: the session sockets live under it, but so does
// everything else that fills a box -- container images, build caches, logs. An operator who has
// moved sessions elsewhere still runs out of space here first.
const diskPath = "/"

// maxTreeProcs bounds one session's tree walk. A session is a shell and its descendants, which is
// a handful of processes; anything approaching this is a fork storm, and the point of the bound is
// that a fork storm is precisely when this endpoint must still answer.
const maxTreeProcs = 4096

func newHostProbe() hostProbe {
	return hostProbe{
		readFile: os.ReadFile,
		readDir:  readDirNames,
		statfs:   statfsBytes,
		pageSize: syscall.Getpagesize(),
		// NumCPU, not the cgroup's cpu.max: this is the divisor for a load average, and the
		// kernel computes that over runnable tasks against real CPUs regardless of any quota.
		cpuCount: runtime.NumCPU(),
	}
}

// report assembles the document. Every reader degrades to null on its own; there is no error
// return, because a box whose /proc is partly unreadable is exactly when a client most needs the
// half that worked.
func (p hostProbe) report(sessions []Session, listErr error) hostReport {
	h := hostReport{
		At:       time.Now().UTC(),
		CPUCount: p.cpuCount,
		Load:     p.load(),
		Memory:   p.memory(),
		Pressure: p.pressure(),
		Disk:     p.disk(),
	}
	if up, ok := p.uptime(); ok {
		h.UptimeSeconds = &up
	}
	if listErr == nil {
		h.Sessions, h.TotalRSSBytes = p.sessionCosts(sessions)
	}
	return h
}

func (p hostProbe) load() *hostLoad {
	b, err := p.readFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return nil
	}
	one, err1 := strconv.ParseFloat(f[0], 64)
	five, err2 := strconv.ParseFloat(f[1], 64)
	fifteen, err3 := strconv.ParseFloat(f[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil
	}
	return &hostLoad{One: one, Five: five, Fifteen: fifteen}
}

func (p hostProbe) uptime() (float64, bool) {
	b, err := p.readFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 1 {
		return 0, false
	}
	up, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, false
	}
	return up, true
}

func (p hostProbe) memory() *hostMemory {
	b, err := p.readFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	kb := parseMeminfo(string(b))
	total, haveTotal := kb["MemTotal"]
	avail, haveAvail := kb["MemAvailable"]
	if !haveTotal || !haveAvail {
		// MemAvailable has been there since Linux 3.14 and is the only field here worth
		// reporting. Without it the rest is not worth half a document.
		return nil
	}
	m := &hostMemory{
		TotalBytes:     total * 1024,
		AvailableBytes: avail * 1024,
		UsedBytes:      (total - avail) * 1024,
		SwapTotalBytes: kb["SwapTotal"] * 1024,
		SwapFreeBytes:  kb["SwapFree"] * 1024,
	}
	return m
}

// parseMeminfo reads the "Key:  <n> kB" lines into kilobytes. Values with no kB suffix (HugePages_*
// are counts, not sizes) are still parsed; callers ask only for keys they know the unit of.
func parseMeminfo(s string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(s, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if v, err := strconv.ParseInt(f[0], 10, 64); err == nil {
			out[key] = v
		}
	}
	return out
}

func (p hostProbe) pressure() *hostPressure {
	some, full, ok := p.psi("/proc/pressure/memory")
	if !ok {
		// No PSI at all: not compiled in, or a container that does not expose it. Reporting
		// zeros here would be a claim, not a reading.
		return nil
	}
	out := &hostPressure{MemorySome10: some, MemoryFull10: full}
	if cpu, _, ok := p.psi("/proc/pressure/cpu"); ok {
		out.CPUSome10 = &cpu
	}
	if io, _, ok := p.psi("/proc/pressure/io"); ok {
		out.IOSome10 = &io
	}
	return out
}

// psi pulls avg10 off the "some" and "full" lines of a pressure file. `full` is absent for cpu on
// most kernels and zero where present, which is why its absence is not a failure.
func (p hostProbe) psi(path string) (some, full float64, ok bool) {
	b, err := p.readFile(path)
	if err != nil {
		return 0, 0, false
	}
	seen := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, found := 0.0, false
		for _, field := range f[1:] {
			if rest, isAvg := strings.CutPrefix(field, "avg10="); isAvg {
				if parsed, err := strconv.ParseFloat(rest, 64); err == nil {
					v, found = parsed, true
				}
				break
			}
		}
		if !found {
			continue
		}
		switch f[0] {
		case "some":
			some, seen = v, true
		case "full":
			full = v
		}
	}
	return some, full, seen
}

func (p hostProbe) disk() *hostDisk {
	total, avail, err := p.statfs(diskPath)
	if err != nil {
		return nil
	}
	return &hostDisk{Path: diskPath, TotalBytes: total, AvailableBytes: avail}
}

// statfsBytes is the production statfs. Bavail rather than Bfree: the reserved blocks only root
// can touch are not headroom for the service user.
func statfsBytes(path string) (int64, int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs, nil
}

// sessionCosts sums each session's process tree.
//
// The session list arrives already resolved, so scanDtach's pass over all of /proc -- which every
// listing already makes -- is the only walk of the whole process table. What this adds is bounded
// to the session trees themselves: per process, a thread list, one children file per thread, and
// one statm. A handful of small reads each. Sessions whose pid could not be
// resolved are reported with a zero cost rather than omitted: a client joining on name must find
// every row it can see in the session list, and a missing row reads as a session that costs
// nothing.
func (p hostProbe) sessionCosts(sessions []Session) ([]hostSession, *int64) {
	out := make([]hostSession, 0, len(sessions))
	var total int64
	for _, s := range sessions {
		row := hostSession{Name: s.Name, PID: s.PID}
		if s.PID != 0 {
			row.RSSBytes, row.ProcessCount = p.treeRSS(s.PID)
		}
		total += row.RSSBytes
		out = append(out, row)
	}
	return out, &total
}

// treeRSS walks pid and its descendants breadth-first, summing resident pages.
//
// The visited set is not paranoia about cycles -- /proc cannot express one -- but about the walk
// racing process churn: a pid that exits and is reused mid-walk can appear under two parents, and
// counting it twice would inflate the very number an operator is about to make a decision on.
func (p hostProbe) treeRSS(pid int) (int64, int) {
	visited := map[int]bool{pid: true}
	queue := []int{pid}
	var bytes int64
	seen, counted := 0, 0
	// The bound is on processes *examined*, not on processes that answered. Counting only the
	// ones with a readable statm would leave the walk unbounded exactly where a bound matters:
	// a fork storm churns pids, most reads fail, and the loop would keep going.
	for len(queue) > 0 && seen < maxTreeProcs {
		cur := queue[0]
		queue = queue[1:]
		seen++
		if rss, ok := p.rssBytes(cur); ok {
			bytes += rss
			counted++
		}
		for _, child := range p.children(cur) {
			if !visited[child] {
				visited[child] = true
				queue = append(queue, child)
			}
		}
	}
	return bytes, counted
}

// rssBytes reads field 2 of /proc/<pid>/statm, the resident page count.
//
// statm rather than status's VmRSS: one short line of integers instead of fifty labelled lines,
// and it is world-readable, so a session running as another user still reports a size.
func (p hostProbe) rssBytes(pid int) (int64, bool) {
	b, err := p.readFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * int64(p.pageSize), true
}

// children returns every process pid has forked, across all of its threads.
//
// **The per-thread part is the whole point.** `/proc/<pid>/task/<tid>/children` lists the children
// of that *thread*, not of the process: a child forked from a worker thread appears under that
// thread's file and under no other, for as long as the forking thread is alive. Reading only
// `task/<pid>/children` therefore misses entire subtrees, silently, and the programs it misses
// them for are the ones that matter here -- every Go binary forks from whatever OS thread the
// goroutine landed on, so `go build` started inside a session would have contributed nothing to
// that session's total. Reported memory that is quietly too low is worse than none, because the
// number exists to be acted on.
//
// [LAB] 2026-09-02, on this box: a child forked from a live worker thread was absent from the main
// thread's children file and present only under the forking thread's. Once that thread exits the
// child reparents to the group leader and shows up in the main file, which is exactly why this is
// easy to test wrongly and see nothing.
//
// childPIDs in sessions.go reads only the main thread and is correct where it is used: its
// argument is always a dtach master, which is single-threaded C. The assumption does not survive
// being moved here.
func (p hostProbe) children(pid int) []int {
	tids, err := p.readDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		// No task directory: the process is gone, or /proc is not what we think it is. Fall
		// back to the process-level file rather than reporting a childless tree.
		return p.childrenOfTask(pid, pid)
	}
	var out []int
	for _, tid := range tids {
		n, err := strconv.Atoi(tid)
		if err != nil {
			continue
		}
		out = append(out, p.childrenOfTask(pid, n)...)
	}
	return out
}

func (p hostProbe) childrenOfTask(pid, tid int) []int {
	b, err := p.readFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, tid))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if child, err := strconv.Atoi(f); err == nil {
			out = append(out, child)
		}
	}
	return out
}

// readDirNames is the production readDir.
func readDirNames(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// handleHost serves the report.
//
// Deliberately does NOT reap stale sockets, which handleSessionsList does as a side effect of the
// same listing. A document built to be polled by an open panel must not be the thing that unlinks
// files -- that is the half of #115 which is about what a read is allowed to do, and it applies
// here for the same reason it applies to a narration poll.
//
// A failed listing degrades to `sessions: null` rather than a 500: the host half is the more
// important one, it is read from somewhere else entirely, and an operator watching memory climb is
// not helped by an error page about a socket directory.
func (s *server) handleHost(w http.ResponseWriter, r *http.Request) {
	// Not logged. This route is polled, so a persistently unreadable session directory would
	// write the same line thousands of times a day for as long as one panel is open. The
	// failure is in the response as `sessions: null`, where the client that asked can see it.
	sessions, err := listSessions(s.sessionDir(), s.hubs.stats())
	writeJSON(w, http.StatusOK, newHostProbe().report(sessions, err))
}
