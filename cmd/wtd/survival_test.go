package main

import (
	"errors"
	"strings"
	"testing"
)

// The cgroup parse, across the shapes a real /proc/self/cgroup actually takes. None of this can be
// exercised on the machine running the test — the CI container has no systemd — so the file
// contents are the fixture and the parse is the thing under test.
func TestUnitFromCgroup(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"v2 under system.slice", "0::/system.slice/wt.service\n", "wt.service"},
		{"v2 nested slices", "0::/system.slice/system-x.slice/wt.service\n", "wt.service"},
		{
			// v1 lists one line per controller and the systemd one is not first.
			"v1 multi-controller",
			"12:pids:/system.slice/wt.service\n1:name=systemd:/system.slice/wt.service\n",
			"wt.service",
		},
		{
			// A login shell or `systemd-run --scope`. KillMode does not govern a scope the way it
			// governs a service, so refusing to answer is the correct answer.
			"scope is not a service",
			"0::/user.slice/user-1000.slice/session-3.scope\n",
			"",
		},
		{"root cgroup", "0::/\n", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unitFromCgroup(tc.raw); got != tc.want {
				t.Errorf("unitFromCgroup(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// The three verdicts, and specifically that "could not find out" never collapses into either
// confident answer — the same rule probeSocket follows for the same reason.
func TestCheckSurvival(t *testing.T) {
	const cg = "0::/system.slice/wt.service\n"
	okCgroup := func() (string, error) { return cg, nil }

	for _, tc := range []struct {
		name     string
		cgroup   func() (string, error)
		killMode func(string) (string, error)
		want     survival
		wantUnit string
	}{
		{
			"KillMode=process survives",
			okCgroup,
			func(string) (string, error) { return "process", nil },
			survivalGuaranteed, "wt.service",
		},
		{
			// The trap: mixed SIGTERMs only the main process, then SIGKILLs whatever is left in
			// the cgroup once the unit finishes stopping. It reads as safe and is not.
			"KillMode=mixed is swept, not safe",
			okCgroup,
			func(string) (string, error) { return "mixed", nil },
			survivalSwept, "wt.service",
		},
		{
			"KillMode=control-group is swept",
			okCgroup,
			func(string) (string, error) { return "control-group", nil },
			survivalSwept, "wt.service",
		},
		{
			"KillMode=none does not sweep",
			okCgroup,
			func(string) (string, error) { return "none", nil },
			survivalGuaranteed, "wt.service",
		},
		{
			// No systemctl on PATH, a container, a wedged manager. Unknown, not broken: crying
			// wolf on every developer box would train operators to ignore the warning.
			"systemd not answering is unknown",
			okCgroup,
			func(string) (string, error) { return "", errors.New("exec: systemctl not found") },
			survivalUnknown, "wt.service",
		},
		{
			"no cgroup file is unknown",
			func() (string, error) { return "", errors.New("no such file") },
			func(string) (string, error) { t.Fatal("systemd must not be asked without a unit"); return "", nil },
			survivalUnknown, "",
		},
		{
			"not in a service is unknown",
			func() (string, error) { return "0::/user.slice/session-3.scope\n", nil },
			func(string) (string, error) { t.Fatal("systemd must not be asked without a unit"); return "", nil },
			survivalUnknown, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, unit, _ := checkSurvival(tc.cgroup, tc.killMode)
			if got != tc.want {
				t.Errorf("checkSurvival = %v, want %v", got, tc.want)
			}
			if unit != tc.wantUnit {
				t.Errorf("unit = %q, want %q", unit, tc.wantUnit)
			}
		})
	}
}

// The messages are the deliverable of #24, not a side effect of it: the old shutdown line asserted
// "their sessions survive" unconditionally and kept asserting it all through #21 while restarts
// were destroying sessions. So the wording is asserted, not just the verdict behind it.
func TestSurvivalMessages(t *testing.T) {
	t.Run("swept warns at startup, names the unit and the fix", func(t *testing.T) {
		w := survivalWarning(survivalSwept, "wt.service", "control-group")
		for _, want := range []string{"WARNING", "wt.service", "control-group", "KillMode=process", "daemon-reload"} {
			if !strings.Contains(w, want) {
				t.Errorf("startup warning is missing %q; an operator cannot act on it\ngot: %s", want, w)
			}
		}
	})

	t.Run("nothing to warn about stays silent", func(t *testing.T) {
		for _, s := range []survival{survivalGuaranteed, survivalUnknown} {
			if w := survivalWarning(s, "wt.service", "process"); w != "" {
				t.Errorf("survivalWarning(%v) = %q, want silence", s, w)
			}
		}
	})

	t.Run("shutdown line reports the machine, not the happy case", func(t *testing.T) {
		if got := shutdownNotice(survivalGuaranteed, "wt.service", "process"); !strings.Contains(got, "their sessions survive") {
			t.Errorf("guaranteed: %q", got)
		}
		swept := shutdownNotice(survivalSwept, "wt.service", "control-group")
		if !strings.Contains(swept, "will NOT survive") {
			t.Errorf("swept line does not say sessions are about to die: %q", swept)
		}
		if strings.Contains(strings.ReplaceAll(swept, "will NOT survive", ""), "sessions survive") {
			t.Errorf("swept line still contains the old unconditional promise: %q", swept)
		}
		unknown := shutdownNotice(survivalUnknown, "", "")
		if strings.Contains(unknown, "will NOT survive") || strings.Contains(unknown, "their sessions survive") {
			t.Errorf("unknown must claim neither outcome: %q", unknown)
		}
	})
}
