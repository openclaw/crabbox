package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
)

type FreshPRSpec struct {
	Owner  string
	Repo   string
	Number int
}

func parseFreshPRSpec(value string, local Repo) (FreshPRSpec, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return FreshPRSpec{}, nil
	}
	if n, err := strconv.Atoi(value); err == nil && n > 0 {
		owner, repo := githubOwnerRepoFromRemote(local.RemoteURL)
		if owner == "" || repo == "" {
			return FreshPRSpec{}, exit(2, "--fresh-pr <number> requires a GitHub origin remote")
		}
		return FreshPRSpec{Owner: owner, Repo: repo, Number: n}, nil
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		u, err := url.Parse(value)
		if err != nil {
			return FreshPRSpec{}, exit(2, "invalid --fresh-pr URL: %v", err)
		}
		if !isGitHubHost(u.Hostname()) {
			return FreshPRSpec{}, exit(2, "--fresh-pr URL host must be github.com")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 4 && parts[2] == "pull" {
			n, err := strconv.Atoi(parts[3])
			if err == nil && n > 0 {
				return FreshPRSpec{Owner: parts[0], Repo: parts[1], Number: n}, nil
			}
		}
		return FreshPRSpec{}, exit(2, "--fresh-pr URL must look like https://github.com/owner/repo/pull/123")
	}
	re := regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)#([0-9]+)$`)
	match := re.FindStringSubmatch(value)
	if match == nil {
		return FreshPRSpec{}, exit(2, "--fresh-pr expects owner/repo#123, GitHub PR URL, or PR number")
	}
	n, _ := strconv.Atoi(match[3])
	if n <= 0 {
		return FreshPRSpec{}, exit(2, "--fresh-pr number must be positive")
	}
	return FreshPRSpec{Owner: match[1], Repo: match[2], Number: n}, nil
}

func githubOwnerRepoFromRemote(remote string) (string, string) {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimSuffix(remote, ".git")
	if strings.HasPrefix(remote, "git@github.com:") {
		parts := strings.Split(strings.TrimPrefix(remote, "git@github.com:"), "/")
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
	}
	if u, err := url.Parse(remote); err == nil && isGitHubHost(u.Hostname()) {
		remotePath := strings.Trim(u.Path, "/")
		if u.Scheme == "ssh" && strings.HasPrefix(remotePath, "~") {
			remotePath = strings.TrimPrefix(remotePath, "~")
		}
		parts := strings.Split(remotePath, "/")
		if len(parts) >= 2 {
			return parts[0], parts[1]
		}
	}
	return "", ""
}

func isGitHubHost(host string) bool {
	return strings.EqualFold(strings.TrimSpace(host), "github.com")
}

func (s FreshPRSpec) Empty() bool {
	return s.Owner == "" || s.Repo == "" || s.Number == 0
}

func (s FreshPRSpec) Slug() string {
	if s.Empty() {
		return ""
	}
	return fmt.Sprintf("%s/%s#%d", s.Owner, s.Repo, s.Number)
}

func (s FreshPRSpec) WorkdirName() string {
	if s.Empty() {
		return ""
	}
	return fmt.Sprintf("fresh-pr-%s-%s-%d", safeCaptureName(s.Owner), safeCaptureName(s.Repo), s.Number)
}

func remoteFreshPRCheckoutCommand(workdir string, spec FreshPRSpec) string {
	parent := path.Dir(workdir)
	repoURL := fmt.Sprintf("https://github.com/%s/%s.git", spec.Owner, spec.Repo)
	branch := fmt.Sprintf("crabbox-pr-%d", spec.Number)
	ref := fmt.Sprintf("pull/%d/head:%s", spec.Number, branch)
	script := "set -eu\n" +
		"rm -rf " + shellQuote(workdir) + "\n" +
		"mkdir -p " + shellQuote(parent) + "\n" +
		"git clone --quiet --filter=blob:none " + shellQuote(repoURL) + " " + shellQuote(workdir) + "\n" +
		"cd " + shellQuote(workdir) + "\n" +
		"git fetch --quiet origin " + shellQuote(ref) + "\n" +
		"git checkout --quiet " + shellQuote(branch) + "\n"
	return "bash -lc " + shellQuote(script)
}

func remoteFreshPRCheckoutCommandForTarget(workdir string, spec FreshPRSpec, target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return windowsRemoteFreshPRCheckoutCommand(workdir, spec)
	}
	return remoteFreshPRCheckoutCommand(workdir, spec)
}

func windowsRemoteFreshPRCheckoutCommand(workdir string, spec FreshPRSpec) string {
	repoURL := fmt.Sprintf("https://github.com/%s/%s.git", spec.Owner, spec.Repo)
	branch := fmt.Sprintf("crabbox-pr-%d", spec.Number)
	ref := fmt.Sprintf("pull/%d/head:%s", spec.Number, branch)
	return powershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workdir) + `
$parent = Split-Path -Parent $workdir
if (Test-Path -LiteralPath $workdir) {
  Remove-Item -LiteralPath $workdir -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $parent | Out-Null
git clone --quiet --filter=blob:none ` + psQuote(repoURL) + ` $workdir
if ($LASTEXITCODE -ne 0) { throw "git clone failed with exit $LASTEXITCODE" }
Set-Location -LiteralPath $workdir
git fetch --quiet origin ` + psQuote(ref) + `
if ($LASTEXITCODE -ne 0) { throw "git fetch failed with exit $LASTEXITCODE" }
git checkout --quiet ` + psQuote(branch) + `
if ($LASTEXITCODE -ne 0) { throw "git checkout failed with exit $LASTEXITCODE" }
`)
}

func localGitBinaryDiff(root string) ([]byte, error) {
	cmd := exec.Command("git", "diff", "--binary", "HEAD")
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	return cmd.Output()
}

func remoteApplyLocalPatchCommand(workdir string) string {
	script := "set -eu\ncd " + shellQuote(workdir) + "\ngit apply --whitespace=nowarn -\n"
	return "bash -lc " + shellQuote(script)
}

func remoteApplyLocalPatchCommandForTarget(workdir string, target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$ErrorActionPreference = "Stop"
Set-Location -LiteralPath ` + psQuote(workdir) + `
git apply --whitespace=nowarn -
if ($LASTEXITCODE -ne 0) { throw "git apply failed with exit $LASTEXITCODE" }
`)
	}
	return remoteApplyLocalPatchCommand(workdir)
}

func applyLocalPatchToFreshPR(ctx context.Context, target SSHTarget, workdir string, repo Repo) (bool, error) {
	diff, err := localGitBinaryDiff(repo.Root)
	if err != nil {
		return false, exit(2, "create local patch: %v", err)
	}
	if len(diff) == 0 {
		return false, nil
	}
	if err := runSSHInput(ctx, target, remoteApplyLocalPatchCommandForTarget(workdir, target), bytes.NewReader(diff), io.Discard, io.Discard); err != nil {
		return false, exit(7, "apply local patch: %v", err)
	}
	return true, nil
}
