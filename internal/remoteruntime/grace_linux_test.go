package remoteruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeCommandNormalExitSkipsTermGrace(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			cmd := nativeCommand(t, nativeNonce(t), fmt.Sprintf("exit %d\n", code), nil, 0, false, false)
			cmd.Args[8] = "5000"
			started := time.Now()
			output, err := cmd.CombinedOutput()
			elapsed := time.Since(started)
			if commandExitCode(err) != code || len(output) != 0 {
				t.Fatalf("exit: %v, output=%q", err, output)
			}
			t.Logf("exit %d with 5s TERM grace: %s", code, elapsed)
			if elapsed >= 2*time.Second {
				t.Fatalf("clean exit spent TERM grace: %s", elapsed)
			}
		})
	}
}

func TestNativeCommandBackgroundTermGrace(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore=%t", ignore), func(t *testing.T) {
			dir := t.TempDir()
			ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "term")
			trap := fmt.Sprintf("sleep .2; echo term > %s; exit 0", marker)
			grace := 5 * time.Second
			if ignore {
				trap, grace = "", time.Second
			}
			// The command exits only after its background child has installed
			// the signal handler. Cleanup must still account for that child.
			source := fmt.Sprintf("sh -c 'trap %q TERM; echo $$ > %s; while :; do sleep .02; done' >/dev/null 2>&1 &\nwhile [ ! -s %s ]; do sleep .01; done\nexit 0\n", trap, ready, ready)
			cmd := nativeCommand(t, nativeNonce(t), source, nil, 0, false, false)
			cmd.Args[8] = strconv.FormatInt(grace.Milliseconds(), 10)
			started := time.Now()
			output, err := cmd.CombinedOutput()
			elapsed := time.Since(started)
			if err != nil || len(output) != 0 {
				t.Fatalf("background cleanup: %v, output=%q", err, output)
			}
			data, err := os.ReadFile(ready)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("background child %d survived: %v", pid, err)
			}
			t.Logf("TERM ignore=%t, grace=%s: %s; child gone", ignore, grace, elapsed)
			if ignore {
				if elapsed < grace || elapsed > grace+2*time.Second {
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
