package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	gitOverlayFallbackExitCode = 78
	gitOverlayFallbackMarker   = "CRABBOX_GIT_OVERLAY_FALLBACK:"
	gitOverlayMutationMarker   = "CRABBOX_GIT_OVERLAY_WORKSPACE_MUTATED"
)

var gitOverlayTransformAttributes = []string{
	"text", "crlf", "eol", "working-tree-encoding", "filter", "smudge", "clean", "ident",
}

type gitOverlayDecision struct {
	Requested bool
	Enabled   bool
	Reason    string
}

func decideGitOverlay(cfg Config, repo Repo, target SSHTarget, manifest SyncManifest, coherence gitCoherencePlan, credentialBlocked, fullResync, hydratedByActions bool) gitOverlayDecision {
	decision := gitOverlayDecision{Requested: cfg.Sync.GitOverlay}
	if !decision.Requested {
		return decision
	}
	switch {
	case target.TargetOS != targetLinux || isWindowsNativeTarget(target) || isWindowsWSL2Target(target):
		decision.Reason = "unsupported_target"
	case fullResync:
		decision.Reason = "full_resync"
	case hydratedByActions:
		decision.Reason = "actions_workspace"
	case !cfg.Sync.Delete:
		decision.Reason = "delete_disabled"
	case !cfg.Sync.GitSeed:
		decision.Reason = "git_seed_disabled"
	case len(syncIncludes(cfg)) != 0:
		decision.Reason = "include_whitelist"
	case credentialBlocked || gitRemoteURLHasCredentials(repo.RemoteURL):
		decision.Reason = "credential_origin"
	case !gitOverlayOriginTransportSupported(repo.RemoteURL) || !gitOverlayOriginTransportSupported(coherence.RemoteURL):
		decision.Reason = "unsupported_origin_transport"
	case !coherence.enabled():
		decision.Reason = "unseedable_head"
	default:
		if err := validateGitOverlayManifest(repo, manifest); err != nil {
			decision.Reason = gitOverlayLocalFallbackReason(err)
		} else {
			decision.Enabled = true
		}
	}
	return decision
}

func gitOverlayOriginTransportSupported(remoteURL string) bool {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" || gitRemoteURLHasCredentials(raw) {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme != "" {
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			return parsed.Host != ""
		case "file":
			return parsed.Host == "" || strings.EqualFold(parsed.Host, "localhost")
		default:
			return false
		}
	}
	return !strings.Contains(raw, ":") && !strings.HasPrefix(raw, "-")
}

func validateGitOverlayManifest(repo Repo, manifest SyncManifest) error {
	if repo.Root == "" || repo.Head == "" {
		return fmt.Errorf("missing_repo_identity")
	}
	if gitCheckoutSparseEnabled(repo.Root) {
		return fmt.Errorf("sparse_checkout")
	}
	if head := gitOutput(repo.Root, "rev-parse", "--verify", "HEAD^{commit}"); head == "" || head != repo.Head {
		return fmt.Errorf("head_changed")
	}
	if err := validateGitOverlayCheckoutConfig(repo.Root); err != nil {
		return err
	}
	tracked, err := loadGitTrackedPaths(repo.Root)
	if err != nil {
		return fmt.Errorf("tracked_paths")
	}
	for _, entry := range tracked {
		if entry.stage != 0 {
			return fmt.Errorf("unmerged_index")
		}
		if entry.skipWorktree {
			return fmt.Errorf("skip_worktree")
		}
		if entry.mode == "160000" {
			return fmt.Errorf("gitlink")
		}
	}
	targetTree := exec.Command("git", "ls-tree", "-r", "-z", "--name-only", "--full-tree", repo.Head)
	targetTree.Dir = repo.Root
	targetTree.Env = repositoryGitEnvironment()
	targetPaths, err := targetTree.Output()
	if err != nil {
		return fmt.Errorf("git_attribute_inspection")
	}
	for _, inspection := range []struct {
		source string
		paths  []string
	}{
		{source: repo.Head, paths: splitNul(targetPaths)},
		{paths: manifest.Files},
	} {
		attribute, err := gitOverlayTransformAttribute(repo.Root, inspection.source, inspection.paths)
		if err != nil {
			return fmt.Errorf("git_attribute_inspection")
		}
		if attribute != "" {
			return fmt.Errorf("git_attribute_%s", attribute)
		}
	}
	overlayFiles, overlayBytes := overlayPathSetBytes(repo.Root, manifest.Files, manifest.Changed)
	if !slices.Equal(overlayFiles, manifest.OverlayFiles) || overlayBytes != manifest.OverlayBytes {
		return fmt.Errorf("overlay_manifest_changed")
	}
	deleted := make(map[string]struct{}, len(manifest.Deleted))
	for _, rel := range manifest.Deleted {
		deleted[rel] = struct{}{}
	}
	overlay := make(map[string]struct{}, len(manifest.OverlayFiles))
	for _, rel := range manifest.OverlayFiles {
		overlay[rel] = struct{}{}
		if err := validateGitOverlayPath(repo.Root, rel); err != nil {
			return err
		}
	}
	for _, rel := range manifest.Changed {
		if _, uploaded := overlay[rel]; !uploaded {
			if _, removed := deleted[rel]; !removed {
				return fmt.Errorf("changed_path_not_represented")
			}
		}
	}
	return nil
}

