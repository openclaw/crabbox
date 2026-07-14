package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func trustedReadyPoolRemoteURL(remoteURL string) (string, error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", exit(7, "ready-pool reuse requires a canonical local Git origin")
	}
	if gitRemoteURLHasCredentials(remoteURL) {
		return "", exit(7, "ready-pool reuse refuses a credential-bearing local Git origin")
	}
	if strings.HasPrefix(remoteURL, "ssh://") || (!strings.Contains(remoteURL, "://") && strings.Contains(remoteURL, "@")) {
		return "", exit(7, "ready-pool reuse requires an anonymously fetchable non-SSH Git origin")
	}
	canonical := normalizeGitRemoteURL(remoteURL)
	if canonical == "" {
		return "", exit(7, "ready-pool reuse could not normalize the local Git origin")
	}
	parsed, err := url.Parse(canonical)
	if err != nil || parsed.User != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", exit(7, "ready-pool reuse requires an anonymously fetchable HTTPS Git origin")
	}
	return canonical, nil
}

func preflightReadyPoolRemote(ctx context.Context, remoteURL string) error {
	preflightCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	workdir, err := os.MkdirTemp("", "crabbox-ready-pool-preflight-")
	if err != nil {
		return exit(7, "create ready-pool origin preflight directory")
	}
	defer os.RemoveAll(workdir)
	configNull := "/dev/null"
	if runtime.GOOS == "windows" {
		configNull = "NUL"
	}
	cmd := exec.CommandContext(preflightCtx, "git", "-c", "credential.helper=", "ls-remote", "--exit-code", remoteURL, "HEAD")
	cmd.Dir = workdir
	cmd.Env = []string{
		"HOME=" + workdir,
		"USERPROFILE=" + workdir,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + configNull,
		"GIT_CONFIG_SYSTEM=" + configNull,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=Never",
	}
	if err := cmd.Run(); err != nil {
		return exit(7, "ready-pool reuse requires an anonymously fetchable Git origin before borrowing")
	}
	return nil
}

func (a App) scrubReadyPoolLease(ctx context.Context, target SSHTarget, entry CoordinatorReadyPoolEntry, workdir, trustedRemoteURL string, requireActionsHydration bool) (string, error) {
	if strings.TrimSpace(workdir) == "" {
		return "", exit(7, "ready-pool scrub has no remote workdir")
	}
	branch, err := readyPoolScrubBranch(entry.Ref)
	if err != nil {
		return "", exit(7, "ready-pool scrub requires a branch ref")
	}
	if strings.TrimSpace(trustedRemoteURL) == "" {
		return "", exit(7, "ready-pool scrub has no trusted Git origin")
	}
	command := remoteReadyPoolScrub(workdir, branch, trustedRemoteURL)
	if isWindowsNativeTarget(target) {
		command = windowsRemoteReadyPoolScrub(workdir, branch, trustedRemoteURL)
	}
	out, err := runSSHOutput(ctx, target, command)
	if err != nil {
		return "", exit(7, "ready-pool scrub failed on %s: %v", target.Host, err)
	}
	preparedCommit := strings.TrimSpace(out)
	if !isGitCommitSHA(preparedCommit) {
		return "", exit(7, "ready-pool scrub did not report one valid prepared commit")
	}
	if requireActionsHydration {
		state, err := readActionsHydrationState(ctx, target, entry.LeaseID)
		if err != nil {
			return "", exit(7, "read ready-pool Actions hydration marker: %v", err)
		}
		if strings.TrimSpace(state.Workspace) == "" || strings.TrimSpace(state.Workspace) != strings.TrimSpace(workdir) {
			return "", exit(7, "ready-pool Actions hydration marker no longer owns the prepared workspace")
		}
	}
	return preparedCommit, nil
}

func readyPoolScrubBranch(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "refs/heads/") {
		ref = strings.TrimPrefix(ref, "refs/heads/")
		if ref != "" {
			return ref, nil
		}
	}
	if ref == "" || strings.HasPrefix(ref, "refs/") || isGitCommitSHA(ref) {
		return "", fmt.Errorf("ready-pool scrub requires a branch ref")
	}
	return ref, nil
}

