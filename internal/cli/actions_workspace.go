package cli

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

func readActionsWorkspace(ctx context.Context, target SSHTarget, leaseID string, repo Repo) (actionsHydrationState, error) {
	state, err := readActionsWorkspaceState(ctx, target, leaseID)
	if err != nil {
		return actionsHydrationState{}, err
	}
	if state.Workspace != "" {
		if err := verifyActionsWorkspace(ctx, target, repo, state); err != nil {
			return actionsHydrationState{}, err
		}
	}
	return state, nil
}

func readActionsWorkspaceState(ctx context.Context, target SSHTarget, leaseID string) (actionsHydrationState, error) {
	ctx, cancel := context.WithTimeout(contextWithoutWorkspaceOwner(ctx), 30*time.Second)
	defer cancel()
	marker, err := RunSSHOutputBoundedWithExecutionTimeout(ctx, target, remoteReadActionsWorkspaceState(target, leaseID), 64*1024, 15*time.Second)
	if err != nil {
		return actionsHydrationState{}, errors.Join(Exit(7, "cannot verify Actions workspace marker; refusing workspace adoption"), err)
	}
	if marker == "" {
		return actionsHydrationState{}, nil
	}
	state := normalizeActionsHydrationStateForTarget(target, parseActionsHydrationState(marker))
	if state.Workspace == "" {
		return actionsHydrationState{}, Exit(2, "Actions workspace marker has no workspace; refusing workspace adoption")
	}
	return state, nil
}

func remoteReadActionsWorkspaceState(target SSHTarget, leaseID string) string {
	if isWindowsNativeTarget(target) {
		return PowershellCommand(`$ErrorActionPreference = "Stop"
$path = ` + psQuote(windowsActionsHydrationPath(actionsHydrationStatePath(leaseID))) + `
$markerError = $null
$item = Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue -ErrorVariable markerError
if ($markerError) {
  if ($markerError[0].CategoryInfo.Category -eq 'ObjectNotFound') { exit 0 }
  exit 2
}
Write-Output 'MARKER_PRESENT=1'
Get-Content -Raw -LiteralPath $path -ErrorAction Stop
`)
	}
	// Prove absence through searchable parents; a hidden or unreadable marker
	// must not authorize hydration to replace it. Presence also rejects empty files.
	return remotePOSIXControlCommand(`set -e
[ -n "$HOME" ] && [ -d "$HOME" ] && [ -x "$HOME" ] || exit 2
for dir in "$HOME/.crabbox" "$HOME/.crabbox/actions"; do
  if [ -e "$dir" ] || [ -L "$dir" ]; then
    [ -d "$dir" ] && [ -x "$dir" ] || exit 2
  else
    exit 0
  fi
done
marker="$HOME"/` + shellQuote(actionsHydrationStatePath(leaseID)) + `
if [ -e "$marker" ] || [ -L "$marker" ]; then
  printf 'MARKER_PRESENT=1\n'
  cat "$HOME"/` + shellQuote(actionsHydrationStatePath(leaseID)) + `
fi
`)
}

func verifyActionsWorkspace(ctx context.Context, target SSHTarget, repo Repo, state actionsHydrationState) error {
	expected := actionsRepositoryIdentity(repo.RemoteURL)
	if expected == "" || state.Workspace == "" {
		return Exit(2, "cannot verify Actions workspace without an invoking repository origin and workspace; use a different lease or the repository that owns this workspace")
	}
	// A lease marker is not repository identity. Verify before adopting its path,
	// environment, or metadata; treating a mismatch as absence would let hydration
	// invalidate a foreign workspace's marker.
	ctx, cancel := context.WithTimeout(contextWithoutWorkspaceOwner(ctx), 30*time.Second)
	defer cancel()
	origin, err := RunSSHOutputBoundedWithExecutionTimeout(ctx, target, remoteActionsWorkspaceOrigin(target, state.Workspace), 4096, 15*time.Second)
	if err != nil {
		return errors.Join(Exit(7, "cannot verify Actions workspace Git root and origin; refusing workspace adoption"), err)
	}
	actual := actionsRepositoryIdentity(origin)
	if actual == "" {
		return Exit(2, "cannot verify Actions workspace origin; refusing workspace adoption")
	}
	if actual != expected {
		return Exit(2, "Actions workspace repository does not match the invoking repository; use a different lease or the repository that owns this workspace")
	}
	return nil
}

func remoteActionsWorkspaceOrigin(target SSHTarget, workspace string) string {
	if isWindowsNativeTarget(target) {
		return PowershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workspace) + `
Get-Command git -ErrorAction Stop | Out-Null
` + windowsGitRootIdentityScript() + `
if (-not (Test-Path -LiteralPath $workdir -PathType Container)) { exit 2 }
$root = & git -C $workdir rev-parse --show-toplevel 2>$null
if ($LASTEXITCODE -ne 0 -or -not $root) { exit 2 }
if (-not (Test-CrabboxSameDirectory $workdir ([string]$root))) { exit 2 }
& git -C $workdir remote get-url origin
exit $LASTEXITCODE
`)
	}
	return remotePOSIXControlCommand("cd " + shellPathQuote(workspace) + " || exit 2\n" + remoteExactGitRootFunction() + `
exact_git_root || exit 2
git remote get-url origin
`)
}

func actionsRepositoryIdentity(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" || strings.ContainsAny(remote, "\r\n\x00") {
		return ""
	}
	// GitHub's SSH and HTTPS clone URLs name the same repository. Keep other
	// origins exact: arbitrary SSH relative paths, ports, and users are identities.
	githubRemote := normalizeGitRemoteURL(remote)
	if parsed, err := url.Parse(remote); err == nil && strings.EqualFold(parsed.Host, "github.com") && parsed.RawQuery == "" && parsed.Fragment == "" {
		switch strings.ToLower(parsed.Scheme) {
		case "https", "http", "git":
			githubRemote = "https://github.com" + parsed.Path
		case "ssh":
			if parsed.User == nil || parsed.User.Username() == "git" {
				githubRemote = "https://github.com" + parsed.Path
			}
		}
	}
	if strings.HasPrefix(githubRemote, "https://github.com/") {
		repo := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(githubRemote, "https://github.com/"), "/"), ".git")
		parts := strings.Split(repo, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" && parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".." && !strings.ContainsAny(repo, "?#") {
			return "https://github.com/" + strings.ToLower(repo)
		}
	}
	return remote
}
