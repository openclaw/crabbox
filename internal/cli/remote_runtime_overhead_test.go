package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNativeLinuxConnectionLifetimes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	for _, tc := range []struct {
		name         string
		run, receipt int
		missing      bool
	}{
		{"success", 0, 0, false}, {"command failure", 7, 0, false}, {"transport failure", 255, 0, false}, {"retirement failure", 0, 74, false}, {"lost receipt", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			t.Setenv("NATIVE_CALLS", log)
			t.Setenv("NATIVE_RUN_EXIT", fmt.Sprint(tc.run))
			t.Setenv("NATIVE_RECEIPT_EXIT", fmt.Sprint(tc.receipt))
			t.Setenv("NATIVE_MISSING", fmt.Sprint(tc.missing))
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			writeExecutable(t, filepath.Join(dir, "ssh"), `#!/bin/sh
printf '%s\n' "$*" >> "$NATIVE_CALLS"
for arg; do remote=$arg; done
case "$remote" in
 *"'run'"*) printf workload; exit "$NATIVE_RUN_EXIT";;
 *"'control'"*)
   if [ "$NATIVE_MISSING" != true ]; then printf 'CBX-PREFLIGHT-1\n%s\nready\nworker-quiesced\nscratch-removed\ncomplete\n' "$NATIVE_NONCE"; fi
   exit "$NATIVE_RECEIPT_EXIT";;
 *) exit 99;;
esac
`)
			target := SSHTarget{Host: "fixture.invalid", User: "fixture", Port: "22", TargetOS: targetLinux}
			native := &remoteNativeRuntime{target: target, path: "/fixture/runtime"}
			transport, err := native.prepareCommand("true", nil, 0, sshCommandLimit{})
			if err != nil {
				t.Fatal(err)
			}
			defer transport.close()
			t.Setenv("NATIVE_NONCE", transport.nativeNonce)
			output := newSynchronizedBuffer(0)
			err = transport.runNative(context.Background(), target, "2", "1", &output, io.Discard)
			if (err != nil) != (tc.run != 0 || tc.receipt != 0 || tc.missing) {
				t.Fatalf("result: %v", err)
			}
			if output.String() != "workload" {
				t.Fatalf("output changed: %q", output.String())
			}
			if transport.retirementUnconfirmed != (tc.receipt != 0 || tc.missing) {
				t.Fatalf("retirement: %v", err)
			}
			data, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatal(readErr)
			}
			calls := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(calls) != 2 {
				t.Fatalf("got %d calls, want workload plus settlement:\n%s", len(calls), data)
			}
			if !strings.Contains(calls[0], "ControlMaster=auto") {
				t.Errorf("workload did not reuse transport: %s", calls[0])
			}
			if !strings.Contains(calls[1], "ControlMaster=auto") || !strings.Contains(calls[1], "'retire'") {
				t.Errorf("settlement did not share one multiplexed round trip: %s", calls[1])
			}
			action := "'observe'"
			if tc.run != 0 {
				action = "'cancel'"
			}
			if !strings.Contains(calls[1], action) {
				t.Errorf("wrong completion action: %s", calls[1])
			}
		})
	}
}
