package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestGuardedGroupWorkloadSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, snapshot string
		code           int
	}{
		{"guard only", "printf '10 10 S\\n'", 1},
		{"live child", "printf '10 10 S\\n20 10 S\\n'", 0},
		{"zombie and unrelated", "printf '10 10 S\\n20 10 Z\\n30 30 S\\n'", 1},
		{"guard missing", "printf '30 30 S\\n'", 0},
		{"permission error", "return 1", 0},
		{"malformed", "printf '10 10 S\\nbad snapshot\\n'", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := guardedGroupFunctions + "\ngroup=10; guard=10\nps() { " + tc.snapshot + "; }\ngroup_has_workload\n"
			output, err := exec.Command("bash", "-c", script).CombinedOutput()
			if exitCode(err) != tc.code || len(output) != 0 {
				t.Fatalf("snapshot exit=%d want=%d: %v, %s", exitCode(err), tc.code, err, output)
			}
		})
	}
}

func TestWSL2CleanupTermGrace(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(fmt.Sprintf("exit=%d", code), func(t *testing.T) {
			source := fmt.Sprintf("exit %d\n", code)
			started := time.Now()
			f := startWSLLinuxFixture(t, source, nil, len(source), wslLinuxHelper, 5000)
			f.wait(t, code)
			elapsed := time.Since(started)
			assertWSLLinuxAbsent(t, f)
			t.Logf("exit %d with 5s TERM grace: %s", code, elapsed)
			if elapsed >= 2*time.Second {
				t.Fatalf("clean exit spent TERM grace: %s", elapsed)
			}
		})
	}
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel ignore=%t", ignore), func(t *testing.T) {
			dir := t.TempDir()
			fifo, ready, marker := filepath.Join(dir, "fifo"), filepath.Join(dir, "ready"), filepath.Join(dir, "term")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			trap := "sleep .2; : >" + shellQuote(marker) + "; exit 0"
			grace := 5000
			if ignore {
				trap, grace = "", 1000
			}
			source := "trap " + shellQuote(trap) + " TERM\n: >" + shellQuote(ready) + "\nwhile :; do read -r -t 1 -u 7 ignored || :; done 7<>" + shellQuote(fifo) + "\n"
			f := startWSLLinuxFixture(t, source, nil, len(source), wslLinuxHelper, grace)
			f.guard(t)
			waitForTestFile(t, ready, 5*time.Second)
			started := time.Now()
			_ = f.control.Close()
			f.wait(t, 74)
			elapsed := time.Since(started)
			assertWSLLinuxAbsent(t, f)
			t.Logf("cancel TERM ignore=%t, grace=%dms: %s; group gone", ignore, grace, elapsed)
			if ignore {
				if elapsed < time.Second || elapsed > 3*time.Second {
					t.Fatalf("TERM-ignoring child did not receive bounded grace: %s", elapsed)
				}
			} else {
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("TERM handler did not finish: %v", err)
				}
				if elapsed >= 2*time.Second {
					t.Fatalf("TERM-respecting child spent full grace: %s", elapsed)
				}
			}
		})
	}
}
