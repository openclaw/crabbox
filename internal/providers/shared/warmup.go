package shared

import (
	"fmt"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type WarmupCompletion struct {
	Provider string
	LeaseID  string
	Slug     string
	Workdir  string
	Total    time.Duration
}

func CompleteWarmup(rt core.Runtime, timingJSON bool, result WarmupCompletion) error {
	fmt.Fprintf(rt.Stdout, "warmup complete total=%s\n", result.Total.Round(time.Millisecond))
	if timingJSON {
		return core.WriteTimingJSON(rt.Stderr, core.TimingReport{
			Provider: result.Provider,
			LeaseID:  result.LeaseID,
			Slug:     result.Slug,
			TotalMs:  result.Total.Milliseconds(),
			ExitCode: 0,
			Workdir:  result.Workdir,
		})
	}
	return nil
}
