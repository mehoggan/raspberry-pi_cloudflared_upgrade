// raspberry-pi-cloudflared-upgrade upgrades cloudflared on a list of hosts over SSH,
// one host at a time, and stops at the first failure so the remaining
// tunnels keep serving traffic.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"
)

// defaultMinimumVersion is the first release that no longer trusts the
// compromised eMudhra (emSign) intermediate certificates (Cloudflare notice,
// October 2026).
const defaultMinimumVersion = "2026.10.2"

func main() {
	hosts := flag.String("hosts", "", "comma-separated hosts or ~/.ssh/config aliases, upgraded in this order (required)")
	user := flag.String("user", "", "SSH user (default: from ~/.ssh/config or your local user)")
	port := flag.Int("port", 0, "SSH port (default: from ~/.ssh/config or 22)")
	minimumVersion := flag.String("minimum-version", defaultMinimumVersion, "fail if a host ends up older than this")
	serviceName := flag.String("service", "cloudflared.service", "systemd service to upgrade")
	expectedConnections := flag.Int("expected-connections", 4, "registered tunnel connections required before a host counts as healthy")
	dryRun := flag.Bool("dry-run", false, "show what would happen; change nothing")
	forceRestart := flag.Bool("force-restart", false, "restart the service even if the version did not change")
	removeStaleCopies := flag.Bool("remove-stale-copies", false, "delete other cloudflared binaries on PATH that no package owns")
	continueOnFailure := flag.Bool("continue-on-failure", false, "keep going to the next host after a failure")
	connectionTimeout := flag.Duration("connection-timeout", 15*time.Second, "SSH connect timeout")
	verifyTimeout := flag.Duration("verify-timeout", 90*time.Second, "how long to wait for the tunnel to reconnect after a restart")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s -hosts 192.168.1.2,192.168.1.4 [options]\n\nOptions:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	hostList := splitHosts(*hosts)
	if len(hostList) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	minimum, err := parseVersion(*minimumVersion)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid -minimum-version:", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	options := hostOptions{
		serviceName:         *serviceName,
		minimumVersion:      minimum,
		dryRun:              *dryRun,
		forceRestart:        *forceRestart,
		removeStaleCopies:   *removeStaleCopies,
		expectedConnections: *expectedConnections,
		verifyTimeout:       *verifyTimeout,
		pollInterval:        3 * time.Second,
		log: func(format string, arguments ...any) {
			fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, arguments...))
		},
	}

	var reports []hostReport
	stopped := false
	for _, host := range hostList {
		if stopped {
			reports = append(reports, hostReport{host: host, outcome: outcomeSkipped})
			continue
		}
		runner := sshRunner{host: host, user: *user, port: *port, connectionTimeout: *connectionTimeout}
		report := upgradeHost(ctx, runner, host, options)
		reports = append(reports, report)
		if report.err != nil && !*continueOnFailure {
			options.log("stopping here so the remaining hosts keep serving; fix the problem and run again")
			stopped = true
		}
		if ctx.Err() != nil {
			stopped = true
		}
	}

	printSummary(reports)
	for _, report := range reports {
		if report.outcome == outcomeFailed || report.outcome == outcomeSkipped {
			os.Exit(1)
		}
	}
}

func splitHosts(text string) []string {
	var hosts []string
	for _, host := range strings.Split(text, ",") {
		if host = strings.TrimSpace(host); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func printSummary(reports []hostReport) {
	fmt.Println()
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "HOST\tHOSTNAME\tINSTALLED AS\tBEFORE\tAFTER\tCONNECTIONS\tRESULT")
	for _, report := range reports {
		connections := "-"
		if report.restarted {
			connections = fmt.Sprint(report.registeredConnections)
		}
		result := report.outcome
		if report.err != nil {
			result += ": " + report.err.Error()
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			report.host, dash(report.hostname), dash(report.installMethod),
			report.versionBefore, report.versionAfter, connections, result)
	}
	table.Flush()
	for _, report := range reports {
		for _, copyPath := range report.staleCopies {
			fmt.Printf("\nnote: %s still has another cloudflared at %s (rerun with -remove-stale-copies to delete it)\n", report.host, copyPath)
		}
	}
}

func dash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}