func validateGitOverlayCheckoutConfig(root string) error {
	checks := []struct {
		key     string
		boolean bool
		allowed []string
	}{
		{key: "core.autocrlf", allowed: []string{"false", "no", "off", "0"}},
		{key: "core.eol", allowed: []string{"lf"}},
		{key: "core.symlinks", boolean: true, allowed: []string{"true"}},
		{key: "core.filemode", boolean: true, allowed: []string{"true"}},
	}
	for _, check := range checks {
		args := []string{"config"}
		if check.boolean {
			args = append(args, "--type=bool")
		}
		args = append(args, "--get", check.key)
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = repositoryGitEnvironment()
		out, err := cmd.Output()
		if err != nil {
			if exitCode(err) == 1 {
				continue
			}
			return fmt.Errorf("%s_inspection", strings.ReplaceAll(check.key, ".", "_"))
		}
		if !slices.Contains(check.allowed, strings.ToLower(strings.TrimSpace(string(out)))) {
			return fmt.Errorf("%s", strings.ReplaceAll(check.key, ".", "_"))
		}
	}
	return nil
}

func gitOverlayTransformAttribute(root, source string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	var input bytes.Buffer
	for _, rel := range paths {
		if !safeRepoRel(rel) {
			return "", fmt.Errorf("unsafe path")
		}
		input.WriteString(rel)
		input.WriteByte(0)
	}
	args := []string{"check-attr"}
	if source != "" {
		args = append(args, "--source="+source)
	}
	args = append(args, "-z", "--stdin")
	args = append(args, gitOverlayTransformAttributes...)
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	cmd.Stdin = &input
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	fields := bytes.Split(out, []byte{0})
	for index := 2; index < len(fields); index += 3 {
		if value := string(fields[index]); value != "" && value != "unspecified" && value != "unset" {
			return string(fields[index-1]), nil
		}
	}
	return "", nil
}

func validateGitOverlayPath(root, rel string) error {
	if !safeRepoRel(rel) {
		return fmt.Errorf("unsafe_path")
	}
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	current := root
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("missing_overlay_path")
		}
		if index < len(parts)-1 {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink_ancestor")
			}
		} else if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported_file_type")
		}
	}
	return nil
}

func sameSyncManifest(left, right SyncManifest) bool {
	return slices.Equal(left.Files, right.Files) &&
		slices.Equal(left.Deleted, right.Deleted) &&
		slices.Equal(left.Changed, right.Changed) &&
		slices.Equal(left.OverlayFiles, right.OverlayFiles) &&
		slices.Equal(left.ProtectedTrackedExcludes, right.ProtectedTrackedExcludes) &&
		left.Bytes == right.Bytes && left.ChangedBytes == right.ChangedBytes && left.OverlayBytes == right.OverlayBytes
}

