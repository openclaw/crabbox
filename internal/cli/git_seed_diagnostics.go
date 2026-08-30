package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

const gitSeedDiagnosticLimit = 16 << 10

// Remote output is untrusted, including helper output and URLs. Only fixed
// labels leave this classifier; truncated captures are discarded by the reader.
func warnRemoteGitSeedFailure(w io.Writer, output string, err error) {
	phase, reason := "unknown", "unknown"
	lines := strings.Split(output, "\n")
	detailStart := 0
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case "crabbox-git-seed phase=prerequisite":
			phase = "prerequisite"
		case "crabbox-git-seed phase=prepare":
			phase = "prepare"
		case "crabbox-git-seed phase=clone":
			phase = "clone"
		case "crabbox-git-seed phase=checkout":
			phase = "checkout"
		case "crabbox-git-seed phase=verify":
			phase = "verify"
		case "crabbox-git-seed phase=origin":
			phase = "origin"
		case "crabbox-git-seed phase=publish":
			phase = "publish"
		default:
			continue
		}
		detailStart = i + 1
	}
	text := strings.ToLower(strings.Join(lines[detailStart:], "\n"))
	contains := func(patterns ...string) bool {
		for _, pattern := range patterns {
			if strings.Contains(text, pattern) {
				return true
			}
		}
		return false
	}
	switch {
	case phase == "prerequisite":
		reason = "missing-git"
	case contains("authentication failed", "could not read username", "terminal prompts disabled", "permission denied (publickey", "access denied"):
		reason = "authentication/access"
	case contains("could not resolve host", "could not resolve hostname", "name or service not known", "temporary failure in name resolution"):
		reason = "dns"
	case contains("ssl certificate problem", "certificate verify failed", "server certificate verification failed", "ssl connect error", "schannel:"):
		reason = "tls"
	case contains("failed to connect", "connection timed out", "connection refused", "network is unreachable", "couldn't connect"):
		reason = "connectivity"
	case contains("does not appear to be a git repository", "not a git repository", "couldn't find remote ref", "invalid reference", "not our ref", "unable to read tree") || (contains("repository", "remote branch") && contains("not found", "does not exist")):
		reason = "repository/ref"
	case phase == "verify":
		reason = "verification"
	}
	status := "unknown"
	var exitErr *exec.ExitError
	switch {
	case errors.Is(err, context.Canceled):
		status = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		status = "deadline"
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		status = strconv.Itoa(exitErr.ExitCode())
	}
	fmt.Fprintf(w, "warning: remote git seed failed: phase=%s reason=%s exit=%s; continuing with file sync; Git metadata was not seeded or verified\n", phase, reason, status)
}
