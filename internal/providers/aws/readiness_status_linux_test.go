package aws

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Use the emitted argv and real Linux pgrep. Only the Python body is replaced
// with a pipe wait, so this cannot depend on tree size or scheduler timing.
func TestAWSReadinessStatusRecognizesEmittedOwnershipChild(t *testing.T) {
	generated := core.CloudInitUserData(core.Config{TargetOS: "linux", SSHUser: "root", WorkRoot: "/work/my app", SSHPort: "2222"}, "fixture")
	invocation := ""
	for _, line := range strings.Split(generated, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PATH=/usr/sbin:/usr/bin:/sbin:/bin python3 ") {
			invocation = strings.TrimSpace(line)
			break
		}
	}
	if invocation == "" {
		t.Fatal("missing emitted ownership child")
	}
	for _, tc := range []struct {
		name, command, expected string
		otherParent             bool
	}{
		{name: "current recipe", command: invocation, expected: "pending ownership-repair"},
		{name: "legacy Python recipe", command: strings.Replace(invocation, "crabbox-ownership-repair-v1 ", "", 1), expected: "pending ownership-repair"},
		{name: "unrelated Python", command: "python3 -I - unrelated-task <<'CRABBOX_OWNERSHIP'", expected: "pending bootstrap"},
		{name: "different bootstrap parent", command: invocation, otherParent: true, expected: "pending bootstrap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			gateRead, gateWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer gateRead.Close()
			defer gateWrite.Close()
			// The final ':' keeps bash as the parent, like the real bootstrap.
			child := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-c", tc.command+"\nimport os\nprint('started', flush=True)\nos.read(3, 1)\nCRABBOX_OWNERSHIP\n:\n")
			child.ExtraFiles = []*os.File{gateRead}
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				gateWrite.Close()
				if err := child.Wait(); err != nil {
					t.Errorf("fixture child: %v", err)
				}
			}()
			started := make(chan bool, 1)
			go func() {
				line, err := bufio.NewReader(stdout).ReadString('\n')
				started <- err == nil && line == "started\n"
			}()
			select {
			case ok := <-started:
				if !ok {
					t.Fatal("Python child did not start")
				}
			case <-ctx.Done():
				t.Fatal("Python child start timed out")
			}
			dir := t.TempDir()
			pid := child.Process.Pid
			if tc.otherParent {
				pid = os.Getpid() // Python is a grandchild, not this process's child.
			}
			systemctl := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in is-failed) exit 1;; show) if [ \"$2\" = --property=MainPID ]; then echo %d; else echo inactive; fi;; cat) exit 0;; *) exit 1;; esac\n", pid)
			if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(systemctl), 0755); err != nil {
				t.Fatal(err)
			}
			script := strings.NewReplacer("/run/crabbox/workspace-ready", filepath.Join(dir, "workspace"), "/var/lib/crabbox/bootstrapped", filepath.Join(dir, "bootstrapped")).Replace(awsLinuxReadinessStatus)
			observer := exec.CommandContext(ctx, "sh", "-c", script)
			observer.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
			out, err := observer.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != tc.expected {
				t.Fatalf("observer=%q error=%v want %q", out, err, tc.expected)
			}
		})
	}
}