func gitOverlayLocalSnapshot(repo Repo, manifest SyncManifest) (string, error) {
	if gitOutput(repo.Root, "rev-parse", "--verify", "HEAD^{commit}") != repo.Head {
		return "", fmt.Errorf("head_changed")
	}
	index := gitOutput(repo.Root, "rev-parse", "--git-path", "index")
	if index == "" {
		return "", fmt.Errorf("missing_index")
	}
	if !filepath.IsAbs(index) {
		index = filepath.Join(repo.Root, index)
	}
	h := sha256.New()
	fmt.Fprintf(h, "head=%s\nmanifest=%x\ndeleted=%x\noverlay=%x\n", repo.Head, sha256.Sum256(manifest.NUL()), sha256.Sum256(manifest.DeletedNUL()), sha256.Sum256(manifest.OverlayNUL()))
	indexFile, err := os.Open(index)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(h, indexFile); err != nil {
		_ = indexFile.Close()
		return "", err
	}
	_ = indexFile.Close()
	for _, rel := range manifest.Changed {
		full := filepath.Join(repo.Root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		fmt.Fprintf(h, "\npath=%s\n", rel)
		if os.IsNotExist(err) {
			fmt.Fprint(h, "missing\n")
			continue
		}
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "mode=%s size=%d\n", info.Mode(), info.Size())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "link=%s\n", target)
			continue
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("unsupported_file_type")
		}
		file, err := os.Open(full)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(h, file); err != nil {
			_ = file.Close()
			return "", err
		}
		_ = file.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func gitOverlayLocalFallbackReason(err error) string {
	reason := strings.ToLower(strings.TrimSpace(err.Error()))
	if reason == "" {
		return "local_invariant"
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range reason {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		case b.Len() != 0 && !lastUnderscore:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func gitOverlayFallbackResult(output string, err error) (string, bool) {
	reason, fallback, _ := gitOverlayFallbackOutcome(output, err)
	return reason, fallback
}

func gitOverlayFallbackOutcome(output string, err error) (string, bool, bool) {
	if err == nil || exitCode(err) != gitOverlayFallbackExitCode {
		return "", false, false
	}
	mutated := false
	reason := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == gitOverlayMutationMarker {
			mutated = true
			continue
		}
		if value, ok := strings.CutPrefix(line, gitOverlayFallbackMarker); ok {
			reason = gitOverlayLocalFallbackReason(fmt.Errorf("%s", value))
		}
	}
	if reason == "" {
		reason = "remote_preparation_invariant"
	}
	return reason, true, mutated
}

func gitOverlayHermeticFunctions() string {
	return `git() {
  /usr/bin/env -i HOME=/dev/null XDG_CONFIG_HOME=/dev/null PATH=/usr/bin:/bin LANG=C LC_ALL=C \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
    GIT_ATTR_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false SSH_ASKPASS=/bin/false \
    GCM_INTERACTIVE=Never GIT_SSH_COMMAND=/bin/false GIT_LFS_SKIP_SMUDGE=1 \
    GIT_TRACE_PACKET="${overlay_packet_trace:-0}" \
    /usr/bin/git -c credential.helper= -c credential.interactive=never \
      -c core.hooksPath=/dev/null -c core.attributesFile=/dev/null -c core.excludesFile=/dev/null \
      -c core.fsmonitor=false -c core.autocrlf=false -c core.eol=lf \
      -c core.symlinks=true -c core.filemode=true -c protocol.allow=never \
      -c protocol.file.allow=always -c protocol.http.allow=always -c protocol.https.allow=always \
      -c protocol.ext.allow=never -c protocol.git.allow=never -c protocol.ssh.allow=never \
      -c fetch.recurseSubmodules=false -c submodule.recurse=false "$@"
}
overlay_workspace_safe() {
  checkout_root="$(cd -P -- "$1" 2>/dev/null && pwd -P)" || return 1
  [ -d "$checkout_root/.git" ] && [ ! -L "$checkout_root/.git" ] || return 1
  [ -f "$checkout_root/.git/config" ] && [ ! -L "$checkout_root/.git/config" ] || return 1
  [ -d "$checkout_root/.git/objects" ] && [ ! -L "$checkout_root/.git/objects" ] || return 1
  [ ! -s "$checkout_root/.git/objects/info/alternates" ] || return 1
  [ ! -s "$checkout_root/.git/info/attributes" ] || return 1
  set +e
  unsafe_keys="$(git config --file "$checkout_root/.git/config" --no-includes --name-only --get-regexp \
    '^(include([.]|$)|includeif[.]|url[.].*[.](insteadof|pushinsteadof)|protocol([.]|$)|http([.]|$)|credential([.]|$)|filter[.]|extensions[.]worktreeconfig|core[.](hookspath|attributesfile|excludesfile|fsmonitor|gitproxy|sshcommand|worktree)|remote[.].*[.](uploadpack|receivepack|proxy|vcs))' 2>/dev/null)"
  config_status=$?
  set -e
  { [ "$config_status" -eq 0 ] || [ "$config_status" -eq 1 ]; } && [ -z "$unsafe_keys" ] || return 1
  checkout_git_dir="$(git -C "$checkout_root" rev-parse --absolute-git-dir 2>/dev/null)" || return 1
  checkout_common_dir="$(git -C "$checkout_root" rev-parse --git-common-dir 2>/dev/null)" || return 1
  case "$checkout_common_dir" in /*) ;; *) checkout_common_dir="$checkout_root/$checkout_common_dir" ;; esac
  checkout_git_dir="$(cd -P -- "$checkout_git_dir" && pwd -P)" || return 1
  checkout_common_dir="$(cd -P -- "$checkout_common_dir" && pwd -P)" || return 1
  expected_git_dir="$(cd -P -- "$checkout_root/.git" && pwd -P)" || return 1
  [ "$checkout_git_dir" = "$expected_git_dir" ] && [ "$checkout_common_dir" = "$expected_git_dir" ] || return 1
  checkout_index="$(git -C "$checkout_root" rev-parse --git-path index 2>/dev/null)" || return 1
  case "$checkout_index" in /*) ;; *) checkout_index="$checkout_root/$checkout_index" ;; esac
  [ "$checkout_index" = "$expected_git_dir/index" ] && [ ! -L "$checkout_index" ]
}
overlay_metadata_safe() {
  checkout_root="$(cd -P -- "$1" 2>/dev/null && pwd -P)" || return 1
  metadata_root="$checkout_root/.git/crabbox"
  if [ -L "$metadata_root" ]; then return 1; fi
  if [ ! -e "$metadata_root" ]; then return 0; fi
  [ -d "$metadata_root" ] || return 1
  resolved_metadata="$(cd -P -- "$metadata_root" && pwd -P)" || return 1
  [ "$resolved_metadata" = "$checkout_root/.git/crabbox" ] || return 1
  for metadata_path in "$metadata_root"/*; do
    if [ ! -e "$metadata_path" ] && [ ! -L "$metadata_path" ]; then continue; fi
    case "${metadata_path##*/}" in
      sync-manifest*|sync-deleted*|sync-fingerprint*|sync-finalize*|sync-git-status*|git-hydrate-base*)
        if [ -L "$metadata_path" ] || [ ! -f "$metadata_path" ]; then return 1; fi
        ;;
    esac
  done
}
overlay_runtime_state_safe() {
  checkout_root="$(cd -P -- "$1" 2>/dev/null && pwd -P)" || return 1
  runtime_root="$checkout_root/.crabbox"
  if [ -L "$runtime_root" ]; then return 1; fi
  if [ ! -e "$runtime_root" ]; then return 0; fi
  [ -d "$runtime_root" ] || return 1
  [ "$(cd -P -- "$runtime_root" && pwd -P)" = "$runtime_root" ] || return 1
  for runtime_name in env scripts logs captures runs; do
    runtime_path="$runtime_root/$runtime_name"
    if [ -L "$runtime_path" ] || { [ -e "$runtime_path" ] && [ ! -d "$runtime_path" ]; }; then return 1; fi
  done
}
`
}

func remoteGitOverlayShellCommand(script string) string {
	return "/usr/bin/env -i PATH=/usr/bin:/bin LANG=C LC_ALL=C /bin/bash --noprofile --norc -c " + shellQuote(script)
}

func remotePrepareGitOverlay(workdir string, plan gitCoherencePlan) string {
	return remotePrepareGitOverlayWithBase(workdir, plan, "", "")
}

func remotePrepareGitOverlayWithBase(workdir string, plan gitCoherencePlan, baseRef, baseSHA string) string {
	if !plan.enabled() || !gitOverlayOriginTransportSupported(plan.RemoteURL) {
		return "printf '" + gitOverlayFallbackMarker + "unsupported_origin_transport\\n' >&2; exit 78"
	}
	parent := filepath.ToSlash(filepath.Dir(workdir))
	script := `set -e -o pipefail
workdir=` + shellQuote(workdir) + `
parent=` + shellQuote(parent) + `
expected_origin=` + shellQuote(plan.RemoteURL) + `
expected_target=` + shellQuote(plan.Target) + `
expected_tree=` + shellQuote(plan.Tree) + `
advertised_branch=` + shellQuote(plan.Branch) + `
base_ref=` + shellQuote(baseRef) + `
expected_base=` + shellQuote(baseSHA) + `
overlay_mutated=
overlay_fallback() {
  if [ -n "$overlay_mutated" ]; then printf '%s\n' ` + shellQuote(gitOverlayMutationMarker) + ` >&2; fi
  printf '%s%s\n' ` + shellQuote(gitOverlayFallbackMarker) + ` "$1" >&2
  exit 78
}
case "$workdir" in "$parent"/*) ;; *) overlay_fallback unsafe_remote_root ;; esac
if [ -L "$workdir" ]; then overlay_fallback symlink_remote_root; fi
for prerequisite in /bin/bash /bin/mkdir /bin/rm /bin/mv /bin/cp /usr/bin/env /usr/bin/git /usr/bin/find /usr/bin/mktemp /usr/bin/awk /usr/bin/grep; do
  [ -x "$prerequisite" ] || overlay_fallback remote_prerequisite_missing
done
/bin/mkdir -p "$parent"
git_runtime_root="$(/usr/bin/mktemp -d "$parent/.overlay-git.XXXXXX")" || overlay_fallback remote_prerequisite_missing
seed_tmp=
baseline_tmp=
cleanup_git_overlay() {
  cleanup_code=$?
  if [ -n "$baseline_tmp" ]; then /bin/rm -f -- "$baseline_tmp"; fi
  if [ -n "$seed_tmp" ]; then /bin/rm -rf -- "$seed_tmp"; fi
  /bin/rm -rf -- "$git_runtime_root"
  trap - EXIT
  if [ "$cleanup_code" -ne 0 ]; then exit "$cleanup_code"; fi
}
trap cleanup_git_overlay EXIT
` + gitOverlayHermeticFunctions() + `
if [ -n "$base_ref" ] && [ -z "$expected_base" ]; then overlay_fallback base_ref_unavailable; fi
overlay_packet_trace="$git_runtime_root/packets"
set +e
git -C "$git_runtime_root" -c protocol.version=2 ls-remote --exit-code --heads "$expected_origin" \
  "refs/heads/$advertised_branch" "refs/heads/$base_ref" >"$git_runtime_root/advertised" 2>"$git_runtime_root/transport-error"
transport_status=$?
overlay_packet_trace=
set -e
if [ "$transport_status" -ne 0 ]; then
  if [ "$transport_status" -eq 2 ]; then overlay_fallback advertised_branch_missing; fi
  if /usr/bin/grep -Eiq 'authentication (failed|required)|could not read Username|unable to get password|terminal prompts disabled|access denied|permission denied|HTTP.*(401|403)|requested URL returned error: (401|403)|repository not found' "$git_runtime_root/transport-error"; then
    overlay_fallback origin_auth_required
  fi
  case "$expected_origin" in http://*|https://*) /bin/cat "$git_runtime_root/transport-error" >&2; exit "$transport_status" ;; *) overlay_fallback origin_unavailable ;; esac
fi
if ! /usr/bin/grep -Eq 'packet:.*< fetch=.*filter' "$git_runtime_root/packets"; then
  overlay_fallback filtered_history_unsupported
fi
advertised_target="$(/usr/bin/awk -v branch="refs/heads/$advertised_branch" '$2 == branch { print $1; exit }' "$git_runtime_root/advertised")"
[ -n "$advertised_target" ] || overlay_fallback advertised_branch_missing
if [ -n "$base_ref" ]; then
  advertised_base="$(/usr/bin/awk -v branch="refs/heads/$base_ref" '$2 == branch { print $1; exit }' "$git_runtime_root/advertised")"
  [ -n "$advertised_base" ] || overlay_fallback base_ref_missing
  [ "$advertised_base" = "$expected_base" ] || overlay_fallback base_ref_mismatch
fi
if [ -e "$workdir/.git" ] || [ -L "$workdir/.git" ]; then
  overlay_workspace_safe "$workdir" || overlay_fallback unsafe_git_workspace
  overlay_metadata_safe "$workdir" || overlay_fallback unsafe_overlay_metadata
  overlay_runtime_state_safe "$workdir" || overlay_fallback unsafe_runtime_state
  [ "$(git -C "$workdir" remote get-url origin 2>/dev/null || true)" = "$expected_origin" ] || overlay_fallback origin_mismatch
elif [ -d "$workdir" ]; then
  overlay_fallback non_git_workspace
fi
if [ ! -d "$workdir/.git" ]; then
  seed_tmp="$(/usr/bin/mktemp -d "$parent/.overlay-seed.XXXXXX")"
  git init --quiet "$seed_tmp" || overlay_fallback checkout_init_failed
  git -C "$seed_tmp" remote add origin "$expected_origin" || overlay_fallback checkout_init_failed
  checkout_root="$seed_tmp"
else
  checkout_root="$workdir"
fi
git -C "$checkout_root" config --local remote.origin.promisor true || overlay_fallback filtered_history_unsupported
git -C "$checkout_root" config --local remote.origin.partialclonefilter blob:none || overlay_fallback filtered_history_unsupported
fetch_args=(--quiet --no-tags --no-write-fetch-head --filter=blob:none)
if [ "$(git -C "$checkout_root" rev-parse --is-shallow-repository 2>/dev/null || true)" = true ]; then
  fetch_args+=(--unshallow)
fi
refspecs=("+refs/heads/$advertised_branch:refs/remotes/origin/$advertised_branch")
if [ -n "$base_ref" ] && [ "$base_ref" != "$advertised_branch" ]; then
  refspecs+=("+refs/heads/$base_ref:refs/remotes/origin/$base_ref")
fi
set +e
git -C "$checkout_root" fetch "${fetch_args[@]}" origin "${refspecs[@]}" 2>"$git_runtime_root/transport-error"
transport_status=$?
set -e
if [ "$transport_status" -ne 0 ]; then
  if /usr/bin/grep -Eiq 'authentication (failed|required)|could not read Username|unable to get password|terminal prompts disabled|access denied|permission denied|HTTP.*(401|403)|requested URL returned error: (401|403)|repository not found' "$git_runtime_root/transport-error"; then
    overlay_fallback origin_auth_required
  fi
  /bin/cat "$git_runtime_root/transport-error" >&2
  exit "$transport_status"
fi
if /usr/bin/grep -Eiq 'filtering not recognized|filtering not supported|server does not support filter' "$git_runtime_root/transport-error"; then
  overlay_fallback filtered_history_unsupported
fi
fetched_branch="$(git -C "$checkout_root" rev-parse --verify "refs/remotes/origin/$advertised_branch^{commit}" 2>/dev/null || true)"
[ "$fetched_branch" = "$advertised_target" ] || overlay_fallback advertised_branch_changed
git -C "$checkout_root" cat-file -e "$expected_target^{commit}" 2>/dev/null || overlay_fallback target_missing
git -C "$checkout_root" merge-base --is-ancestor "$expected_target" "$fetched_branch" >/dev/null 2>&1 || overlay_fallback target_not_advertised
[ "$(git -C "$checkout_root" rev-parse --verify "$expected_target^{tree}" 2>/dev/null || true)" = "$expected_tree" ] || overlay_fallback tree_mismatch
if [ -n "$base_ref" ] && [ "$(git -C "$checkout_root" rev-parse --verify "refs/remotes/origin/$base_ref^{commit}" 2>/dev/null || true)" != "$expected_base" ]; then
  overlay_fallback base_ref_mismatch
fi
overlay_metadata_safe "$checkout_root" || overlay_fallback unsafe_overlay_metadata
if [ -f "$checkout_root/.git/crabbox/sync-manifest" ]; then
  /bin/cp -- "$checkout_root/.git/crabbox/sync-manifest" "$git_runtime_root/previous-manifest"
fi
/bin/mkdir -p "$checkout_root/.git/crabbox"
overlay_metadata_safe "$checkout_root" || overlay_fallback unsafe_overlay_metadata
baseline_tmp="$checkout_root/.git/crabbox/sync-manifest.overlay.$$"
if [ -e "$baseline_tmp" ] || [ -L "$baseline_tmp" ]; then overlay_fallback unsafe_overlay_metadata; fi
if [ -f "$git_runtime_root/previous-manifest" ]; then
  /bin/cp -- "$git_runtime_root/previous-manifest" "$baseline_tmp" || overlay_fallback baseline_failed
else
  (set -C; : > "$baseline_tmp") || overlay_fallback baseline_failed
fi
git -C "$checkout_root" ls-tree -r -z --name-only --full-tree "$expected_target" >> "$baseline_tmp" || overlay_fallback baseline_failed
/bin/mv -- "$baseline_tmp" "$checkout_root/.git/crabbox/sync-manifest" || overlay_fallback baseline_failed
baseline_tmp=
if [ -n "$seed_tmp" ]; then
  /bin/mv -- "$seed_tmp" "$workdir"
  seed_tmp=
fi
overlay_mutated=1
cd "$workdir"
workdir="$(pwd -P)"
overlay_workspace_safe "$workdir" || overlay_fallback unsafe_git_workspace
overlay_metadata_safe "$workdir" || overlay_fallback unsafe_overlay_metadata
overlay_runtime_state_safe "$workdir" || overlay_fallback unsafe_runtime_state
if [ "$(git config --bool core.sparseCheckout 2>/dev/null || true)" = true ] ||
   [ "$(git config --bool core.sparseCheckoutCone 2>/dev/null || true)" = true ]; then
  overlay_fallback sparse_checkout
fi
git checkout --quiet --force --detach "$expected_target" || overlay_fallback checkout_failed
git reset --hard --quiet "$expected_target" || overlay_fallback reset_failed
clean_args=(-ffdx --quiet -e /.crabbox/)
/usr/bin/find -P . \( -ipath './.git' -o -ipath './.crabbox' \) -prune -o \
  \( -type d -o -type l \) \( -iname node_modules -o -iname .pnpm-store -o -ipath '*/.yarn/cache' -o -ipath '*/.yarn/unplugged' \) \
  -print0 -prune >"$git_runtime_root/cache-paths" || overlay_fallback cache_discovery_failed
while IFS= read -r -d '' cache_path; do
  cache_path="${cache_path#./}"
  if [ -L "$cache_path" ]; then overlay_fallback unsafe_cache_root; fi
  set +e
  printf '%s\0' "$cache_path" | git check-ignore --no-index -v -z --stdin >"$git_runtime_root/cache-ignore" 2>/dev/null
  ignore_status=$?
  set -e
  if [ "$ignore_status" -eq 1 ]; then continue; fi
  [ "$ignore_status" -eq 0 ] || overlay_fallback cache_ignore_failed
  {
    IFS= read -r -d '' ignore_source && IFS= read -r -d '' ignore_line &&
      IFS= read -r -d '' ignore_pattern && IFS= read -r -d '' ignore_path
  } <"$git_runtime_root/cache-ignore" || overlay_fallback cache_ignore_failed
  case "$ignore_source" in .gitignore|*/.gitignore) ;; *) continue ;; esac
  case "$ignore_source" in /*|../*|*/../*) continue ;; esac
  [ -f "$ignore_source" ] && [ ! -L "$ignore_source" ] || overlay_fallback cache_ignore_untrusted
  expected_ignore="$(git rev-parse --verify "$expected_target:$ignore_source" 2>/dev/null || true)"
  actual_ignore="$(git hash-object --no-filters -- "$ignore_source" 2>/dev/null || true)"
  [ -n "$expected_ignore" ] && [ "$actual_ignore" = "$expected_ignore" ] || continue
  if [ -L "$cache_path" ] || [ ! -d "$cache_path" ]; then overlay_fallback unsafe_cache_root; fi
  resolved_cache="$(cd -P -- "$cache_path" && pwd -P)" || overlay_fallback unsafe_cache_root
  case "$resolved_cache/" in "$workdir"/*) ;; *) overlay_fallback unsafe_cache_root ;; esac
  cache_pattern="${cache_path//\\/\\\\}"
  cache_pattern="${cache_pattern//\*/\\*}"
  cache_pattern="${cache_pattern//\?/\\?}"
  cache_pattern="${cache_pattern//\[/\\[}"
  cache_pattern="${cache_pattern//\]/\\]}"
  cache_pattern="${cache_pattern//!/\\!}"
  cache_pattern="${cache_pattern//#/\\#}"
  clean_args+=(-e "$cache_pattern/")
done <"$git_runtime_root/cache-paths"
git clean "${clean_args[@]}" || overlay_fallback clean_failed
[ "$(git rev-parse --verify HEAD^{commit})" = "$expected_target" ] || overlay_fallback head_mismatch
[ "$(git write-tree)" = "$expected_tree" ] || overlay_fallback index_tree_mismatch
overlay_metadata_safe "$workdir" || overlay_fallback unsafe_overlay_metadata
/bin/rm -f -- .git/crabbox/sync-fingerprint .git/crabbox/git-hydrate-base
`
	return remoteGitOverlayShellCommand(script)
}

func remotePruneGitOverlaySyncManifest(workdir, finalizeToken string, allowMassDeletions ...bool) string {
	return remotePruneSafeSyncManifest(workdir, finalizeToken, remotePlainSyncMetaDirScript(), allowMassDeletions...)
}

func remotePruneSafeSyncManifest(workdir, finalizeToken, metadataScript string, allowMassDeletions ...bool) string {
	manifestName := remoteSyncPendingManifestName(finalizeToken)
	deletedName := remoteSyncPendingDeletedName(finalizeToken)
	allowValue := "0"
	if len(allowMassDeletions) != 0 && allowMassDeletions[0] {
		allowValue = "1"
	}
	python := `import os, stat, sys
def read(path):
    if not os.path.isfile(path):
        return []
    with open(path, "rb") as stream:
        data = stream.read()
    if data and not data.endswith(b"\0"):
        raise SystemExit("malformed overlay deletion manifest")
    entries = data.split(b"\0")[:-1]
    for entry in entries:
        parts = entry.split(b"/")
        if not entry or entry.startswith(b"/") or any(part in (b"", b".", b"..", b".git", b".crabbox") for part in parts):
            raise SystemExit("unsafe overlay deletion path")
    return entries
def shape(path):
    current = b""
    parts = path.split(b"/")
    for index, part in enumerate(parts):
        current = part if not current else current + b"/" + part
        try:
            mode = os.lstat(current).st_mode
        except (FileNotFoundError, NotADirectoryError):
            return None
        if index + 1 < len(parts) and not stat.S_ISDIR(mode):
            return None
    return mode
def leaf(path):
    mode = shape(path)
    return mode is not None and not stat.S_ISDIR(mode)
old, new_entries, deleted = (read(path) for path in sys.argv[1:4])
new = set(new_entries)
pending = []
seen = set()
for path in deleted + old:
    if path in seen or path in new:
        continue
    if any(current.startswith(path + b"/") for current in new) and stat.S_ISDIR(shape(path) or 0):
        continue
    if any(path.startswith(current + b"/") and leaf(current) for current in new):
        continue
    seen.add(path)
    pending.append(path)
if sys.argv[4] != "1" and len(pending) >= 200:
    raise SystemExit("remote sync sanity failed: %d pending deletions" % len(pending))
sys.stdout.buffer.write(b"".join(path + b"\0" for path in pending))`
	perl := `use strict; use warnings;
sub read_manifest {
  return () unless -f $_[0]; open my $fh, "<", $_[0] or die "open: $!"; binmode $fh; local $/; my $data = <$fh>; $data //= "";
  die "malformed overlay deletion manifest\n" if length($data) && substr($data, -1) ne "\0";
  my @entries = split /\0/, $data, -1; pop @entries if @entries;
  for my $entry (@entries) {
    my @parts = split m{/}, $entry, -1;
    die "unsafe overlay deletion path\n" if !length($entry) || $entry =~ m{^/} || grep { $_ eq "" || $_ eq "." || $_ eq ".." || $_ eq ".git" || $_ eq ".crabbox" } @parts;
  }
  return @entries;
}
sub shape {
  my @parts = split m{/}, $_[0]; my $current = "";
  for my $index (0 .. $#parts) {
    $current = length($current) ? "$current/$parts[$index]" : $parts[$index];
    my @state = lstat($current); return undef unless @state;
    return undef if $index < $#parts && ($state[2] & 0170000) != 0040000;
    return $state[2] if $index == $#parts;
  }
  return undef;
}
sub leaf {
  my $mode = shape($_[0]); return defined($mode) && ($mode & 0170000) != 0040000;
}
my @old = read_manifest($ARGV[0]); my @entries = read_manifest($ARGV[1]); my @deleted = read_manifest($ARGV[2]);
my %new = map { $_ => 1 } @entries; my %seen; my @pending;
for my $path (@deleted, @old) {
  next if $seen{$path}++ || $new{$path};
  my $path_shape = shape($path);
  next if defined($path_shape) && ($path_shape & 0170000) == 0040000 && grep { index($_, $path . "/") == 0 } @entries;
  next if grep { index($path, $_ . "/") == 0 && leaf($_) } @entries;
  push @pending, $path;
}
die "remote sync sanity failed: " . scalar(@pending) . " pending deletions\n" if $ARGV[3] ne "1" && @pending >= 200;
binmode STDOUT; print STDOUT map { $_ . "\0" } @pending;`
	script := `set -e -o pipefail
cd ` + shellQuote(workdir) + `
` + gitOverlayHermeticFunctions() + metadataScript + `
old="$meta_dir/sync-manifest"
new="$meta_dir/` + manifestName + `"
deleted="$meta_dir/` + deletedName + `"
delete_paths() {
  while IFS= read -r -d '' rel; do
    case "$rel" in ''|/*|../*|*/../*|.git|.git/*|*/.git|*/.git/*|.crabbox|.crabbox/*|*/.crabbox|*/.crabbox/*) echo "unsafe overlay deletion path" >&2; exit 67 ;; esac
    remainder="$rel"
    ancestor=
    while [ "$remainder" != "${remainder#*/}" ]; do
      component="${remainder%%/*}"
      remainder="${remainder#*/}"
      if [ -z "$ancestor" ]; then ancestor="$component"; else ancestor="$ancestor/$component"; fi
      if [ -L "$ancestor" ]; then echo "overlay deletion refuses symlink ancestor: $ancestor" >&2; exit 67; fi
    done
    /bin/rm -f -- "$rel"
    dir=$(/usr/bin/dirname -- "$rel")
    while [ "$dir" != . ] && [ "$dir" != / ]; do
      if [ -L "$dir" ]; then echo "overlay deletion refuses symlink ancestor: $dir" >&2; exit 67; fi
      /bin/rmdir -- "$dir" 2>/dev/null || break
      dir=$(/usr/bin/dirname -- "$dir")
    done
  done
}
` + remoteSyncInterpreterCommand(python, perl, "\"$old\" \"$new\" \"$deleted\" "+shellQuote(allowValue)) + ` | delete_paths
`
	return remoteGitOverlayShellCommand(script)
}
