package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const seededSyncDeltaMarker = "CRABBOX_SEEDED_DELTA_V1\x00"

var seededSyncTransformAttributes = []string{"crlf", "working-tree-encoding", "filter", "smudge", "clean", "ident"}

// A seed is only a baseline, not proof that a reused worktree is still clean.
// Repair remote tracked edits as well as local edits, including locally reverted
// changes from a previous run. The full manifest still owns pruning/finalization.
func seededSyncTransfer(ctx context.Context, target SSHTarget, repo Repo, manifest SyncManifest, plan gitCoherencePlan, workdir string) ([]byte, int, int64, bool) {
	if !plan.seedEnabled() || plan.Tree == "" || isWindowsWSL2Target(target) || isWindowsNativeTarget(target) {
		return nil, 0, 0, false
	}
	var err error
	manifest, err = seededSyncManifest(repo, manifest)
	if err != nil {
		return nil, 0, 0, false
	}
	var output bytes.Buffer
	err = executeSSH(ctx, &target, remoteSeededSyncChanges(workdir, plan), nil, 0, 0, "10", "3", &output, nil)
	if err != nil {
		return nil, 0, 0, false
	}
	return seededSyncTransferFromChanges(repo.Root, manifest, output.Bytes())
}

// With other transforms excluded, CRLF normalization only removes bytes. A
// size mismatch against the Git blob therefore finds normalized-clean working
// bytes without reading every file through Git's --eol content scanner.
func seededSyncManifest(repo Repo, manifest SyncManifest) (SyncManifest, error) {
	for _, setting := range []struct {
		key     string
		allowed []string
	}{
		{"core.trustctime", []string{"true", "yes", "on", "1"}},
		{"core.checkstat", []string{"default"}},
		{"core.ignorestat", []string{"false", "no", "off", "0"}},
	} {
		args := []string{"config"}
		if setting.key != "core.checkstat" {
			args = append(args, "--type=bool")
		}
		args = append(args, "--get", setting.key)
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo.Root, repositoryGitEnvironment()
		output, err := cmd.Output()
		if err != nil && exitCode(err) != 1 {
			return manifest, err
		}
		value := strings.ToLower(strings.TrimSpace(string(output)))
		if err == nil && !slices.Contains(setting.allowed, value) {
			return manifest, fmt.Errorf("unsupported Git stat setting: %s", setting.key)
		}
	}
	checkout, err := captureGitOverlayCheckoutState(repo.Root)
	if err != nil {
		return manifest, err
	}
	if err := validateGitSyncManifestAtState(repo, manifest, checkout, seededSyncTransformAttributes); err != nil {
		return manifest, err
	}
	cmd := exec.Command("git", "ls-tree", "-r", "-l", "-z", "--full-tree", repo.Head)
	cmd.Dir, cmd.Env = repo.Root, repositoryGitEnvironment()
	data, err := cmd.Output()
	if err != nil {
		return manifest, err
	}
	manifest.seededExtraFiles = slices.Clone(manifest.seededExtraFiles)
	included := make(map[string]bool, len(manifest.Files))
	for _, path := range manifest.Files {
		included[path] = true
	}
	for _, entry := range splitNul(data) {
		metadata, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || !safeRepoRel(path) || len(fields) != 4 {
			return manifest, fmt.Errorf("invalid Git tree record")
		}
		if !included[path] || fields[1] != "blob" {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return manifest, fmt.Errorf("invalid Git blob size")
		}
		info, err := os.Lstat(filepath.Join(repo.Root, filepath.FromSlash(path)))
		if err != nil {
			return manifest, err
		}
		if info.Mode().IsRegular() && info.Size() != size {
			manifest.seededExtraFiles = append(manifest.seededExtraFiles, path)
		}
	}
	return manifest, nil
}

func seededSyncTransferFromChanges(root string, manifest SyncManifest, output []byte) ([]byte, int, int64, bool) {
	if !bytes.HasPrefix(output, []byte(seededSyncDeltaMarker)) {
		return nil, 0, 0, false
	}
	output = output[len(seededSyncDeltaMarker):]
	if len(output) > 0 && output[len(output)-1] != 0 {
		return nil, 0, 0, false
	}
	changed := make(map[string]bool, len(manifest.OverlayFiles))
	for _, rel := range manifest.OverlayFiles {
		changed[rel] = true
	}
	for _, rel := range manifest.seededExtraFiles {
		changed[rel] = true
	}
	for _, rel := range splitNul(output) {
		if !safeRepoRel(rel) || rel == ".gitattributes" || strings.HasSuffix(rel, "/.gitattributes") {
			return nil, 0, 0, false
		}
		changed[rel] = true
	}
	var files []string
	for _, rel := range manifest.Files {
		if changed[rel] {
			files = append(files, rel)
		}
	}
	_, size := changedPathSetBytes(root, files)
	return (SyncManifest{Files: files}).NUL(), len(files), size, true
}

