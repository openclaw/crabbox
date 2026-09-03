package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Synchronous POSIX helpers may omit the race runtime's exit sleep while
// retaining inherited detector options. Windows keeps its original execution path.
func synchronousHelperRacePrefix() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "GORACE=\"$GORACE atexit_sleep_ms=0\" "
}

func synchronousTestHelperCommand(testName string) []string {
	filter := "-test.run=^" + testName + "$"
	if runtime.GOOS == "windows" {
		return []string{os.Args[0], filter}
	}
	return []string{"/bin/sh", "-c", synchronousHelperRacePrefix() + `exec "$@"`, "synchronous-test-helper", os.Args[0], filter}
}

func TestSynchronousTestHelperEnvironment(t *testing.T) {
	for _, options := range []string{"", "history_size=2 exitcode=73", "atexit_sleep_ms=123 history_size=2 exitcode=73"} {
		for _, code := range []string{"0", "7"} {
			t.Run(options+"/exit="+code, func(t *testing.T) {
				parentEnv := os.Environ()
				// Remove only our synthetic inputs, then supply a private child copy.
				var env []string
				for _, entry := range parentEnv {
					key, _, _ := strings.Cut(entry, "=")
					if !strings.EqualFold(key, "GORACE") && !strings.HasPrefix(key, "CRABBOX_SYNC_HELPER_") {
						env = append(env, entry)
					}
				}
				env = append(env, "GORACE="+options, "CRABBOX_SYNC_HELPER_EXIT="+code, "CRABBOX_SYNC_HELPER_KEEP=preserved")
				before := append([]string(nil), env...)
				args := synchronousTestHelperCommand("TestSynchronousTestHelperProcess")
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, args[0], args[1:]...)
				cmd.Env = env
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				if ctx.Err() != nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != int(code[0]-'0') || (err == nil) != (code == "0") {
					t.Fatalf("helper exit=%v err=%v context=%v stderr=%s", cmd.ProcessState, err, ctx.Err(), &stderr)
				}
				var got map[string]string
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatalf("helper output=%q: %v", &stdout, err)
				}
				wantRace := options
				if runtime.GOOS != "windows" {
					wantRace += " atexit_sleep_ms=0"
				}
				if got["race"] != wantRace || got["keep"] != "preserved" || stderr.String() != "helper stderr\n" {
					t.Fatalf("helper output=%v stderr=%q", got, &stderr)
				}
				if !reflect.DeepEqual(env, before) || !reflect.DeepEqual(os.Environ(), parentEnv) {
					t.Fatal("helper changed its input or parent environment")
				}
			})
		}
	}
}

func TestSynchronousTestHelperProcess(t *testing.T) {
	code := os.Getenv("CRABBOX_SYNC_HELPER_EXIT")
	if code == "" {
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"race": os.Getenv("GORACE"), "keep": os.Getenv("CRABBOX_SYNC_HELPER_KEEP")})
	_, _ = os.Stderr.WriteString("helper stderr\n")
	if code == "7" {
		os.Exit(7)
	}
	os.Exit(0)
}
