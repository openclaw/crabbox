//go:build darwin || linux

package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture creation is outside the timed region. Each operation executes the
// production receiver, including framing, metadata setup and length checks.
func BenchmarkRemoteWriteSyncManifests(b *testing.B) {
	for _, files := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprint(files), func(b *testing.B) {
			root := b.TempDir()
			var manifest strings.Builder
			for i := range files {
				name := fmt.Sprintf("packages/package-%04d/src/components/component-%05d.test.ts", i/100, i)
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					b.Fatal(err)
				}
				manifest.WriteString(name + "\x00")
			}
			const token = "0123456789abcdef0123456789abcdef"
			deleted := []byte("old.txt\x00newline\nname\xff\x00")
			data := []byte(manifest.String())
			input := syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, data, deleted)
			command := remoteWriteSyncManifestsNew(root, token)
			b.SetBytes(int64(len(data) + len(deleted)))
			b.ResetTimer()
			for range b.N {
				cmd := exec.Command("/bin/sh", "-c", command)
				cmd.Stdin = strings.NewReader(input)
				if out, err := cmd.CombinedOutput(); err != nil {
					b.Fatalf("write manifest: %v\n%s", err, out)
				}
			}
			b.StopTimer()
			for name, want := range map[string][]byte{
				remoteSyncPendingManifestName(token): data,
				remoteSyncPendingDeletedName(token):  deleted,
			} {
				got, err := os.ReadFile(filepath.Join(root, ".crabbox", name))
				if err != nil || !bytes.Equal(got, want) {
					b.Fatalf("incorrect %s: bytes=%d want=%d err=%v", name, len(got), len(want), err)
				}
			}
			b.ReportMetric(float64(files), "files/op")
		})
	}
}
