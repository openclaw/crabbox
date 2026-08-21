package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	gitOverlayFallbackExitCode = 78
	gitOverlayFallbackMarker   = "CRABBOX_GIT_OVERLAY_FALLBACK:"
)

var gitOverlayTransformAttributes = []string{
	"text",
	"crlf",
	"eol",
	"working-tree-encoding",
	"filter",
	"smudge",
	"clean",
	"ident",
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
	case isWindowsNativeTarget(target) || target.TargetOS == targetWindows || target.TargetOS == targetMacOS:
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
	default:
		if err := validateGitOverlayManifest(repo, manifest); err != nil {
			decision.Reason = gitOverlayLocalFallbackReason(err)
		} else if !coherence.enabled() {
			decision.Reason = "unseedable_head"
		} else {
			decision.Enabled = true
		}
	}
	return decision
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
		if entry.mode == "160000" {
			return fmt.Errorf("gitlink")
		}
	}
	targetAttribute, err := gitOverlayTargetTransformAttribute(repo.Root, repo.Head)
	if err != nil {
		return fmt.Errorf("git_attribute_inspection")
	}
	if targetAttribute != "" {
		return fmt.Errorf("git_attribute_%s", gitOverlayLocalFallbackReason(fmt.Errorf("%s", targetAttribute)))
	}
	worktreeAttribute, err := gitOverlayWorktreeTransformAttribute(repo.Root, manifest.Files)
	if err != nil {
		return fmt.Errorf("git_attribute_inspection")
	}
	if worktreeAttribute != "" {
		return fmt.Errorf("git_attribute_%s", gitOverlayLocalFallbackReason(fmt.Errorf("%s", worktreeAttribute)))
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
		if _, uploaded := overlay[rel]; uploaded {
			continue
		}
		if _, removed := deleted[rel]; !removed {
			return fmt.Errorf("changed_path_not_represented")
		}
	}
	return nil
}

func validateGitOverlayCheckoutConfig(root string) error {
	autoCRLF, set, err := gitOverlayConfigValue(root, "core.autocrlf", false)
	if err != nil {
		return fmt.Errorf("core_autocrlf_inspection")
	}
	if set && autoCRLF != "false" && autoCRLF != "no" && autoCRLF != "off" && autoCRLF != "0" {
		return fmt.Errorf("core_autocrlf")
	}
	eol, set, err := gitOverlayConfigValue(root, "core.eol", false)
	if err != nil {
		return fmt.Errorf("core_eol_inspection")
	}
	if set && eol != "lf" {
		return fmt.Errorf("core_eol")
	}
	symlinks, set, err := gitOverlayConfigValue(root, "core.symlinks", true)
	if err != nil {
		return fmt.Errorf("core_symlinks_inspection")
	}
	if set && symlinks != "true" {
		return fmt.Errorf("core_symlinks")
	}
	filemode, set, err := gitOverlayConfigValue(root, "core.filemode", true)
	if err != nil {
		return fmt.Errorf("core_filemode_inspection")
	}
	if set && filemode != "true" {
		return fmt.Errorf("core_filemode")
	}
	return nil
}

