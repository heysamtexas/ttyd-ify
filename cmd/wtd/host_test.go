package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// fakeProc turns a map of path -> contents into a hostProbe. Everything the collector reads goes
// through it, so a starving box can be described in a literal instead of waited for: the readings
// that matter most here are ones a healthy test machine will never produce.
func fakeProc(files map[string]string) hostProbe {
	return hostProbe{
		readFile: func(path string) ([]byte, error) {
			body, ok := files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(body), nil
		},
		statfs: func(string) (int64, int64, error) {
			return 65498251264, 23622320128, nil
		},
		pageSize: 4096,
		cpuCount: 2,
		now:      func() time.Time { return time.Date(2026, 9, 2, 14, 2, 11, 0, time.UTC) },
	}
}

// The outage in #77: load 61 on 2 vCPUs, 592 MB of 7938 MB available, no swap, and pressure pinned.
// Deliberately the fixture for the happy path, because these are the numbers the endpoint exists to
// make visible and every threshold downstream is calibrated against them.
func starvingProc() map[string]string {
	return map[string]string{
		"/proc/loadavg":         "61.40 61.21 55.42 1/1051 557846\n",
		"/proc/uptime":          "1642000.10 6533221.44\n",
		"/proc/meminfo":         "MemTotal:        8129166 kB\nMemFree:          201728 kB\nMemAvailable:     606208 kB\nBuffers:            2048 kB\nCached:           920576 kB\nSwapTotal:             0 kB\nSwapFree:              0 kB\n",
		"/proc/pressure/memory": "some avg10=71.40 avg60=68.12 avg300=51.03 total=12621283\nfull avg10=44.20 avg60=40.01 avg300=30.55 total=10659027\n",
		"/proc/pressure/cpu":    "some avg10=98.10 avg60=97.44 avg300=90.12 total=19107256586\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
		"/proc/pressure/io":     "some avg10=12.60 avg60=10.02 avg300=8.11 total=88123\nfull avg10=6.10 avg60=5.00 avg300=4.00 total=44000\n",
	}
}

func TestHostReportReadsEveryVital(t *testing.T) {
	got := fakeProc(starvingProc()).report(nil, errors.New("no session dir"))

	if got.CPUCount != 2 {
		t.Errorf("cpuCount = %d, want 2", got.CPUCount)
	}
	if got.Load == nil || got.Load.One != 61.40 || got.Load.Five != 61.21 || got.Load.Fifteen != 55.42 {
		t.Errorf("load = %+v, want 61.40/61.21/55.42", got.Load)
	}
	if got.UptimeSeconds == nil || *got.UptimeSeconds != 1642000.10 {
		t.Errorf("uptimeSeconds = %v, want 1642000.10", got.UptimeSeconds)
	}
	if got.Memory == nil {
		t.Fatal("memory is nil")
	}
	// kB in the file, bytes on the wire. A client comparing availableBytes to totalBytes must
	// not have to know which unit the kernel happened to use.
	if want := int64(8129166) * 1024; got.Memory.TotalBytes != want {
		t.Errorf("totalBytes = %d, want %d", got.Memory.TotalBytes, want)
	}
	if want := int64(606208) * 1024; got.Memory.AvailableBytes != want {
		t.Errorf("availableBytes = %d, want %d", got.Memory.AvailableBytes, want)
	}
	// total - available, not free's "used": on this fixture free would report ~7005 MB by
	// discounting cache, and the point of the field is headroom, so it must be the larger number.
	if want := int64(8129166-606208) * 1024; got.Memory.UsedBytes != want {
		t.Errorf("usedBytes = %d, want %d (total - available)", got.Memory.UsedBytes, want)
	}
	if got.Memory.SwapTotalBytes != 0 {
		t.Errorf("swapTotalBytes = %d, want 0", got.Memory.SwapTotalBytes)
	}
	if got.Pressure == nil {
		t.Fatal("pressure is nil")
	}
	if got.Pressure.MemorySome10 != 71.40 || got.Pressure.MemoryFull10 != 44.20 {
		t.Errorf("memory pressure = %+v, want some 71.40 / full 44.20", got.Pressure)
	}
	// avg10 specifically, not avg60 or the cumulative total: the field names it, and reading
	// the wrong column here would report a box as calm through the first ten seconds of a stall.
	if got.Pressure.CPUSome10 == nil || *got.Pressure.CPUSome10 != 98.10 {
		t.Errorf("cpuSome10 = %v, want 98.10", got.Pressure.CPUSome10)
	}
	if got.Pressure.IOSome10 == nil || *got.Pressure.IOSome10 != 12.60 {
		t.Errorf("ioSome10 = %v, want 12.60", got.Pressure.IOSome10)
	}
	if got.Disk == nil || got.Disk.Path != "/" || got.Disk.AvailableBytes != 23622320128 {
		t.Errorf("disk = %+v", got.Disk)
	}
	// The listing failed, so sessions is unknown rather than empty.
	if got.Sessions != nil {
		t.Errorf("sessions = %v, want nil when the listing failed", got.Sessions)
	}
	if got.TotalRSSBytes != nil {
		t.Errorf("totalRssBytes = %v, want nil when the listing failed", got.TotalRSSBytes)
	}
}

