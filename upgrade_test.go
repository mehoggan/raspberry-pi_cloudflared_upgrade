package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

const fakeToken = "eyJhIjoiU0VDUkVUIn0"

// fakePi simulates one Raspberry Pi well enough to exercise upgradeHost.
type fakePi struct {
	installedVersion  string
	availableVersion  string // what apt / cloudflared update would install
	installedByApt    bool
	sudoNeedsPassword bool
	cloudflaredUpdate int // exit code of `cloudflared update`
	processID         int
	binaryReplaced    bool
	extraCopy         string
	commands          []string
}

func (pi *fakePi) run(_ context.Context, command string) (commandResult, error) {
	pi.commands = append(pi.commands, command)
	ok := func(stdout string) (commandResult, error) { return commandResult{stdout: stdout}, nil }
	switch {
	case command == "hostname":
		return ok("raspberrypi\n")
	case command == "sudo -n true":
		if pi.sudoNeedsPassword {
			return commandResult{exitCode: 1, stderr: "sudo: a password is required"}, nil
		}
		return ok("")
	case strings.HasPrefix(command, "systemctl cat"):
		return ok("[Service]\nExecStart=/usr/bin/cloudflared --no-autoupdate tunnel run --token " + fakeToken + "\n")
	case strings.HasSuffix(command, "--version"):
		return ok("cloudflared version " + pi.installedVersion + " (built 2026-10-30)\n")
	case command == "dpkg -S /usr/bin/cloudflared":
		if pi.installedByApt {
			return ok("cloudflared: /usr/bin/cloudflared\n")
		}
		return commandResult{exitCode: 1}, nil
	case strings.HasPrefix(command, "dpkg -S"):
		return commandResult{exitCode: 1}, nil
	case command == "readlink -f /usr/bin/cloudflared":
		return ok("/usr/bin/cloudflared\n")
	case strings.HasPrefix(command, "which -a"):
		return ok("/usr/bin/cloudflared\n" + pi.extraCopy)
	case strings.HasPrefix(command, "systemctl show -p MainPID"):
		return ok(strconv.Itoa(pi.processID) + "\n")
	case strings.HasPrefix(command, "sudo -n readlink /proc/"):
		if pi.binaryReplaced {
			return ok("/usr/bin/cloudflared (deleted)\n")
		}
		return ok("/usr/bin/cloudflared\n")
	case command == "sudo -n apt-get update -q":
		return ok("")
	case strings.HasPrefix(command, "sudo -n apt-get install"):
		pi.installedVersion = pi.availableVersion
		return ok("")
	case strings.HasSuffix(command, " update"):
		if pi.cloudflaredUpdate == cloudflaredUpdatedExitCode {
			pi.installedVersion = pi.availableVersion
		}
		return commandResult{exitCode: pi.cloudflaredUpdate}, nil
	case strings.HasPrefix(command, "sudo -n systemctl restart"):
		pi.processID++
		pi.binaryReplaced = false
		return ok("")
	case strings.HasPrefix(command, "systemctl is-active"):
		return ok("active\n")
	case strings.HasPrefix(command, "sudo -n journalctl"):
		lines := "INF Version " + pi.installedVersion + "\n"
		for index := 0; index < 4; index++ {
			lines += "INF Registered tunnel connection connIndex=" + strconv.Itoa(index) + "\n"
		}
		return ok(lines)
	case strings.HasPrefix(command, "sudo -n rm -f"):
		return ok("")
	case strings.HasPrefix(command, "apt-cache policy"):
		return ok("cloudflared:\n  Installed: " + pi.installedVersion + "\n  Candidate: " + pi.availableVersion + "\n")
	}
	return commandResult{exitCode: 127, stderr: "fake: unknown command " + command}, nil
}

func testOptions(t *testing.T) (hostOptions, *strings.Builder) {
	t.Helper()
	minimum, _ := parseVersion(defaultMinimumVersion)
	var log strings.Builder
	return hostOptions{
		serviceName:         "cloudflared.service",
		minimumVersion:      minimum,
		expectedConnections: 4,
		verifyTimeout:       time.Second,
		pollInterval:        time.Millisecond,
		log: func(format string, arguments ...any) {
			log.WriteString(fmt.Sprintf(format, arguments...) + "\n")
		},
	}, &log
}

func TestUpgradesAptPackageAndRestarts(t *testing.T) {
	pi := &fakePi{installedVersion: "2026.7.2", availableVersion: "2026.10.7", installedByApt: true, processID: 1889}
	options, log := testOptions(t)

	report := upgradeHost(context.Background(), pi, "192.168.1.2", options)

	if report.err != nil {
		t.Fatalf("unexpected error: %v\n%s", report.err, log)
	}
	if report.outcome != outcomeUpgraded || !report.restarted || report.registeredConnections != 4 {
		t.Fatalf("got outcome %q restarted=%v connections=%d", report.outcome, report.restarted, report.registeredConnections)
	}
	if report.versionAfter.String() != "2026.10.7" {
		t.Fatalf("got version after %s", report.versionAfter)
	}
	if strings.Contains(log.String(), fakeToken) {
		t.Fatal("the tunnel token was written to the log")
	}
}

