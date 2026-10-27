package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var availabilityDebugMu sync.Mutex

// Only fixed signal names, outcomes and timings are logged. Command arguments,
// output and error text can contain private runtime data and are never logged.
func debugAvailability(out io.Writer, signal, outcome string, started time.Time) {
	if out != nil && os.Getenv("CRABBOX_SERVE_AVAILABILITY_DEBUG") == "1" {
		availabilityDebugMu.Lock()
		defer availabilityDebugMu.Unlock()
		fmt.Fprintf(out, "serve availability: signal=%s outcome=%s elapsed=%s\n", signal, outcome, time.Since(started).Round(time.Millisecond))
	}
}

func signalOutcome(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "canceled"
	}
	if err != nil {
		return "error"
	}
	return "ok"
}

type availabilityCommandRunner struct {
	core.CommandRunner
	out io.Writer
}

func (r availabilityCommandRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	started := time.Now()
	result, err := r.CommandRunner.Run(ctx, req)
	signal := "runtime"
	switch req.Name {
	case "top":
		signal = "host_cpu_command"
	case "ioreg":
		signal = "user_idle_command"
	case "pmset":
		signal = "power_command"
	}
	outcome := signalOutcome(ctx, err)
	if outcome == "ok" && result.ExitCode != 0 {
		outcome = "exit_nonzero"
	}
	debugAvailability(r.out, signal, outcome, started)
	return result, err
}

func availabilityRunner(rt core.Runtime) core.CommandRunner {
	if rt.Exec == nil || os.Getenv("CRABBOX_SERVE_AVAILABILITY_DEBUG") != "1" {
		return rt.Exec
	}
	return availabilityCommandRunner{CommandRunner: rt.Exec, out: rt.Stderr}
}
