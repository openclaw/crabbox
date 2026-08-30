package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxArtifactChangePaths      = 32
	maxArtifactChangeFileBytes  = 5 * 1024 * 1024
	maxArtifactChangeTotalBytes = 20 * 1024 * 1024
)

type ArtifactChangeResult struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type artifactChangeSnapshot struct {
	present bool
	digest  [sha256.Size]byte
	data    []byte
}

func validateArtifactChangePaths(paths []string) error {
	if len(paths) > maxArtifactChangePaths {
		return exit(2, "--require-artifact-change accepts at most %d paths", maxArtifactChangePaths)
	}
	for _, p := range paths {
		if len(p) > 1024 || strings.TrimSpace(p) != p || !safeArtifactGlob(p) || strings.ContainsAny(p, "*?") || path.Clean(p) != p || p == "." {
			return exit(2, "--require-artifact-change requires exact safe relative file paths: %s", p)
		}
		for _, component := range strings.Split(p, "/") {
			if component == ".git" || component == ".crabbox" {
				return exit(2, "--require-artifact-change excludes protected paths: %s", p)
			}
		}
	}
	return nil
}

func validateArtifactChangeTarget(target SSHTarget, paths []string) error {
	if len(paths) > 0 && target.TargetOS != targetLinux {
		return exit(2, "--require-artifact-change requires an ordinary SSH-backed Linux target")
	}
	return nil
}

func initialArtifactChanges(paths []string) []ArtifactChangeResult {
	var results []ArtifactChangeResult
	for _, p := range paths {
		results = append(results, ArtifactChangeResult{Path: p, Status: "not-evaluated"})
	}
	return results
}

// Snapshots travel over stdin/stdout, never argv or retained workspace files.
// Keep the post-command bytes so collection cannot reread a different file.
func snapshotArtifactChanges(ctx context.Context, target SSHTarget, workdir string, paths []string) ([]artifactChangeSnapshot, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	var output synchronizedBuffer
	output.limit = 4*((maxArtifactChangeTotalBytes+2)/3) + 1024
	var diagnostic synchronizedBuffer
	diagnostic.limit = 4096
	script := artifactChangeSnapshotScript(workdir, paths)
	if err := runSSHInput(ctx, target, remoteRunArtifactShellInputCommand(target), strings.NewReader(script), &output, &diagnostic); err != nil {
		return nil, exit(7, "artifact change snapshot: %v: %s", err, strings.TrimSpace(diagnostic.String()))
	}
	return parseArtifactChangeSnapshot(output.String(), paths)
}

func artifactChangeSnapshotScript(workdir string, paths []string) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\ncd " + shellQuote(workdir) + "\n")
	fmt.Fprintf(&b, "total=0\nfile_limit=%d\ntotal_limit=%d\n", maxArtifactChangeFileBytes, maxArtifactChangeTotalBytes)
	b.WriteString(`snapshot_artifact() {
  local rel="$1" rest="$1" component current= encoded size limit
  while [ -n "$rest" ]; do
    component=${rest%%/*}
    if [ "$component" = "$rest" ]; then rest=; else rest=${rest#*/}; fi
    current=${current:+$current/}$component
    if [ -L "$current" ]; then printf 'artifact change rejects symlink: %s\n' "$rel" >&2; return 7; fi
    if [ ! -e "$current" ]; then printf 'missing\n'; return; fi
    if [ -n "$rest" ] && [ ! -d "$current" ]; then printf 'artifact change rejects non-directory parent: %s\n' "$rel" >&2; return 7; fi
  done
  if [ ! -f "$rel" ]; then printf 'artifact change requires regular file: %s\n' "$rel" >&2; return 7; fi
  limit=$file_limit
  if [ "$((total_limit-total))" -lt "$limit" ]; then limit=$((total_limit-total)); fi
  encoded=$(head -c "$((limit+1))" -- "$rel" | base64 | tr -d '\n')
  size=$((${#encoded}/4*3))
  case "$encoded" in *==) size=$((size-2));; *=) size=$((size-1));; esac
  if [ "$size" -gt "$limit" ]; then printf 'artifact change snapshot byte limit exceeded: %s\n' "$rel" >&2; return 7; fi
  total=$((total+size))
  printf 'file %s\n' "$encoded"
}
`)
	for _, p := range paths {
		b.WriteString("snapshot_artifact " + shellQuote(p) + "\n")
	}
	return b.String()
}

func parseArtifactChangeSnapshot(output string, paths []string) ([]artifactChangeSnapshot, error) {
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != len(paths) {
		return nil, exit(7, "incomplete artifact change snapshot")
	}
	snapshots := make([]artifactChangeSnapshot, len(paths))
	total := 0
	for i, line := range lines {
		if line == "missing" {
			continue
		}
		encoded, ok := strings.CutPrefix(line, "file ")
		if !ok {
			return nil, exit(7, "invalid artifact change snapshot for %s", paths[i])
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		total += len(data)
		if err != nil || len(data) > maxArtifactChangeFileBytes || total > maxArtifactChangeTotalBytes {
			return nil, exit(7, "invalid or oversized artifact change snapshot for %s", paths[i])
		}
		snapshots[i] = artifactChangeSnapshot{present: true, digest: sha256.Sum256(data), data: data}
	}
	return snapshots, nil
}

func compareArtifactChanges(paths []string, before, after []artifactChangeSnapshot) ([]ArtifactChangeResult, error) {
	results := initialArtifactChanges(paths)
	var failed []string
	for i, p := range paths {
		switch {
		case !after[i].present:
			results[i].Status = "missing"
		case !before[i].present:
			results[i].Status = "created"
		case before[i].digest != after[i].digest:
			results[i].Status = "changed"
		default:
			results[i].Status = "unchanged"
		}
		if results[i].Status == "missing" || results[i].Status == "unchanged" {
			failed = append(failed, p+": "+results[i].Status)
		}
	}
	if len(failed) > 0 {
		return results, exit(7, "required artifact change failed: %s", strings.Join(failed, "; "))
	}
	return results, nil
}

func collectChangedArtifacts(repoRoot, runID, leaseID string, results []ArtifactChangeResult, snapshots []artifactChangeSnapshot) ([]runArtifact, error) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for i, result := range results {
		if result.Status != "created" && result.Status != "changed" {
			continue
		}
		data := snapshots[i].data
		if err := tw.WriteHeader(&tar.Header{Name: result.Path, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	name := safeCaptureName(firstNonBlank(runID, leaseID, "run")) + "-artifacts.tgz"
	local := localRunArtifactPath(repoRoot, runID, leaseID, name)
	if err := createPrivateRunOutputDir(filepath.Dir(local)); err != nil {
		return nil, exit(7, "create artifact change archive directory: %v", err)
	}
	if err := writePrivateRunOutputFile(local, archive.Bytes()); err != nil {
		return nil, exit(7, "write artifact change archive: %v", err)
	}
	return []runArtifact{{Kind: "artifact-change", Path: local, Bytes: archive.Len()}}, nil
}
