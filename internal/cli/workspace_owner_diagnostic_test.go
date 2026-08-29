package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestWorkspaceOwnerSetupFailureDoesNotRetrySSHPort(t *testing.T) {
	err := &workspaceOwnerSetupError{phase: "handoff", cause: exit(255, "connection closed")}
	if shouldRetrySSHPort(err) {
		t.Fatal("proven setup failure was retried as transport failure")
	}
}

func TestWorkspaceOwnerSetupDiagnosticFraming(t *testing.T) {
	marker := workspaceOwnerSetupPrefix + strings.Repeat("1", 32)
	failure := marker + " failed registration\n"
	started := marker + " started\n"
	for _, test := range []struct {
		name, input, output, phase string
	}{
		{"setup", failure, "", "registration"},
		{"first-failure", failure + marker + " failed handoff\n", "", "registration"},
		{"workload", started + failure, failure, ""},
		{"other-invocation", workspaceOwnerSetupPrefix + "other failed registration\n", workspaceOwnerSetupPrefix + "other failed registration\n", ""},
		{"partial", strings.TrimSuffix(failure, "\n"), strings.TrimSuffix(failure, "\n"), ""},
		{"embedded", "warning: " + failure, "warning: " + failure, ""},
		{"unknown-phase", marker + " failed arbitrary-output\n", marker + " failed arbitrary-output\n", ""},
		{"bounded", strings.Repeat("x", 1024*1024) + "\n" + failure, strings.Repeat("x", 1024*1024) + "\n", "registration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, chunkSize := range []int{1, 17, 4096} {
				var out bytes.Buffer
				writer := workspaceOwnerSetupWriter{destination: &out, marker: marker}
				data := []byte(test.input)
				for len(data) > 0 {
					n := min(chunkSize, len(data))
					if count, err := writer.Write(data[:n]); err != nil || count != n {
						t.Fatalf("write count=%d err=%v", count, err)
					}
					if len(writer.pending) >= 160 {
						t.Fatal("diagnostic parser exceeded its buffer bound")
					}
					data = data[n:]
				}
				if err := writer.flush(); err != nil {
					t.Fatal(err)
				}
				if out.String() != test.output || writer.failure != test.phase {
					t.Fatalf("chunk=%d output-bytes=%d phase=%q", chunkSize, out.Len(), writer.failure)
				}
			}
		})
	}
}
