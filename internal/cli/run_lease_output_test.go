package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCommandRejectsLeaseOutputCollisionsBeforeAcquire(t *testing.T) {
	for _, flag := range []string{"--download", "--download-on-failure", "--capture-stdout", "--capture-stderr"} {
		for _, alias := range []string{"exact", "relative", "dot", "symlink parent", "hardlink", "case"} {
			t.Run(flag+"/"+alias, func(t *testing.T) {
				clearConfigEnv(t)
				dir := t.TempDir()
				isolateRunTestUserDirs(t, dir)
				t.Chdir(dir)
				t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
				lease := filepath.Join(dir, "session.json")
				const sentinel = "existing lease bytes\n"
				if err := os.WriteFile(lease, []byte(sentinel), 0600); err != nil {
					t.Fatal(err)
				}
				other := lease
				switch alias {
				case "relative":
					other = "session.json"
				case "dot":
					other = dir + string(filepath.Separator) + "." + string(filepath.Separator) + "session.json"
				case "symlink parent":
					if err := os.Symlink(dir, filepath.Join(dir, "link")); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
					other = filepath.Join(dir, "link", "session.json")
				case "hardlink":
					other = filepath.Join(dir, "hardlink.json")
					if err := os.Link(lease, other); err != nil {
						t.Skipf("hardlink unavailable: %v", err)
					}
				case "case":
					other = filepath.Join(dir, "SESSION.JSON")
					left, _ := os.Stat(lease)
					right, err := os.Stat(other)
					if err != nil || !os.SameFile(left, right) {
						t.Skip("case-sensitive filesystem")
					}
				}
				calls := 0
				runEnvProfileTestAcquireHook = func(AcquireRequest) { calls++ }
				t.Cleanup(func() { runEnvProfileTestAcquireHook = nil })
				value := other
				if strings.HasPrefix(flag, "--download") {
					value = "report=" + other
				}
				err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(t.Context(), []string{
					"--provider", "local-container", "--keep", "--stop-after", "never",
					"--lease-output", " " + lease + " ", flag, value, "--", "true",
				})
				if exitCodeForError(err, 0) != 2 || !strings.Contains(fmt.Sprint(err), "lease output/") || !strings.Contains(fmt.Sprint(err), "paths must be different") || calls != 0 {
					t.Fatalf("error=%v acquisition calls=%d; want collision before acquisition", err, calls)
				}
				for _, path := range []string{lease, other} {
					data, err := os.ReadFile(path)
					if err != nil || string(data) != sentinel {
						t.Fatalf("preflight changed %s: %q, %v", path, data, err)
					}
				}
			})
		}
	}
}

func TestRunCommandLeaseOutputWithDistinctDownloads(t *testing.T) {
	for _, code := range []int{0, 23} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			dir, leaseTarget := setupLocalContainerRunSessionTest(t, "")
			leaseTarget.SSH.ReadyCheck = "exit 0"
			runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) { return leaseTarget, nil }
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(`#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
exec sh -c "$cmd"
`), 0755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			t.Setenv("CRABBOX_WORK_ROOT", filepath.Join(dir, "remote"))
			lease := filepath.Join(dir, "session.json")
			report := filepath.Join(dir, "report.txt")
			flag := "--download"
			if code != 0 {
				flag = "--download-on-failure"
			}
			var stderr bytes.Buffer
			err := (App{Stdout: io.Discard, Stderr: &stderr}).runCommand(t.Context(), []string{
				"--provider", "local-container", "--keep", "--stop-after", "never", "--no-sync",
				"--lease-output", lease, flag, "report=" + report,
				"--shell", "--", fmt.Sprintf("printf evidence > report; exit %d", code),
			})
			if exitCodeForError(err, 0) != code {
				t.Fatalf("run: %v\n%s", err, stderr.String())
			}
			session, _, _ := readRunSessionHandleTest(t, lease)
			if session.LeaseID != localContainerRunSessionTestLeaseID || !session.Kept {
				t.Fatalf("session=%+v", session)
			}
			if data, err := os.ReadFile(report); err != nil || string(data) != "evidence" {
				t.Fatalf("download=%q err=%v\n%s", data, err, stderr.String())
			}
		})
	}
}