// A field that could not be read is null on the wire, and the key is still there.
//
// Absent keys are the failure mode this repo has already paid for once: pid and cwd were
// omitempty, which made a session whose /proc lookup failed unrepresentable to a decoder
// generated from the very schema that documents them. The whole document is nullable here, so
// getting it wrong would be worse.
func TestHostRendersUnreadableFieldsAsNull(t *testing.T) {
	// Nothing readable at all, which is also what a non-Linux or heavily sandboxed box looks like.
	body, err := json.Marshal(fakeProc(nil).report([]Session{}, nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"load", "memory", "pressure", "uptimeSeconds"} {
		v, ok := raw[key]
		if !ok {
			t.Errorf("%s is absent; it must be present and null", key)
			continue
		}
		if string(v) != "null" {
			t.Errorf("%s = %s, want null", key, v)
		}
	}
	// disk does not come from a file, so it survives an empty /proc. That is the point of the
	// per-reading degradation: one unreadable source does not blank the others.
	if string(raw["disk"]) == "null" {
		t.Error("disk went null although statfs answered")
	}
	// An empty listing is an empty array, never null. The two mean different things.
	if string(raw["sessions"]) != "[]" {
		t.Errorf("sessions = %s, want [] for a readable but empty session dir", raw["sessions"])
	}
}

// pressure: null and pressure: 0 must not be the same bytes.
//
// This is agentStatus's null-versus-clear distinction again, and it is the one a client is most
// likely to collapse: 0.00 is the reading on every healthy box, so a client that treats a missing
// PSI as zero shows a calm gauge for a machine it has measured nothing about.
func TestHostPressureNullIsDistinctFromZero(t *testing.T) {
	noPSI := starvingProc()
	delete(noPSI, "/proc/pressure/memory")
	delete(noPSI, "/proc/pressure/cpu")
	delete(noPSI, "/proc/pressure/io")

	idle := starvingProc()
	idle["/proc/pressure/memory"] = "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"

	missing := fakeProc(noPSI).report(nil, nil)
	if missing.Pressure != nil {
		t.Errorf("pressure = %+v with no PSI files, want nil", missing.Pressure)
	}
	measured := fakeProc(idle).report(nil, nil)
	if measured.Pressure == nil {
		t.Fatal("pressure is nil although /proc/pressure/memory read as all zeroes")
	}
	if measured.Pressure.MemorySome10 != 0 {
		t.Errorf("memorySome10 = %v, want 0", measured.Pressure.MemorySome10)
	}
}

// Memory is reported only when MemAvailable is there. Half a memory reading is worse than none:
// total alone invites a client to subtract MemFree and call it headroom, which is the mistake the
// field exists to prevent.
func TestHostMemoryNeedsMemAvailable(t *testing.T) {
	files := starvingProc()
	files["/proc/meminfo"] = "MemTotal:        8129166 kB\nMemFree:          201728 kB\n"
	if got := fakeProc(files).report(nil, nil); got.Memory != nil {
		t.Errorf("memory = %+v without MemAvailable, want nil", got.Memory)
	}
}

func TestHostTreeRSSSumsTheProcessTree(t *testing.T) {
	// 100 (shell) -> 101, 102; 101 -> 103. Four processes, one page apiece except 103.
	files := map[string]string{
		"/proc/100/task/100/children": "101 102\n",
		"/proc/101/task/101/children": "103\n",
		"/proc/100/statm":             "2000 10 5 1 0 100 0\n",
		"/proc/101/statm":             "2000 20 5 1 0 100 0\n",
		"/proc/102/statm":             "2000 30 5 1 0 100 0\n",
		"/proc/103/statm":             "2000 40 5 1 0 100 0\n",
	}
	p := fakeProc(files)
	bytes, count := p.treeRSS(100)
	if want := int64(10+20+30+40) * 4096; bytes != want {
		t.Errorf("treeRSS bytes = %d, want %d", bytes, want)
	}
	if count != 4 {
		t.Errorf("treeRSS count = %d, want 4", count)
	}

	// A pid reachable from two parents is counted once. /proc cannot express a cycle, but a pid
	// that exits and is reused mid-walk can appear twice, and double-counting inflates exactly
	// the number an operator is about to act on.
	files["/proc/102/task/102/children"] = "103\n"
	if bytes, count = p.treeRSS(100); count != 4 {
		t.Errorf("with a doubly-reachable child: count = %d, want 4", count)
	}

	// A process that vanished between the children read and the statm read contributes nothing
	// and does not abort the walk.
	delete(files, "/proc/101/statm")
	bytes, count = p.treeRSS(100)
	if want := int64(10+30+40) * 4096; bytes != want {
		t.Errorf("with one process gone: bytes = %d, want %d", bytes, want)
	}
	if count != 3 {
		t.Errorf("with one process gone: count = %d, want 3", count)
	}
}

// A session whose pid could not be resolved is still listed, at zero.
//
// Dropping the row would be worse than reporting nothing for it: a client joins these to the
// session list on name, and a missing row is indistinguishable from a session that costs nothing.
func TestHostSessionCostsKeepUnresolvedSessions(t *testing.T) {
	files := map[string]string{
		"/proc/100/statm": "2000 50 5 1 0 100 0\n",
	}
	rows, total := fakeProc(files).sessionCosts([]Session{
		{Name: "ops", PID: 100},
		{Name: "unresolved"}, // pid 0: /proc lookup failed for this one
	})
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if idx := slices.IndexFunc(rows, func(r hostSession) bool { return r.Name == "unresolved" }); idx < 0 {
		t.Error("the unresolved session was dropped from the listing")
	}
	if want := int64(50) * 4096; rows[0].RSSBytes != want {
		t.Errorf("ops rssBytes = %d, want %d", rows[0].RSSBytes, want)
	}
	if total == nil || *total != int64(50)*4096 {
		t.Errorf("totalRssBytes = %v, want %d", total, int64(50)*4096)
	}
}

// The walk is bounded, because a fork storm is exactly when this endpoint has to answer.
func TestHostTreeRSSIsBounded(t *testing.T) {
	files := map[string]string{}
	// A chain long enough to exceed the bound: 1..maxTreeProcs+50, each the child of the last.
	for i := 1; i <= maxTreeProcs+50; i++ {
		files[fmt.Sprintf("/proc/%d/statm", i)] = "2000 1 1 1 0 1 0\n"
		files[fmt.Sprintf("/proc/%d/task/%d/children", i, i)] = fmt.Sprintf("%d\n", i+1)
	}
	_, count := fakeProc(files).treeRSS(1)
	if count > maxTreeProcs {
		t.Errorf("visited %d processes, want at most %d", count, maxTreeProcs)
	}
	if count < maxTreeProcs/2 {
		t.Errorf("visited only %d processes; the bound should not stop the walk this early", count)
	}
}

// The handler's key set and the schema's must agree, in both directions.
//
// Same check as TestMetaMatchesItsSchema and for the same reason: a hand-written JSON document and
// a hand-written schema for it drift silently, and a generated client is the audience that pays.
func TestHostMatchesItsSchema(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string       `json:"required"`
				Properties map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIJSON, &spec); err != nil {
		t.Fatalf("decode the embedded spec: %v", err)
	}

	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/host", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not an object: %v", err)
	}
	declared := spec.Components.Schemas["Host"]
	if len(declared.Properties) == 0 {
		t.Fatal("the spec declares no properties for Host; this test would pass vacuously")
	}
	for key := range declared.Properties {
		if _, ok := got[key]; !ok {
			t.Errorf("the spec declares Host.%s and the handler does not send it", key)
		}
	}
	for key := range got {
		if _, ok := declared.Properties[key]; !ok {
			t.Errorf("the handler sends %q and the spec does not declare it", key)
		}
	}
	// Every property is required, so the shape never varies between polls.
	for key := range declared.Properties {
		if !slices.Contains(declared.Required, key) {
			t.Errorf("Host.%s is declared but not required; a nullable field must still always be present", key)
		}
	}
}

