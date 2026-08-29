package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestShellChainDiagnosticFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/shell-chain-diagnostics.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name     string   `json:"name"`
		Command  string   `json:"command"`
		Segments []string `json:"segments"`
		ExitCode *int     `json:"exitCode"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			if got := shellAndChainSegments(fixture.Command); !reflect.DeepEqual(got, fixture.Segments) {
				t.Fatalf("segments=%q want=%q", got, fixture.Segments)
			}
			var out bytes.Buffer
			printFailureDigestShellChain(&out, runFailureDigestInput{ShellMode: true, CommandDisplay: fixture.Command})
			for _, field := range []string{"shell_chain:", "would_skip_if_left_failed:", "chain_semantics:"} {
				if got := strings.Contains(out.String(), field); got != (len(fixture.Segments) > 1) {
					t.Fatalf("field %s: diagnostic=%q", field, out.String())
				}
			}
			if fixture.ExitCode != nil && runtime.GOOS != "windows" {
				cmd := exec.Command("/bin/sh", "-c", fixture.Command)
				output, err := cmd.CombinedOutput()
				if cmd.ProcessState == nil {
					t.Fatalf("start original workload: %v", err)
				}
				if got := cmd.ProcessState.ExitCode(); got != *fixture.ExitCode {
					t.Fatalf("original workload exit=%d want=%d: %v %s", got, *fixture.ExitCode, err, output)
				}
			}
		})
	}
}
