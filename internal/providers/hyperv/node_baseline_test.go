package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestEnsureNodeRunsTheSharedNodeBaselineInTheGuest(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	if err := b.ensureNode(context.Background(), "crabbox-blue-1234", "crabbox"); err != nil {
		t.Fatalf("ensureNode: %v", err)
	}
	var script string
	for _, call := range runner.calls {
		if s := call.Args[len(call.Args)-1]; strings.Contains(s, "Invoke-Command") && strings.Contains(s, "Ensure-CrabboxNode") {
			script = s
		}
	}
	if script == "" {
		t.Fatal("the Node baseline was not installed over PowerShell Direct")
	}
	if !strings.Contains(script, "Assert-CrabboxFileSHA256") || !strings.Contains(script, "function Retry") {
		t.Fatalf("baseline script lost its verification or retry helpers: %.300s", script)
	}
}

func TestStartVMTreatsAFailedStartCommandAsSuccessWhenTheVMIsRunning(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     int
		wantError bool
	}{
		{"vm started despite the reported failure", hypervStateRunning, false},
		{"vm genuinely not started", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
			runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				script := req.Args[len(req.Args)-1]
				switch {
				case strings.Contains(script, "Start-VM"):
					return core.LocalCommandResult{Stderr: "signal: killed"}, errors.New("exit status 1"), true
				case strings.Contains(script, "Get-VM"):
					out, err := json.Marshal(hypervVM{Name: "crabbox-blue-1234", State: tc.state})
					if err != nil {
						t.Fatal(err)
					}
					return core.LocalCommandResult{Stdout: string(out)}, nil, true
				}
				return core.LocalCommandResult{}, nil, false
			}
			b := testBackend(runner)
			err := b.startVM(context.Background(), "crabbox-blue-1234")
			if (err != nil) != tc.wantError {
				t.Fatalf("startVM error=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}