// Reading host state must not unlink anything.
//
// GET /api/v1/sessions reaps stale sockets as a side effect, deliberately -- it is where the two
// pickers get reconciled. This document is built to be polled by an open panel, so the same
// listing must not carry the same side effect here. #115 is the standing version of this rule.
func TestHostDoesNotReapStaleSockets(t *testing.T) {
	srv, dir := newTestServer(t)
	stale := filepath.Join(dir, "abandoned"+socketSuffix)
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Leave the socket file behind with nothing listening, which is what a killed dtach master
	// leaves and what reapStale is entitled to remove.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/host", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the host report unlinked a stale socket: %v", err)
	}

	// And the control: the listing route does reap it, so this test is asserting a difference
	// rather than that nothing reaps anywhere.
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("GET /api/v1/sessions did not reap the stale socket (err = %v); the comparison above proves nothing", err)
	}
}

// The live readers must work on the machine running the tests. The fixtures above prove the
// parsing; this proves the paths and the statfs are real, which no fixture can.
func TestHostProbeReadsThisMachine(t *testing.T) {
	got := newHostProbe().report(nil, nil)
	if got.CPUCount < 1 {
		t.Errorf("cpuCount = %d on a running machine", got.CPUCount)
	}
	if got.Load == nil {
		t.Error("load is nil on a running Linux machine")
	}
	if got.Memory == nil {
		t.Fatal("memory is nil on a running Linux machine")
	}
	if got.Memory.TotalBytes <= 0 || got.Memory.AvailableBytes <= 0 {
		t.Errorf("memory = %+v, want positive totals", got.Memory)
	}
	if got.Disk == nil || got.Disk.TotalBytes <= 0 {
		t.Errorf("disk = %+v, want a positive size for %s", got.Disk, diskPath)
	}
	if got.UptimeSeconds == nil || *got.UptimeSeconds <= 0 {
		t.Errorf("uptimeSeconds = %v", got.UptimeSeconds)
	}
	// Pressure is NOT asserted: a kernel without CONFIG_PSI is a supported host, and requiring
	// it here would fail on one for a reason the endpoint already reports honestly as null.
}