func gitOverlayConfigValue(root, key string, boolean bool) (string, bool, error) {
	args := []string{"config", "--get", key}
	if boolean {
		args = []string{"config", "--type=bool", "--get", key}
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		if exitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.ToLower(strings.TrimSpace(string(out))), true, nil
}

func gitOverlayTargetTransformAttribute(root, target string) (string, error) {
	cmd := exec.Command("git", "ls-tree", "-r", "-z", "--name-only", "--full-tree", target)
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	paths, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return gitOverlayTransformAttribute(root, target, paths)
}

func gitOverlayWorktreeTransformAttribute(root string, paths []string) (string, error) {
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
	return gitOverlayTransformAttribute(root, "", input.Bytes())
}

func gitOverlayTransformAttribute(root, source string, paths []byte) (string, error) {
	if len(paths) == 0 {
		return "", nil
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
	cmd.Stdin = bytes.NewReader(paths)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	fields := bytes.Split(out, []byte{0})
	for index := 2; index < len(fields); index += 3 {
		value := string(fields[index])
		if value != "" && value != "unspecified" && value != "unset" {
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
			continue
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported_file_type")
		}
	}
	return nil
}

func gitOverlayLocalFallbackReason(err error) string {
	reason := strings.TrimSpace(err.Error())
	if reason == "" {
		return "local_invariant"
	}
	reason = strings.ToLower(reason)
	var b strings.Builder
	lastUnderscore := false
	for _, r := range reason {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		case b.Len() > 0 && !lastUnderscore:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func sameSyncManifest(left, right SyncManifest) bool {
	return slices.Equal(left.Files, right.Files) &&
		slices.Equal(left.Deleted, right.Deleted) &&
		slices.Equal(left.Changed, right.Changed) &&
		slices.Equal(left.OverlayFiles, right.OverlayFiles) &&
		slices.Equal(left.ProtectedTrackedExcludes, right.ProtectedTrackedExcludes) &&
		left.Bytes == right.Bytes &&
		left.ChangedBytes == right.ChangedBytes &&
		left.OverlayBytes == right.OverlayBytes
}

func gitOverlayFallbackResult(output string, err error) (string, bool) {
	if err == nil || exitCode(err) != gitOverlayFallbackExitCode {
		return "", false
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if reason, ok := strings.CutPrefix(line, gitOverlayFallbackMarker); ok {
			reason = gitOverlayLocalFallbackReason(fmt.Errorf("%s", reason))
			if reason != "" {
				return reason, true
			}
		}
	}
	return "remote_reset_invariant", true
}

func remotePrepareGitOverlay(workdir string, plan gitCoherencePlan) string {
	if !plan.enabled() {
		return "true"
	}
	parent := filepath.ToSlash(filepath.Dir(workdir))
	script := `set -e -o pipefail
workdir=` + shellQuote(workdir) + `
parent=` + shellQuote(parent) + `
expected_origin=` + shellQuote(plan.RemoteURL) + `
expected_target=` + shellQuote(plan.Target) + `
expected_tree=` + shellQuote(plan.Tree) + `
advertised_branch=` + shellQuote(plan.Branch) + `
git() {
  command git -c core.hooksPath=/dev/null "$@"
}
overlay_fallback() {
  printf '%s%s\n' ` + shellQuote(gitOverlayFallbackMarker) + ` "$1" >&2
  exit ` + fmt.Sprintf("%d", gitOverlayFallbackExitCode) + `
}
classify_missing_advertised_branch() {
  failed_status="$1"
  set +e
  git ls-remote --exit-code --heads "$expected_origin" "refs/heads/$advertised_branch" >/dev/null
  branch_status=$?
  set -e
  case "$branch_status" in
    2) overlay_fallback advertised_branch_missing ;;
    *) exit "$failed_status" ;;
  esac
}
materialize_target_objects() {
  git -C "$1" rev-list --objects --no-object-names "$expected_target" |
    git -C "$1" cat-file --batch >/dev/null
}
prepare_hermetic_checkout() {
  checkout_root="$1"
  checkout_git_dir="$(git -C "$checkout_root" rev-parse --absolute-git-dir)" || return 1
  rm -f -- "$checkout_git_dir/info/attributes" || return 1
  filter_keys="$(git -C "$checkout_root" config --local --name-only --get-regexp '^filter\.' 2>/dev/null || true)"
  if [ -n "$filter_keys" ]; then
    while IFS= read -r filter_key; do
      [ -z "$filter_key" ] || git -C "$checkout_root" config --local --unset-all "$filter_key" || return 1
    done <<<"$filter_keys"
  fi
  hermetic_root="$checkout_git_dir/crabbox-overlay-hermetic"
  hermetic_home="$hermetic_root/home"
  hermetic_hooks="$hermetic_root/hooks"
  rm -rf -- "$hermetic_root" || return 1
  mkdir -p "$hermetic_home" "$hermetic_hooks" || return 1
  git -C "$checkout_root" config --local core.autocrlf false &&
  git -C "$checkout_root" config --local core.eol lf &&
  git -C "$checkout_root" config --local core.symlinks true &&
  git -C "$checkout_root" config --local core.filemode true &&
  git -C "$checkout_root" config --local core.fsmonitor false &&
  git -C "$checkout_root" config --local core.attributesFile /dev/null &&
  git -C "$checkout_root" config --local core.hooksPath "$hermetic_hooks"
}
hermetic_git() {
  /usr/bin/env -i \
    HOME="$hermetic_home" \
    XDG_CONFIG_HOME="$hermetic_home" \
    PATH="$PATH" \
    LANG=C \
    GIT_CONFIG_NOSYSTEM=1 \
    GIT_CONFIG_GLOBAL=/dev/null \
    GIT_CONFIG_SYSTEM=/dev/null \
    GIT_ATTR_NOSYSTEM=1 \
    GIT_TERMINAL_PROMPT=0 \
    git \
      -c core.autocrlf=false \
      -c core.eol=lf \
      -c core.symlinks=true \
      -c core.filemode=true \
      -c core.fsmonitor=false \
      -c core.attributesFile=/dev/null \
      -c core.hooksPath="$hermetic_hooks" \
      "$@"
}
cleanup_hermetic_checkout() {
  rm -rf -- "${hermetic_root:-}"
}
` + remoteGitWorkspaceFunctions() + `
case "$workdir" in "$parent"/*) ;; *) overlay_fallback unsafe_remote_root ;; esac
if [ -L "$workdir" ]; then overlay_fallback symlink_remote_root; fi
mkdir -p "$parent"
if [ -d "$workdir" ]; then
  cd "$workdir"
  if ! usable_git_workspace || ! origin_matches; then
    cd /
    tmp="$(mktemp -d "$parent/.overlay-seed.XXXXXX")"
    cleanup_overlay_seed() { rm -rf -- "$tmp"; }
    trap cleanup_overlay_seed EXIT
    set +e
    git clone --quiet --filter=blob:none --no-checkout --single-branch --branch "$advertised_branch" "$expected_origin" "$tmp"
    clone_status=$?
    set -e
    [ "$clone_status" -eq 0 ] || classify_missing_advertised_branch "$clone_status"
    git -C "$tmp" cat-file -e "$expected_target^{commit}" 2>/dev/null || overlay_fallback target_missing
    [ "$(git -C "$tmp" rev-parse --verify "$expected_target^{tree}")" = "$expected_tree" ] || overlay_fallback tree_mismatch
    rm -rf -- "$workdir"
    mv -- "$tmp" "$workdir"
    trap - EXIT
  fi
else
  tmp="$(mktemp -d "$parent/.overlay-seed.XXXXXX")"
  cleanup_overlay_seed() { rm -rf -- "$tmp"; }
  trap cleanup_overlay_seed EXIT
  set +e
  git clone --quiet --filter=blob:none --no-checkout --single-branch --branch "$advertised_branch" "$expected_origin" "$tmp"
  clone_status=$?
  set -e
  [ "$clone_status" -eq 0 ] || classify_missing_advertised_branch "$clone_status"
  git -C "$tmp" cat-file -e "$expected_target^{commit}" 2>/dev/null || overlay_fallback target_missing
  [ "$(git -C "$tmp" rev-parse --verify "$expected_target^{tree}")" = "$expected_tree" ] || overlay_fallback tree_mismatch
  mv -- "$tmp" "$workdir"
  trap - EXIT
fi
cd "$workdir"
workdir="$(pwd -P)"
exact_git_root || overlay_fallback invalid_git_root
repair_origin || overlay_fallback origin_repair_failed
if [ "$(git config --bool core.sparseCheckout 2>/dev/null || true)" = true ] ||
   [ "$(git config --bool core.sparseCheckoutCone 2>/dev/null || true)" = true ]; then
  overlay_fallback sparse_checkout
fi
tmp_ref="refs/crabbox/overlay-$expected_target"
set +e
git fetch --quiet --no-tags "$expected_origin" "+refs/heads/$advertised_branch:$tmp_ref"
fetch_status=$?
set -e
[ "$fetch_status" -eq 0 ] || classify_missing_advertised_branch "$fetch_status"
fetched_head="$(git rev-parse --verify "$tmp_ref^{commit}")"
git cat-file -e "$expected_target^{commit}" 2>/dev/null || overlay_fallback target_missing
git merge-base --is-ancestor "$expected_target" "$fetched_head" >/dev/null 2>&1 || overlay_fallback target_not_advertised
[ "$(git rev-parse --verify "$expected_target^{tree}")" = "$expected_tree" ] || overlay_fallback tree_mismatch
materialize_target_objects "$workdir"
prepare_hermetic_checkout "$workdir" || overlay_fallback checkout_config_failed
trap cleanup_hermetic_checkout EXIT
hermetic_git -C "$workdir" checkout --quiet --detach "$expected_target" || overlay_fallback checkout_failed
hermetic_git -C "$workdir" reset --hard --quiet "$expected_target" || overlay_fallback reset_failed
` + remoteIgnoredWarmCacheCleanScript("hermetic_git", "git overlay") + `
crabbox_clean_ignored_warm_caches || overlay_fallback clean_failed
[ "$(hermetic_git -C "$workdir" rev-parse --verify HEAD^{commit})" = "$expected_target" ] || overlay_fallback head_mismatch
[ "$(hermetic_git -C "$workdir" write-tree)" = "$expected_tree" ] || overlay_fallback index_tree_mismatch
status="$(hermetic_git -C "$workdir" status --porcelain --untracked-files=normal)" || overlay_fallback status_failed
[ -z "$status" ] || overlay_fallback dirty_after_reset
hermetic_git -C "$workdir" update-ref -d "$tmp_ref" "$fetched_head" >/dev/null 2>&1 || true
cleanup_hermetic_checkout
trap - EXIT
`
	return "bash -lc " + shellQuote(script)
}
