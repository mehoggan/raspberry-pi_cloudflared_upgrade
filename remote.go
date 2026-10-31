package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// commandResult is what one remote command produced.
type commandResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// commandRunner runs a shell command on one host. The error is only for
// transport problems (SSH could not connect); a command that ran and failed is
// reported through exitCode.
type commandRunner interface {
	run(ctx context.Context, command string) (commandResult, error)
}

// sshRunner runs commands through the system `ssh` binary, so it honours
// ~/.ssh/config, ssh-agent, known_hosts, ProxyJump and so on.
type sshRunner struct {
	host              string
	user              string
	port              int
	connectionTimeout time.Duration
	extraOptions      []string
}

// sshTransportFailure is the exit code ssh itself uses when it cannot connect.
const sshTransportFailure = 255

func (r sshRunner) run(ctx context.Context, command string) (commandResult, error) {
	arguments := []string{
		"-o", "BatchMode=yes", // never prompt; fail instead
		"-o", "ConnectTimeout=" + strconv.Itoa(int(r.connectionTimeout.Seconds())),
		"-o", "ServerAliveInterval=10",
	}
	if r.port != 0 {
		arguments = append(arguments, "-p", strconv.Itoa(r.port))
	}
	if r.user != "" {
		arguments = append(arguments, "-l", r.user)
	}
	arguments = append(arguments, r.extraOptions...)
	// Wrap in `sh -c` so the command behaves the same whatever the remote
	// login shell is (bash, zsh, ...).
	arguments = append(arguments, r.host, "sh -c "+shellQuote(command))

	process := exec.CommandContext(ctx, "ssh", arguments...)
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	err := process.Run()

	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.exitCode = exitError.ExitCode()
		if result.exitCode == sshTransportFailure {
			return result, fmt.Errorf("ssh to %s failed: %s", r.host, strings.TrimSpace(result.stderr))
		}
		return result, nil
	}
	return result, fmt.Errorf("could not start ssh: %w", err)
}

// shellQuote wraps text in single quotes for a POSIX shell.
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'"'"'`) + "'"
}
