package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const gitSeedDiagnosticLimit = 16 << 10

// Remote output is untrusted, including helper output and URLs. Only fixed
// labels leave this classifier; truncated captures are discarded before parsing.
// ctx is the context the seed ran under; it attributes interrupted exits.
func warnRemoteGitSeedFailure(ctx context.Context, w io.Writer, output string, err error) {
	reportRemoteGitSeedFailure(ctx, w, output, err, "continuing with file sync")
}

func reportRemoteGitSeedFailure(ctx context.Context, w io.Writer, output string, err error, disposition string) {
	var truncated *gitOriginDiagnosticsTruncatedError
	if errors.As(err, &truncated) {
		output = ""
	}
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
	status := remoteGitSeedExitStatus(ctx, err)
	fmt.Fprintf(w, "warning: remote git seed failed: phase=%s reason=%s exit=%s; %s; Git metadata was not seeded or verified\n", phase, reason, status, disposition)
}

// remoteGitSeedExitStatus maps a seed's local process outcome to a fixed label.
// A numeric exit code is a completed remote result and is reported as-is. A
// signalled or otherwise unknown outcome under a finished context is attributed
// to that cancellation or deadline; other signals are reported by name.
func remoteGitSeedExitStatus(ctx context.Context, err error) string {
	if status := contextExitStatus(err); status != "" {
		return status
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return strconv.Itoa(exitErr.ExitCode())
	}
	// exec reports a context-killed ssh as an ordinary signal exit, so the
	// context itself is the only evidence that Crabbox interrupted the seed.
	if status := contextExitStatus(ctx.Err()); status != "" {
		return status
	}
	if exitErr != nil {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return "signal:" + signalLabel(ws.Signal())
		}
	}
	return "unknown"
}

// contextExitStatus labels context cancellation and deadline errors, or
// returns "" for any other error.
func contextExitStatus(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	}
	return ""
}

// signalLabel renders the platform's fixed signal description (for example
// "killed") as a single lowercase token safe to embed in a diagnostic field.
func signalLabel(sig syscall.Signal) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r - 'A' + 'a'
		}
		return '-'
	}, sig.String())
}
