package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// hostOptions controls how one host is upgraded.
type hostOptions struct {
	serviceName         string
	minimumVersion      cloudflaredVersion
	dryRun              bool
	forceRestart        bool
	removeStaleCopies   bool
	expectedConnections int
	verifyTimeout       time.Duration
	pollInterval        time.Duration
	log                 func(format string, arguments ...any)
}

// hostReport is the result for one host, shown in the final summary.
type hostReport struct {
	host                  string
	hostname              string
	binaryPath            string
	installMethod         string
	versionBefore         cloudflaredVersion
	versionAfter          cloudflaredVersion
	restarted             bool
	registeredConnections int
	staleCopies           []string
	outcome               string
	err                   error
}

const (
	outcomeUpgraded       = "upgraded"
	outcomeRestarted      = "restarted (binary was already new)"
	outcomeAlreadyCurrent = "already current"
	outcomeDryRun         = "dry run"
	outcomeFailed         = "FAILED"
	outcomeSkipped        = "skipped"

	installMethodPackage = "apt package"
	installMethodBinary  = "standalone binary"

	// `cloudflared update` exits with 11 when it installed a new version.
	cloudflaredUpdatedExitCode = 11

	registeredConnectionMarker = "Registered tunnel connection"
)

var safePathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/+-]+$`)

// upgradeHost runs the whole procedure on one host:
// find the binary the service runs → upgrade it the way it was installed →
// restart only if needed → wait until the tunnel registers its connections.
func upgradeHost(ctx context.Context, runner commandRunner, host string, options hostOptions) hostReport {
	report := hostReport{host: host}
	logf := func(format string, arguments ...any) {
		options.log("[%s] "+format, append([]any{host}, arguments...)...)
	}
	fail := func(err error) hostReport {
		report.outcome = outcomeFailed
		report.err = err
		logf("FAILED: %v", err)
		return report
	}
	run := func(command string) (commandResult, error) {
		return runner.run(ctx, command)
	}
	service := options.serviceName

	// Step 1: reach the host, and check sudo will not stop for a password.
	result, err := run("hostname")
	if err != nil {
		return fail(err)
	}
	report.hostname = strings.TrimSpace(result.stdout)
	logf("connected (hostname %s)", report.hostname)

	if !options.dryRun {
		if result, err = run("sudo -n true"); err != nil {
			return fail(err)
		}
		if result.exitCode != 0 {
			return fail(errors.New("sudo asks for a password on this host; see README section 'Let sudo run without a password'"))
		}
	}

	// Step 2: find the binary the service actually runs. The unit file holds
	// the tunnel token, so its text is parsed here and never printed.
	if result, err = run("systemctl cat " + service); err != nil {
		return fail(err)
	}
	if result.exitCode != 0 {
		return fail(fmt.Errorf("no %s service on this host", service))
	}
	report.binaryPath, err = serviceBinary(result.stdout)
	if err != nil {
		return fail(err)
	}
	logf("service runs %s", report.binaryPath)

	if report.versionBefore, err = binaryVersion(run, report.binaryPath); err != nil {
		return fail(err)
	}
	logf("version on disk: %s (need %s or newer)", report.versionBefore, options.minimumVersion)

	// Step 3: how was it installed? A package must be upgraded by its package manager.
	if result, err = run("dpkg -S " + report.binaryPath); err != nil {
		return fail(err)
	}
	if result.exitCode == 0 {
		report.installMethod = installMethodPackage
	} else {
		report.installMethod = installMethodBinary
	}
	logf("installed as: %s", report.installMethod)

	staleCopies, err := findStaleCopies(run, report.binaryPath)
	if err != nil {
		return fail(err)
	}
	report.staleCopies = staleCopies
	for _, copyPath := range staleCopies {
		logf("note: another cloudflared at %s shadows or differs from the service binary", copyPath)
	}

	if options.dryRun {
		if report.installMethod == installMethodPackage {
			result, _ = run("apt-cache policy cloudflared")
			if candidate := aptCandidate(result.stdout); candidate != "" {
				logf("dry run: apt would install %s", candidate)
			}
			logf("dry run: would run apt-get update + apt-get install --only-upgrade cloudflared")
		} else {
			logf("dry run: would run sudo %s update", report.binaryPath)
		}
		logf("dry run: would restart %s if the version changed, then wait for %d connections", service, options.expectedConnections)
		report.versionAfter = report.versionBefore
		report.outcome = outcomeDryRun
		return report
	}

	pidBefore, err := mainProcessID(run, service)
	if err != nil {
		return fail(err)
	}

	// Step 4: upgrade.
	if report.installMethod == installMethodPackage {
		if err = upgradeWithApt(run, logf); err != nil {
			return fail(err)
		}
		version, versionError := binaryVersion(run, report.binaryPath)
		if versionError != nil {
			return fail(versionError)
		}
		if version.olderThan(options.minimumVersion) {
			logf("apt only offered %s; installing the latest .deb from GitHub instead", version)
			if err = installLatestDebFromGitHub(run); err != nil {
				return fail(err)
			}
		}
	} else {
		logf("running cloudflared update")
		if result, err = run("sudo -n " + report.binaryPath + " update"); err != nil {
			return fail(err)
		}
		if result.exitCode != 0 && result.exitCode != cloudflaredUpdatedExitCode {
			return fail(fmt.Errorf("cloudflared update exited %d: %s", result.exitCode, lastLines(result.stdout+result.stderr, 3)))
		}
	}

	if report.versionAfter, err = binaryVersion(run, report.binaryPath); err != nil {
		return fail(err)
	}
	logf("version on disk now: %s", report.versionAfter)
	if report.versionAfter.olderThan(options.minimumVersion) {
		return fail(fmt.Errorf("still %s after upgrading; need %s or newer", report.versionAfter, options.minimumVersion))
	}

	// Step 5: restart only if the running process is old. A process whose
	// binary was replaced on disk shows "(deleted)" in /proc/<pid>/exe; that
	// catches an upgrade done earlier without a restart.
	binaryChanged := report.versionBefore != report.versionAfter
	runningOldBinary := runningBinaryReplaced(run, pidBefore)
	if !(options.forceRestart || binaryChanged || runningOldBinary) {
		logf("service already runs %s; no restart needed", report.versionAfter)
		if result, err = run("systemctl is-active " + service); err != nil {
			return fail(err)
		}
		if strings.TrimSpace(result.stdout) != "active" {
			return fail(fmt.Errorf("%s is %s", service, strings.TrimSpace(result.stdout)))
		}
		report.outcome = outcomeAlreadyCurrent
		return finishWithStaleCopies(run, &report, options, logf)
	}

	logf("restarting %s (in-progress requests get up to 30 s to finish)", service)
	if result, err = run("sudo -n systemctl restart " + service); err != nil {
		return fail(err)
	}
	if result.exitCode != 0 {
		return fail(fmt.Errorf("systemctl restart exited %d: %s", result.exitCode, lastLines(result.stderr, 3)))
	}
	report.restarted = true

	// Step 6: wait until the new process is active and has registered its
	// connections to Cloudflare.
	if err = waitForTunnel(ctx, run, &report, pidBefore, options, logf); err != nil {
		return fail(err)
	}
	if binaryChanged {
		report.outcome = outcomeUpgraded
	} else {
		report.outcome = outcomeRestarted
	}
	return finishWithStaleCopies(run, &report, options, logf)
}

type runFunction func(command string) (commandResult, error)

// serviceBinary returns the program from the last ExecStart= line of
// `systemctl cat` output (drop-ins can override earlier lines).
func serviceBinary(unitText string) (string, error) {
	binary := ""
	for _, line := range strings.Split(unitText, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ExecStart="))
		if len(fields) == 0 {
			binary = "" // an empty ExecStart= resets the list
			continue
		}
		// systemd allows prefixes such as "-" or "@" before the path.
		binary = strings.TrimLeft(fields[0], "@-:+!")
	}
	if binary == "" {
		return "", errors.New("could not find ExecStart= in the service unit")
	}
	if !safePathPattern.MatchString(binary) {
		return "", fmt.Errorf("unexpected service binary path %q", binary)
	}
	return binary, nil
}

func binaryVersion(run runFunction, binaryPath string) (cloudflaredVersion, error) {
	result, err := run(binaryPath + " --version")
	if err != nil {
		return cloudflaredVersion{}, err
	}
	if result.exitCode != 0 {
		return cloudflaredVersion{}, fmt.Errorf("%s --version exited %d", binaryPath, result.exitCode)
	}
	return parseVersion(result.stdout + result.stderr)
}

func mainProcessID(run runFunction, service string) (int, error) {
	result, err := run("systemctl show -p MainPID --value " + service)
	if err != nil {
		return 0, err
	}
	processID, _ := strconv.Atoi(strings.TrimSpace(result.stdout))
	return processID, nil
}

func runningBinaryReplaced(run runFunction, processID int) bool {
	if processID == 0 {
		return false
	}
	result, err := run("sudo -n readlink /proc/" + strconv.Itoa(processID) + "/exe")
	return err == nil && strings.Contains(result.stdout, "(deleted)")
}

func upgradeWithApt(run runFunction, logf func(string, ...any)) error {
	logf("running apt-get update")
	result, err := run("sudo -n apt-get update -q")
	if err != nil {
		return err
	}
	if result.exitCode != 0 {
		return fmt.Errorf("apt-get update exited %d: %s", result.exitCode, lastLines(result.stderr, 3))
	}
	logf("running apt-get install --only-upgrade cloudflared")
	result, err = run("sudo -n apt-get install -y -q --only-upgrade cloudflared")
	if err != nil {
		return err
	}
	if result.exitCode != 0 {
		return fmt.Errorf("apt-get install exited %d: %s", result.exitCode, lastLines(result.stdout+result.stderr, 3))
	}
	return nil
}

func installLatestDebFromGitHub(run runFunction) error {
	command := `set -e
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
architecture=$(dpkg --print-architecture)
curl -fsSL -o "$temporary/cloudflared.deb" "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$architecture.deb"
sudo -n dpkg -i "$temporary/cloudflared.deb"`
	result, err := run(command)
	if err != nil {
		return err
	}
	if result.exitCode != 0 {
		return fmt.Errorf("installing the GitHub .deb exited %d: %s", result.exitCode, lastLines(result.stdout+result.stderr, 3))
	}
	return nil
}

func waitForTunnel(ctx context.Context, run runFunction, report *hostReport, pidBefore int, options hostOptions, logf func(string, ...any)) error {
	service := options.serviceName
	deadline := time.Now().Add(options.verifyTimeout)
	lastState := ""
	for {
		result, err := run("systemctl is-active " + service)
		if err != nil {
			return err
		}
		lastState = strings.TrimSpace(result.stdout)

		processID, err := mainProcessID(run, service)
		if err != nil {
			return err
		}
		if lastState == "active" && processID != 0 && processID != pidBefore {
			// Only read log lines from the new process, so the old process's
			// shutdown errors ("context canceled") are ignored.
			result, err = run("sudo -n journalctl --no-pager -o cat -u " + service + " _PID=" + strconv.Itoa(processID))
			if err != nil {
				return err
			}
			report.registeredConnections = strings.Count(result.stdout, registeredConnectionMarker)
			if strings.Contains(result.stdout, "x509") {
				return errors.New("the new cloudflared reports certificate errors; an HTTPS origin may use an affected certificate. Replace that certificate (do not turn on noTLSVerify)")
			}
			if report.registeredConnections >= options.expectedConnections {
				logf("tunnel healthy: %d registered connections (process %d)", report.registeredConnections, processID)
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("after %s the service is %q with %d of %d registered connections", options.verifyTimeout, lastState, report.registeredConnections, options.expectedConnections)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(options.pollInterval):
		}
	}
}

// findStaleCopies lists other cloudflared binaries on PATH that are not the
// one the service runs (for example an old /usr/local/bin/cloudflared).
func findStaleCopies(run runFunction, serviceBinaryPath string) ([]string, error) {
	result, err := run("readlink -f " + serviceBinaryPath)
	if err != nil {
		return nil, err
	}
	serviceReal := strings.TrimSpace(result.stdout)

	result, err = run("which -a cloudflared | xargs -r readlink -f | sort -u")
	if err != nil {
		return nil, err
	}
	var staleCopies []string
	for _, path := range strings.Fields(result.stdout) {
		if path != serviceReal && safePathPattern.MatchString(path) {
			staleCopies = append(staleCopies, path)
		}
	}
	return staleCopies, nil
}

func finishWithStaleCopies(run runFunction, report *hostReport, options hostOptions, logf func(string, ...any)) hostReport {
	if !options.removeStaleCopies || len(report.staleCopies) == 0 {
		return *report
	}
	var kept []string
	for _, copyPath := range report.staleCopies {
		owned, _ := run("dpkg -S " + copyPath)
		if owned.exitCode == 0 {
			logf("keeping %s: a package owns it", copyPath)
			kept = append(kept, copyPath)
			continue
		}
		if result, err := run("sudo -n rm -f " + copyPath); err != nil || result.exitCode != 0 {
			logf("could not remove %s", copyPath)
			kept = append(kept, copyPath)
			continue
		}
		logf("removed stale copy %s", copyPath)
	}
	report.staleCopies = kept
	return *report
}

func aptCandidate(policyText string) string {
	for _, line := range strings.Split(policyText, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Candidate:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Candidate:"))
		}
	}
	return ""
}

func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, " | ")
}