func remoteReadyPoolScrub(workdir, ref, trustedRemoteURL string) string {
	script := `set -euo pipefail
workdir=` + shellQuote(workdir) + `
ref=` + shellQuote(strings.TrimSpace(ref)) + `
trusted_remote=` + shellQuote(strings.TrimSpace(trustedRemoteURL)) + `
if [ -L "$workdir" ] || [ ! -d "$workdir" ]; then
  echo "ready-pool workspace root must be a real directory" >&2
  exit 1
fi
resolved_workdir="$(cd -P -- "$workdir" && pwd -P)"
workdir="$resolved_workdir"
cd "$resolved_workdir"
test -d .git
parent="$(dirname -- "$workdir")"
tmp="$(mktemp -d "$parent/.crabbox-scrub.XXXXXX")"
safe_home="$tmp/home"
mkdir -p "$safe_home"
trap 'rm -rf -- "$tmp"' EXIT
safe_git() {
  /usr/bin/env -i HOME="$safe_home" PATH="/usr/bin:/bin" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_TERMINAL_PROMPT=0 /usr/bin/git "$@"
}
test -x /usr/bin/git
safe_git check-ref-format --branch "$ref" >/dev/null
safe_git init --quiet "$tmp"
safe_git -C "$tmp" remote add origin "$trusted_remote"
safe_git -C "$tmp" fetch --quiet --prune --tags origin '+refs/heads/*:refs/remotes/origin/*'
remote_ref="refs/remotes/origin/$ref"
remote_commit="$(safe_git -C "$tmp" rev-parse --verify "$remote_ref^{commit}")"
target_commit="$remote_commit"
safe_git -C "$tmp" read-tree "$target_commit"
filter_paths="$tmp/filter-paths"
safe_git -C "$tmp" ls-files -z -- ':(top)**' ':(top,exclude,attr:!filter)**' ':(top,exclude,attr:-filter)**' > "$filter_paths"
if [ -s "$filter_paths" ]; then
  echo "ready-pool scrub does not reuse Git filter-managed worktrees" >&2
  exit 1
fi
rm -f -- "$filter_paths"
rm -rf -- .git
mv -- "$tmp/.git" .git
rm -rf -- "$safe_home"
rmdir -- "$tmp"
trap - EXIT
safe_git remote set-url origin "$trusted_remote"
test "$(safe_git remote get-url origin)" = "$trusted_remote"
safe_git checkout --quiet -f -B "$ref" "$target_commit"
safe_git reset --hard --quiet "$target_commit"
safe_git branch --set-upstream-to="$remote_ref" "$ref" >/dev/null
if safe_git ls-files --stage | awk '$1 == "160000" { found=1 } END { exit !found }'; then
  echo "ready-pool scrub does not reuse submodule worktrees" >&2
  exit 1
fi
clean_args=(-ffdx --quiet)
for cache_path in node_modules .pnpm-store .yarn/cache .yarn/unplugged; do
  if [ -e "$cache_path" ]; then
    if safe_git check-ignore -q -- "$cache_path"; then
      if [ -L "$cache_path" ] || [ ! -d "$cache_path" ]; then
        echo "ready-pool cache root must be a real directory" >&2
        exit 1
      fi
      resolved_cache="$(cd -P -- "$cache_path" && pwd -P)"
      case "$resolved_cache/" in
        "$workdir"/*) ;;
        *)
          echo "ready-pool cache root escapes the workspace" >&2
          exit 1
          ;;
      esac
      clean_args+=(-e "$cache_path/")
    elif [ "$?" -ne 1 ]; then
      echo "ready-pool cache ignore check failed" >&2
      exit 1
    fi
  fi
done
safe_git clean "${clean_args[@]}"
if [ -L .crabbox ]; then
  echo "ready-pool .crabbox root must not be a symlink" >&2
  exit 1
fi
if [ -e .crabbox ] && [ ! -d .crabbox ]; then
  echo "ready-pool .crabbox root must be a directory" >&2
  exit 1
fi
rm -rf -- .crabbox/env .crabbox/scripts .crabbox/logs .crabbox/captures .crabbox/runs
git_dir="$(safe_git rev-parse --git-dir)"
meta_dir="$git_dir/crabbox"
mkdir -p "$meta_dir"
rm -f -- "$meta_dir/sync-fingerprint" "$meta_dir/sync-manifest" "$meta_dir/sync-manifest.new" "$meta_dir/sync-deleted.new"
printf '%s %s\n' "$ref" "$target_commit" > "$meta_dir/git-hydrate-base"
test "$(safe_git branch --show-current)" = "$ref"
test "$(safe_git rev-parse HEAD)" = "$target_commit"
if ! status="$(safe_git status --porcelain --untracked-files=normal)"; then
  echo "ready-pool Git status failed" >&2
  exit 1
fi
test -z "$status"
printf '%s\n' "$target_commit"`
	return "/bin/bash --noprofile --norc -c " + shellQuote(script)
}