func TestRestartsWhenBinaryWasUpgradedEarlierWithoutRestart(t *testing.T) {
	// Exactly what happened on Pi #1: apt had already installed the new
	// version, but the service was still running the old one.
	pi := &fakePi{installedVersion: "2026.10.7", availableVersion: "2026.10.7", installedByApt: true, processID: 1889, binaryReplaced: true}
	options, log := testOptions(t)

	report := upgradeHost(context.Background(), pi, "192.168.1.2", options)

	if report.err != nil || report.outcome != outcomeRestarted {
		t.Fatalf("got outcome %q err %v\n%s", report.outcome, report.err, log)
	}
}

func TestSkipsRestartWhenAlreadyCurrent(t *testing.T) {
	pi := &fakePi{installedVersion: "2026.10.7", availableVersion: "2026.10.7", installedByApt: true, processID: 42}
	options, _ := testOptions(t)

	report := upgradeHost(context.Background(), pi, "192.168.1.4", options)

	if report.outcome != outcomeAlreadyCurrent || report.restarted {
		t.Fatalf("got outcome %q restarted=%v", report.outcome, report.restarted)
	}
}

func TestStandaloneBinaryUsesCloudflaredUpdate(t *testing.T) {
	pi := &fakePi{installedVersion: "2026.7.2", availableVersion: "2026.10.7", cloudflaredUpdate: cloudflaredUpdatedExitCode, processID: 7}
	options, log := testOptions(t)

	report := upgradeHost(context.Background(), pi, "192.168.1.6", options)

	if report.err != nil || report.installMethod != installMethodBinary || report.outcome != outcomeUpgraded {
		t.Fatalf("got %q / %q err %v\n%s", report.installMethod, report.outcome, report.err, log)
	}
}

func TestStopsWhenSudoNeedsPassword(t *testing.T) {
	pi := &fakePi{installedVersion: "2026.7.2", sudoNeedsPassword: true}
	options, _ := testOptions(t)

	report := upgradeHost(context.Background(), pi, "192.168.1.4", options)

	if report.outcome != outcomeFailed || !strings.Contains(report.err.Error(), "password") {
		t.Fatalf("got outcome %q err %v", report.outcome, report.err)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	pi := &fakePi{installedVersion: "2026.7.2", availableVersion: "2026.10.7", installedByApt: true, processID: 1}
	options, _ := testOptions(t)
	options.dryRun = true

	report := upgradeHost(context.Background(), pi, "192.168.1.2", options)

	if report.outcome != outcomeDryRun {
		t.Fatalf("got outcome %q", report.outcome)
	}
	for _, command := range pi.commands {
		if strings.HasPrefix(command, "sudo") {
			t.Fatalf("dry run ran %q", command)
		}
	}
}

func TestReportsAndRemovesStaleCopy(t *testing.T) {
	pi := &fakePi{installedVersion: "2026.10.7", availableVersion: "2026.10.7", installedByApt: true, processID: 3, extraCopy: "/usr/local/bin/cloudflared\n"}
	options, _ := testOptions(t)
	options.removeStaleCopies = true

	report := upgradeHost(context.Background(), pi, "192.168.1.2", options)

	if len(report.staleCopies) != 0 {
		t.Fatalf("stale copy not removed: %v", report.staleCopies)
	}
	found := false
	for _, command := range pi.commands {
		found = found || command == "sudo -n rm -f /usr/local/bin/cloudflared"
	}
	if !found {
		t.Fatal("rm was not run for the stale copy")
	}
}

func TestServiceBinary(t *testing.T) {
	cases := map[string]string{
		"ExecStart=/usr/bin/cloudflared --no-autoupdate tunnel run --token " + fakeToken: "/usr/bin/cloudflared",
		"ExecStart=-/usr/local/bin/cloudflared tunnel run":                               "/usr/local/bin/cloudflared",
		"ExecStart=/old/cloudflared\nExecStart=\nExecStart=/usr/bin/cloudflared run":     "/usr/bin/cloudflared",
	}
	for unit, want := range cases {
		got, err := serviceBinary(unit)
		if err != nil || got != want {
			t.Errorf("serviceBinary(%q) = %q, %v; want %q", unit, got, err, want)
		}
	}
	if _, err := serviceBinary("ExecStart=/usr/bin/cloudflared;rm -rf / run"); err == nil {
		t.Error("accepted an unsafe path")
	}
}

func TestVersionComparison(t *testing.T) {
	older, _ := parseVersion("cloudflared version 2026.7.2 (built 2026-07-15)")
	minimum, _ := parseVersion("2026.10.2")
	newer, _ := parseVersion("2026.10.7")
	if !older.olderThan(minimum) || newer.olderThan(minimum) || minimum.olderThan(minimum) {
		t.Fatal("version comparison is wrong")
	}
	if _, err := parseVersion("garbage"); err == nil {
		t.Fatal("parsed garbage")
	}
}
