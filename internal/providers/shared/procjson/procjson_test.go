package procjson

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	req    core.LocalCommandRequest
	result core.LocalCommandResult
	err    error
	calls  int
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls++
	r.req = req
	return r.result, r.err
}

func TestExchangeEncodesRequestAndOwnsStdin(t *testing.T) {
	type request struct {
		Operation string `json:"operation"`
		Count     int    `json:"count"`
	}
	type response struct {
		Value int `json:"value"`
	}
	runner := &recordingRunner{result: core.LocalCommandResult{Stdout: `{"value":7}`}}
	grace := 2 * time.Second
	got, _, err := Exchange[request, response](context.Background(), runner, core.LocalCommandRequest{
		Name:  "helper",
		Stdin: strings.NewReader("caller-owned"),
	}, request{Operation: "list", Count: 3}, Limits{MaxBytesPerStream: 1024, CancelGrace: grace})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(runner.req.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "{\"operation\":\"list\",\"count\":3}\n" || got.Value != 7 {
		t.Fatalf("payload=%q response=%#v", payload, got)
	}
	if runner.req.MaxCapturedOutputBytes != 1024 || runner.req.CancelGracePeriod != grace {
		t.Fatalf("limits=%d/%s", runner.req.MaxCapturedOutputBytes, runner.req.CancelGracePeriod)
	}
}

func TestExchangeFailuresPreserveResult(t *testing.T) {
	secret := "secret"
	tests := []struct {
		name   string
		result core.LocalCommandResult
		err    error
		stage  Stage
		runErr bool
	}{
		{name: "runner error", result: core.LocalCommandResult{ExitCode: 1, Stderr: secret}, err: errors.New("failed with " + secret), stage: StageRun, runErr: true},
		{name: "nonzero exit", result: core.LocalCommandResult{ExitCode: 9, Stdout: secret}, stage: StageRun},
		{name: "oversized stdout", result: core.LocalCommandResult{ExitCode: 9, Stdout: strings.Repeat("x", 9)}, err: errors.New("exit status 9"), stage: StageOutputLimit},
		{name: "oversized stderr", result: core.LocalCommandResult{Stdout: `{}`, Stderr: strings.Repeat("x", 9)}, stage: StageOutputLimit},
		{name: "empty stdout", result: core.LocalCommandResult{Stdout: " \n\t", Stderr: secret}, stage: StageEmpty},
		{name: "malformed JSON", result: core.LocalCommandResult{Stdout: `{`, Stderr: secret}, stage: StageDecode},
		{name: "trailing JSON", result: core.LocalCommandResult{Stdout: `{} {}`, Stderr: secret}, stage: StageDecode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{result: tc.result, err: tc.err}
			_, result, err := Exchange[struct{}, map[string]any](context.Background(), runner, core.LocalCommandRequest{}, struct{}{}, Limits{MaxBytesPerStream: 8})
			var failure *Failure
			if !errors.As(err, &failure) || failure.Stage != tc.stage {
				t.Fatalf("error=%v stage=%v", err, failure)
			}
			if !reflect.DeepEqual(result, tc.result) || !reflect.DeepEqual(failure.Result, tc.result) {
				t.Fatalf("result=%#v failure result=%#v want=%#v", result, failure.Result, tc.result)
			}
			if (failure.RunErr != nil) != tc.runErr || (failure.Stage == StageRun && failure.Err == nil) {
				t.Fatalf("failure err=%v runErr=%v", failure.Err, failure.RunErr)
			}
			if strings.Contains(failure.Error(), secret) {
				t.Fatalf("failure exposed captured output: %v", failure)
			}
		})
	}
}

func TestExchangeRejectsInvalidSetup(t *testing.T) {
	tests := []struct {
		name   string
		runner core.CommandRunner
		req    core.LocalCommandRequest
		limit  int
	}{
		{name: "nil runner", limit: 1},
		{name: "zero limit", runner: &recordingRunner{}},
		{name: "negative limit", runner: &recordingRunner{}, limit: -1},
		{name: "capture disabled", runner: &recordingRunner{}, req: core.LocalCommandRequest{DisableOutputCapture: true}, limit: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Exchange[struct{}, struct{}](context.Background(), tc.runner, tc.req, struct{}{}, Limits{MaxBytesPerStream: tc.limit})
			if err == nil {
				t.Fatal("expected setup error")
			}
			if runner, ok := tc.runner.(*recordingRunner); ok && runner.calls != 0 {
				t.Fatalf("runner called %d times", runner.calls)
			}
		})
	}
}
