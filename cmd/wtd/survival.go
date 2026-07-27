package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Whether dtach sessions outlive a restart of this server is not a property of this code. It is a
// property of the unit that runs it, and this file is how the server finds that out instead of
// assuming it.
//
// The background is #21. A dtach master forked here keeps this process's cgroup for its whole life
// — `dtach -n` reparents it to PID 1, but reparenting does not move a cgroup — and systemd's
// default KillMode=control-group signals the entire cgroup on stop. So the unit, not the code,
// decides whether "restart the service" means "drop the clients" or "destroy every session".
//
// Three guards in this repo check that the shipped units say KillMode=process, and none of them
// can see the machine. They read files in a checkout; CI cannot do better, because the container
// that runs the install test has no systemd in it at all. Worse, they check the unit on *disk*,
// while systemd reads KillMode from the **loaded** unit at stop time. A correct install with a
// stale loaded unit is exactly the window this box was in while #21 was being deployed, and no
// static guard can ever see it.
//
// So the server asks systemd directly, once, at startup. This is the only check in the project
// evaluated on the machine that matters at the moment it matters.
//
// The lookup is injected rather than called directly so the parse and all three outcomes are
// testable without systemd, which the test environment does not have either.

// survival is what we know about whether dtach sessions outlive a restart of this unit. Unknown is
// the zero value and is a real answer, not a failure: run from a shell, or on a host without
// systemd, there is no unit to have an opinion about and the server should say nothing rather than
// guess in either direction.
type survival int

const (
	survivalUnknown survival = iota
	survivalGuaranteed
	survivalSwept
)

// unitFromCgroup returns the systemd service this process belongs to, or "" if it cannot tell.
//
// Handles both hierarchies: cgroup v2 has a single `0::<path>` line, v1 has one line per
// controller. The last `.service` component wins, so a unit nested under slices
// (`/system.slice/system-foo.slice/bar.service`) resolves to the service rather than a slice.
//
// A `.scope` is deliberately not matched. Scopes are processes systemd adopted rather than
// launched — a login session, a `systemd-run --scope` — and KillMode does not govern them the way
// it governs a service. Answering "unknown" for one is correct.
func unitFromCgroup(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		// v2: "0::/system.slice/wt-web.service". v1: "1:name=systemd:/system.slice/x.service".
		// The path is whatever follows the last colon in either shape.
		path := line
		if i := strings.LastIndex(line, ":"); i >= 0 {
			path = line[i+1:]
		}
		parts := strings.Split(path, "/")
		for i := len(parts) - 1; i >= 0; i-- {
			if strings.HasSuffix(parts[i], ".service") {
				return parts[i]
			}
		}
	}
	return ""
}

// killModeSurvives reports whether a KillMode value leaves processes other than the main one alone
// when the unit stops.
//
// Only `process` and `none` do. `mixed` is the trap: it SIGTERMs the main process alone, which
// reads as safe, and then SIGKILLs everything still in the cgroup once the unit finishes stopping
// — so a dtach master dies a few seconds later instead of immediately. `none` is deprecated and
// has its own problems, but it does not sweep, and reporting it as broken would be a lie.
func killModeSurvives(mode string) bool {
	return mode == "process" || mode == "none"
}

// checkSurvival resolves this process's unit and asks systemd what it would do to the cgroup on
// stop. It returns the verdict, the unit it asked about, and the raw KillMode, so a caller can name
// both in a message an operator can act on without a second lookup.
func checkSurvival(readCgroup func() (string, error), killMode func(string) (string, error)) (survival, string, string) {
	raw, err := readCgroup()
	if err != nil {
		return survivalUnknown, "", ""
	}
	unit := unitFromCgroup(raw)
	if unit == "" {
		return survivalUnknown, "", ""
	}
	mode, err := killMode(unit)
	if err != nil || mode == "" {
		// systemd is not answering (no systemctl on PATH, a container, a wedged manager). Not
		// knowing is reported as not knowing; a guess here would either cry wolf on every
		// developer box or reassure an operator whose sessions are about to die.
		return survivalUnknown, unit, ""
	}
	if killModeSurvives(mode) {
		return survivalGuaranteed, unit, mode
	}
	return survivalSwept, unit, mode
}

// readSelfCgroup is the production readCgroup.
func readSelfCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	return string(b), err
}

// systemctlKillMode is the production killMode lookup.
//
// Bounded: this runs on the startup path, and a manager that never answers must not be able to
// hold the service down. A timeout here degrades to survivalUnknown, which is the honest answer.
func systemctlKillMode(unit string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=KillMode", "--value").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// shutdownNotice is the line logged on the way out. It reports what is true on this machine rather
// than asserting the happy case, which is the whole point of #24: the previous wording promised
// "their sessions survive" unconditionally, and went on promising it throughout #21 while restarts
// were destroying sessions.
func shutdownNotice(s survival, unit, mode string) string {
	const prefix = "wtd: shutting down (connected terminals will drop; "
	switch s {
	case survivalGuaranteed:
		return prefix + "their sessions survive)"
	case survivalSwept:
		return prefix + "their sessions will NOT survive: " + unit + " has KillMode=" + mode +
			", so systemd is about to signal every process in this unit's cgroup, dtach masters included)"
	default:
		return prefix + "wtd does not touch dtach sessions, but whether they survive depends on " +
			"the unit running it and that could not be determined)"
	}
}

// survivalWarning is the startup message for a unit that will destroy its own sessions, or "" when
// there is nothing to say.
//
// Startup, not shutdown, is the point of it: by the time the shutdown line prints, the sessions are
// already being killed. This is the only warning in the project that can reach an operator while
// the problem is still hypothetical.
func survivalWarning(s survival, unit, mode string) string {
	if s != survivalSwept {
		return ""
	}
	return "wtd: WARNING " + unit + " has KillMode=" + mode + "; restarting it will DESTROY every " +
		"dtach session this server created, because systemd signals the whole cgroup and a dtach " +
		"master keeps that cgroup for life regardless of being reparented to PID 1. " +
		"Fix: add KillMode=process to the unit and run systemctl daemon-reload"
}
