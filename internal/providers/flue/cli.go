package flue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const flueCapturedOutputLimitBytes = defaultStdoutLimitBytes + defaultStderrLimitBytes

type flueCLI struct {
	cfg FlueConfig
	rt  Runtime
}

type flueRunResult struct {
	Response Response
	Raw      LocalCommandResult
}

func newFlueCLI(cfg Config, rt Runtime) (*flueCLI, error) {
	if rt.Exec == nil {
		return nil, exit(2, "provider=%s requires Runtime.Exec to invoke the Flue CLI", providerName)
	}
	if err := ValidateFlueConfig(cfg); err != nil {
		return nil, err
	}
	return &flueCLI{cfg: cfg.Flue, rt: rt}, nil
}

func (c *flueCLI) run(ctx context.Context, requestFile string, redactions []string) (flueRunResult, error) {
	target := strings.ToLower(strings.TrimSpace(c.cfg.Target))
	if target == "" {
		target = defaultTarget
	}
	if target != defaultTarget {
		return flueRunResult{}, exit(2, "provider=%s supports flue target=node only in v1; upload/HTTP staging is required before %q can be used", providerName, target)
	}
	workflow := strings.TrimSpace(c.cfg.Workflow)
	if workflow == "" {
		return flueRunResult{}, exit(2, "flue workflow must not be empty")
	}
	input, err := json.Marshal(CLIInput{RequestFile: requestFile})
	if err != nil {
		return flueRunResult{}, fmt.Errorf("encode flue request pointer: %w", err)
	}
	args := []string{"run", workflowSelector(workflow), "--target", target, "--input", string(input)}
	if root := strings.TrimSpace(c.cfg.Root); root != "" {
		args = append(args, "--root", root)
	}
	if config := strings.TrimSpace(c.cfg.Config); config != "" {
		args = append(args, "--config", config)
	}
	if envFile := strings.TrimSpace(c.cfg.EnvFile); envFile != "" {
		args = append(args, "--env", envFile)
	}
	if output := strings.TrimSpace(c.cfg.Output); output != "" {
		args = append(args, "--output", output)
	}
	runCtx := ctx
	cancel := func() {}
	if c.cfg.TimeoutSecs > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(c.cfg.TimeoutSecs)*time.Second)
	}
	defer cancel()
	result, runErr := c.rt.Exec.Run(runCtx, LocalCommandRequest{
		Name:                   blank(strings.TrimSpace(c.cfg.CLIPath), defaultCLIPath),
		Args:                   args,
		Dir:                    strings.TrimSpace(c.cfg.Root),
		MaxCapturedOutputBytes: flueCapturedOutputLimitBytes,
		CancelGracePeriod:      5 * time.Second,
	})
	if runErr != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) || errors.Is(runErr, context.DeadlineExceeded) {
			return flueRunResult{Raw: result}, context.DeadlineExceeded
		}
		if errors.Is(runCtx.Err(), context.Canceled) || errors.Is(runErr, context.Canceled) {
			return flueRunResult{Raw: result}, context.Canceled
		}
		return flueRunResult{Raw: result}, exit(flueProcessExitCode(result.ExitCode), "flue workflow failed: %s", flueFailureDetail(result, runErr, redactions))
	}
	resp, err := ParseResponseFromStdout(result.Stdout)
	if err != nil {
		return flueRunResult{Raw: result}, err
	}
	return flueRunResult{Response: resp, Raw: result}, nil
}

func workflowSelector(workflow string) string {
	workflow = strings.TrimSpace(workflow)
	if strings.HasPrefix(workflow, "workflow:") {
		return workflow
	}
	return "workflow:" + workflow
}

func flueProcessExitCode(code int) int {
	if code != 0 {
		return code
	}
	return 1
}

func flueFailureDetail(result LocalCommandResult, err error, redactions []string) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" && err != nil {
		detail = err.Error()
	}
	if detail == "" {
		detail = "unknown failure"
	}
	return redactFlueDetail(tailString(detail, 4096), redactions)
}

func redactFlueDetail(value string, redactions []string) string {
	out := value
	for _, secret := range redactions {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, "[REDACTED]")
	}
	return out
}

func tailString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}

func cleanHostPath(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	if abs, err := filepath.Abs(value); err == nil {
		return abs
	}
	return value
}