func remoteSeededSyncChanges(workdir string, plan gitCoherencePlan) string {
	// Inspect flags as binary records, without a shell loop over every filename.
	python := `import os, stat, sys
records = open(sys.argv[1], "rb").read().split(b"\0")
if any(record and record[:2] != b"H " for record in records): sys.exit(1)
attrs = open(sys.argv[2], "rb").read().split(b"\0")
if any(value not in (b"", b"unspecified", b"unset") for value in attrs[2::3]): sys.exit(1)
with open(sys.argv[3], "wb") as output:
    for record in open(sys.argv[4], "rb").read().split(b"\0"):
        if not record: continue
        metadata, path = record.split(b"\t", 1)
        fields = metadata.split()
        if fields[1] != b"blob": continue
        try: info = os.lstat(path)
        except FileNotFoundError: continue
        if stat.S_ISREG(info.st_mode) and info.st_size != int(fields[3]): output.write(path + b"\0")
    for record in records:
        if not record: continue
        path = record[2:]
        try: mode = os.lstat(path).st_mode
        except FileNotFoundError: continue
        if stat.S_ISREG(mode) and stat.S_IMODE(mode) not in (0o644, 0o755): output.write(path + b"\0")
`
	perl := `use strict; use warnings;
sub records { open my $f, "<", $_[0] or die $!; binmode $f; local $/; my $data = <$f>; return split /\0/, $data; }
my @files = records($ARGV[0]);
exit 1 if grep { length($_) && substr($_, 0, 2) ne "H " } @files;
my @attrs = records($ARGV[1]);
for (my $i=2; $i<@attrs; $i+=3) { exit 1 if $attrs[$i] ne "unspecified" && $attrs[$i] ne "unset" && $attrs[$i] ne ""; }
open my $out, ">", $ARGV[2] or die $!; binmode $out;
for my $record (records($ARGV[3])) {
  my ($metadata, $path) = split /\t/, $record, 2; my @fields = split /\s+/, $metadata;
  next unless $fields[1] eq "blob";
  my @st = lstat($path); next unless @st;
  print $out $path, "\0" if ($st[2] & 0170000) == 0100000 && $st[7] != $fields[3];
}
for my $record (@files) {
  my $path = substr($record, 2); my @st = lstat($path); next unless @st;
  my $mode = $st[2];
  print $out $path, "\0" if ($mode & 0170000) == 0100000 && ($mode & 07777) != 0644 && ($mode & 07777) != 0755;
}
close $out or die $!;`
	script := `set -eu
` + gitOverlayHermeticFunctions() + remoteExactGitRootFunction() + `
overlay_no_lazy_fetch=1
cd ` + shellPathQuote(workdir) + `
[ ! -L .git ] && overlay_workspace_safe "$PWD"
exact_git_root
[ "$(git rev-parse --verify HEAD^{commit})" = ` + shellQuote(plan.Target) + ` ]
[ "$(git rev-parse --verify HEAD^{tree})" = ` + shellQuote(plan.Tree) + ` ]
[ "$(git write-tree)" = ` + shellQuote(plan.Tree) + ` ]
probe=$(mktemp -d)
trap 'rm -rf -- "$probe"' EXIT
git ls-files -v -z > "$probe/flags"
git ls-files -z > "$probe/paths"
git check-attr -z --stdin ` + strings.Join(seededSyncTransformAttributes, " ") + ` < "$probe/paths" > "$probe/attrs"
git ls-tree -r -l -z --full-tree HEAD > "$probe/sizes"
` + remoteSyncInterpreterCommand(python, perl, `"$probe/flags" "$probe/attrs" "$probe/modes" "$probe/sizes"`) + `
git -c core.trustctime=true -c core.checkStat=default -c core.ignorestat=false diff-files --no-ext-diff --no-textconv --no-renames --name-only -z > "$probe/changed"
printf 'CRABBOX_SEEDED_DELTA_V1\000'
cat "$probe/changed" "$probe/modes"
`
	return remoteHermeticPOSIXControlCommand(script)
}
