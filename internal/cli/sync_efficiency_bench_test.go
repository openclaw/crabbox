//go:build darwin || linux

package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Run with scripts/benchmark-sync.py. Input is read-only; fixture generation and
// edits happen outside the timed region. One iteration is one complete phase.
func BenchmarkSyncEfficiency(b *testing.B) {
	root := os.Getenv("CRABBOX_BENCH_REPO")
	if root == "" {
		b.Skip("set CRABBOX_BENCH_REPO to a disposable Git fixture")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		b.Fatal(err)
	}
	bin := b.TempDir()
	gitLog := filepath.Join(bin, "calls")
	wrapper := "#!/bin/sh\nprintf x >> " + shellQuote(gitLog) + "\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700); err != nil {
		b.Fatal(err)
	}
	b.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	originalGit := gitOverlayGitExecutable
	gitOverlayGitExecutable = filepath.Join(bin, "git")
	b.Cleanup(func() { gitOverlayGitExecutable = originalGit })
	cfg := defaultConfig()
	repo := Repo{Root: root, Head: gitOutput(root, "rev-parse", "HEAD")}
	excludes, err := syncExcludes(root, cfg)
	if err != nil {
		b.Fatal(err)
	}
	manifest, err := syncManifestFilteredRules(root, excludes, nil)
	if err != nil {
		b.Fatal(err)
	}
	for _, phase := range []string{"manifest", "fingerprint", "fingerprint-full", "snapshot-copy", "snapshot-local", "plan"} {
		b.Run(phase, func(b *testing.B) {
			if err := os.WriteFile(gitLog, nil, 0o600); err != nil {
				b.Fatal(err)
			}
			var start, end syscall.Rusage
			counter := &sourceReadCounter{}
			ctx := context.WithValue(context.Background(), sourceReadCounterKey{}, counter)
			_ = syscall.Getrusage(syscall.RUSAGE_SELF, &start)
			b.ResetTimer()
			for range b.N {
				switch phase {
				case "manifest":
					_, err = syncManifestFilteredRules(root, excludes, nil)
				case "fingerprint", "fingerprint-full":
					paths := manifest.Changed
					if phase == "fingerprint-full" {
						paths = manifest.Files
					}
					err = syncFingerprintPaths(ctx, sha256.New(), root, paths, true)
				case "snapshot-copy":
					var snapshot sourceSnapshot
					snapshot, err = newSourceSnapshot()
					if err == nil {
						err = copySourceSnapshotOwned(ctx, root, &snapshot, manifest.Files, 0, nil)
						if cleanupErr := snapshot.cleanup(); err == nil {
							err = cleanupErr
						}
					}
				case "snapshot-local":
					var snapshot gitOverlaySnapshot
					snapshot, err = prepareLocalGitSeedSnapshot(ctx, repo, cfg, excludes)
					if cleanupErr := snapshot.cleanup(); err == nil {
						err = cleanupErr
					}
				case "plan":
					_, _ = syncPlanRows(root, manifest, 20)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			_ = syscall.Getrusage(syscall.RUSAGE_SELF, &end)
			calls, err := os.ReadFile(gitLog)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(len(calls))/float64(b.N), "git/op")
			b.ReportMetric(float64(end.Inblock-start.Inblock)/float64(b.N), "inblock/op")
			b.ReportMetric(float64(counter.bytes.Load())/float64(b.N), "read-bytes/op")
			b.ReportMetric(float64(counter.hashes.Load())/float64(b.N), "hashes/op")
			b.ReportMetric(float64(len(manifest.Files)), "files")
			b.ReportMetric(float64(manifest.Bytes), "candidate-bytes")
			b.ReportMetric(float64(manifest.ChangedBytes), "dirty-bytes")
			b.Log(fmt.Sprintf("root=%s phase=%s; inblock is physical block input, not logical bytes read", root, phase))
		})
	}
}
