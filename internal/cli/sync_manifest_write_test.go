//go:build !windows

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

func TestRemoteWriteSyncManifestsBlockBoundaries(t *testing.T) {
	for _, portable := range []bool{false, true} {
		t.Run(fmt.Sprintf("portable=%t", portable), func(t *testing.T) {
			if portable {
				dd, err := exec.LookPath("dd")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				mustWriteTestBashNoProfileWrapper(t, bin)
				writeExecutable(t, filepath.Join(bin, "dd"), "#!/bin/sh\nfor arg do case \"$arg\" in iflag=*) exit 1 ;; esac; done\nexec "+shellQuote(dd)+" \"$@\"\n")
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			for _, size := range []int{0, 65535, 65536, 65537} {
				t.Run(fmt.Sprint(size), func(t *testing.T) {
					const token = "0123456789abcdef0123456789abcdef"
					root := t.TempDir()
					manifest := bytes.Repeat([]byte("x\x00\xff\n"), (size+3)/4)[:size]
					deleted := []byte("deleted\xff\x00")
					cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", remoteWriteSyncManifestsNew(root, token))
					cmd.Stdin = strings.NewReader(syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, manifest, deleted))
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("write: %v\n%s", err, out)
					}
					for name, want := range map[string][]byte{remoteSyncPendingManifestName(token): manifest, remoteSyncPendingDeletedName(token): deleted} {
						got, err := os.ReadFile(filepath.Join(root, ".crabbox", name))
						if err != nil || !bytes.Equal(got, want) {
							t.Fatalf("incorrect %s bytes=%d want=%d err=%v", name, len(got), len(want), err)
						}
					}
				})
			}
		})
	}
}