func windowsRemoteReadyPoolScrub(workdir, ref, trustedRemoteURL string) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workdir) + `
$ref = ` + psQuote(strings.TrimSpace(ref)) + `
$trustedRemote = ` + psQuote(strings.TrimSpace(trustedRemoteURL)) + `
# Borrowed commands may persist user-scoped Git variables. Clear the complete
# inherited Git surface before installing the scrub's small trusted set.
Get-ChildItem Env:GIT_* -ErrorAction SilentlyContinue | Remove-Item -ErrorAction Stop
$env:GIT_CONFIG_NOSYSTEM = '1'
$env:GIT_CONFIG_GLOBAL = 'NUL'
$env:GIT_CONFIG_SYSTEM = 'NUL'
$env:GIT_TERMINAL_PROMPT = '0'
$gitCandidates = @()
if ($env:ProgramFiles) {
  $gitCandidates += (Join-Path $env:ProgramFiles 'Git\cmd\git.exe')
  $gitCandidates += (Join-Path $env:ProgramFiles 'Git\bin\git.exe')
}
if (${env:ProgramFiles(x86)}) {
  $gitCandidates += (Join-Path ${env:ProgramFiles(x86)} 'Git\cmd\git.exe')
  $gitCandidates += (Join-Path ${env:ProgramFiles(x86)} 'Git\bin\git.exe')
}
$gitCandidates = $gitCandidates | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf }
$git = $gitCandidates | Select-Object -First 1
if (-not $git) { throw "ready-pool scrub could not locate trusted Git under Program Files" }
$workdirItem = Get-Item -LiteralPath $workdir -Force
if (-not $workdirItem.PSIsContainer -or ($workdirItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
  throw "ready-pool workspace root must be a real directory"
}
$resolvedWorkdir = $workdirItem.FullName
$workdir = $resolvedWorkdir
Set-Location -LiteralPath $resolvedWorkdir
if (-not (Test-Path -LiteralPath (Join-Path $workdir '.git') -PathType Container)) { throw "ready-pool workspace has no replaceable Git metadata" }
& $git check-ref-format --branch $ref | Out-Null
if ($LASTEXITCODE -ne 0) { throw "invalid ready-pool branch ref" }
$parent = Split-Path -Parent $workdir
$tmp = Join-Path $parent ('.crabbox-scrub-' + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$safeHome = Join-Path $tmp 'home'
New-Item -ItemType Directory -Force -Path $safeHome | Out-Null
$env:HOME = $safeHome
$env:USERPROFILE = $safeHome
& $git init --quiet $tmp
if ($LASTEXITCODE -ne 0) { throw "ready-pool temporary Git init failed" }
& $git -C $tmp remote add origin $trustedRemote
if ($LASTEXITCODE -ne 0) { throw "ready-pool trusted origin setup failed" }
& $git -C $tmp fetch --quiet --prune --tags origin '+refs/heads/*:refs/remotes/origin/*'
if ($LASTEXITCODE -ne 0) { throw "ready-pool branch fetch failed" }
$remoteRef = "refs/remotes/origin/$ref"
$remoteCommit = (& $git -C $tmp rev-parse --verify "${remoteRef}^{commit}").Trim()
if ($LASTEXITCODE -ne 0 -or -not $remoteCommit) { throw "ready-pool remote branch is missing" }
$targetCommit = $remoteCommit
& $git -C $tmp read-tree $targetCommit
if ($LASTEXITCODE -ne 0) { throw "ready-pool target tree inspection failed" }
$filterPaths = @(& $git -C $tmp ls-files -- ':(top)**' ':(top,exclude,attr:!filter)**' ':(top,exclude,attr:-filter)**')
if ($LASTEXITCODE -ne 0) { throw "ready-pool Git filter inspection failed" }
if ($filterPaths.Count -ne 0) { throw "ready-pool scrub does not reuse Git filter-managed worktrees" }
$oldGit = Join-Path $workdir '.git'
Remove-Item -LiteralPath $oldGit -Recurse -Force -ErrorAction Stop
if (Test-Path -LiteralPath $oldGit) { throw "ready-pool old Git metadata was not removed" }
Move-Item -LiteralPath (Join-Path $tmp '.git') -Destination $oldGit -ErrorAction Stop
Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction Stop
& $git remote set-url origin $trustedRemote
if ($LASTEXITCODE -ne 0) { throw "ready-pool trusted origin reset failed" }
$actualRemote = (& $git remote get-url origin).Trim()
if ($LASTEXITCODE -ne 0 -or $actualRemote -ne $trustedRemote) { throw "ready-pool trusted origin verification failed" }
& $git checkout --quiet -f -B $ref $targetCommit
if ($LASTEXITCODE -ne 0) { throw "ready-pool branch checkout failed" }
& $git reset --hard --quiet $targetCommit
if ($LASTEXITCODE -ne 0) { throw "ready-pool branch reset failed" }
& $git branch --set-upstream-to=$remoteRef $ref | Out-Null
if ($LASTEXITCODE -ne 0) { throw "ready-pool branch upstream setup failed" }
$gitlinks = @(& $git ls-files --stage | Where-Object { $_ -match '^160000 ' })
if ($LASTEXITCODE -ne 0) { throw "ready-pool gitlink inspection failed" }
if ($gitlinks.Count -ne 0) { throw "ready-pool scrub does not reuse submodule worktrees" }
$cleanArgs = @('clean', '-ffdx', '--quiet')
foreach ($cachePath in @('node_modules', '.pnpm-store', '.yarn/cache', '.yarn/unplugged')) {
  if (Test-Path -LiteralPath $cachePath) {
    & $git check-ignore -q -- $cachePath
    if ($LASTEXITCODE -eq 0) {
      $cacheCursor = $workdir
      foreach ($segment in ($cachePath -split '/')) {
        $cacheCursor = Join-Path $cacheCursor $segment
        $cacheItem = Get-Item -LiteralPath $cacheCursor -Force
        if ($cacheItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
          throw "ready-pool cache root must not contain reparse points"
        }
      }
      if (-not $cacheItem.PSIsContainer) { throw "ready-pool cache root must be a real directory" }
      $cleanArgs += @('-e', "$cachePath/")
    } elseif ($LASTEXITCODE -ne 1) {
      throw "ready-pool cache ignore check failed"
    }
  }
}
& $git @cleanArgs
if ($LASTEXITCODE -ne 0) { throw "ready-pool branch clean failed" }
$crabboxRoot = Join-Path $workdir '.crabbox'
if (Test-Path -LiteralPath $crabboxRoot) {
  $crabboxRootItem = Get-Item -LiteralPath $crabboxRoot -Force
  if (-not $crabboxRootItem.PSIsContainer -or ($crabboxRootItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
    throw "ready-pool .crabbox root must be a real directory"
  }
}
foreach ($relative in @('.crabbox/env', '.crabbox/scripts', '.crabbox/logs', '.crabbox/captures', '.crabbox/runs')) {
  $candidate = Join-Path $workdir $relative
  if (Test-Path -LiteralPath $candidate) { Remove-Item -LiteralPath $candidate -Recurse -Force }
}
$gitDir = (& $git rev-parse --git-dir).Trim()
if ($LASTEXITCODE -ne 0 -or -not $gitDir) { throw "ready-pool Git metadata is missing" }
$metaDir = Join-Path $gitDir 'crabbox'
New-Item -ItemType Directory -Force -Path $metaDir | Out-Null
foreach ($name in @('sync-fingerprint', 'sync-manifest', 'sync-manifest.new', 'sync-deleted.new')) {
  $metadataPath = Join-Path $metaDir $name
  if (Test-Path -LiteralPath $metadataPath) {
    Remove-Item -LiteralPath $metadataPath -Force -ErrorAction Stop
    if (Test-Path -LiteralPath $metadataPath) { throw "ready-pool stale Git metadata was not removed" }
  }
}
Set-Content -LiteralPath (Join-Path $metaDir 'git-hydrate-base') -Value "$ref $targetCommit"
$branch = (& $git branch --show-current).Trim()
if ($LASTEXITCODE -ne 0 -or $branch -ne $ref) { throw "ready-pool branch verification failed" }
$head = (& $git rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $head -ne $targetCommit) { throw "ready-pool commit verification failed" }
$status = @(& $git status --porcelain --untracked-files=normal)
if ($LASTEXITCODE -ne 0 -or $status.Count -ne 0) { throw "ready-pool worktree is not clean" }
Write-Output $targetCommit
`)
}

func readyPoolScrubLifecycleError(leaseID string, scrubErr error) error {
	return fmt.Errorf("ready-pool scrub failed for %s: %w", leaseID, scrubErr)
}
