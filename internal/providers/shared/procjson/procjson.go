package procjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Limits struct {
	MaxBytesPerStream int
	CancelGrace       time.Duration
}

type Stage string

const (
	StageRun         Stage = "run"
	StageOutputLimit Stage = "output-limit"
	StageEmpty       Stage = "empty"
	StageDecode      Stage = "decode"
)

type Failure struct {
	Stage  Stage
	Result core.LocalCommandResult
	Err    error
	RunErr error
}

func (f *Failure) Error() string {
	if f.Stage != StageRun {
		return fmt.Sprintf("JSON exchange failed at %s: %v", f.Stage, f.Err)
	}
	return "JSON exchange failed at run"
}
func (f *Failure) Unwrap() error { return f.Err }

func Exchange[Input, Output any](ctx context.Context, runner core.CommandRunner, req core.LocalCommandRequest, input Input, limits Limits) (output Output, result core.LocalCommandResult, err error) {
	if runner == nil || limits.MaxBytesPerStream <= 0 || req.DisableOutputCapture {
		return output, result, fmt.Errorf("invalid JSON exchange setup: runner and positive output limit required; capture must be enabled")
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return output, core.LocalCommandResult{}, fmt.Errorf("encode JSON request: %w", err)
	}
	req.Stdin, req.MaxCapturedOutputBytes, req.CancelGracePeriod = bytes.NewReader(append(payload, '\n')), limits.MaxBytesPerStream, limits.CancelGrace
	result, err = runner.Run(ctx, req)
	runErr := err
	if len(result.Stdout) > limits.MaxBytesPerStream || len(result.Stderr) > limits.MaxBytesPerStream {
		return output, result, &Failure{Stage: StageOutputLimit, Result: result, Err: fmt.Errorf("captured output exceeded %d-byte output limit", limits.MaxBytesPerStream)}
	}
	if err != nil || result.ExitCode != 0 {
		if err == nil {
			err = fmt.Errorf("command exited with code %d", result.ExitCode)
		}
		return output, result, &Failure{Stage: StageRun, Result: result, Err: err, RunErr: runErr}
	}
	if len(bytes.TrimSpace([]byte(result.Stdout))) == 0 {
		return output, result, &Failure{Stage: StageEmpty, Result: result, Err: fmt.Errorf("stdout is empty")}
	}
	if err := json.Unmarshal([]byte(result.Stdout), &output); err != nil {
		return output, result, &Failure{Stage: StageDecode, Result: result, Err: err}
	}
	return output, result, nil
}
