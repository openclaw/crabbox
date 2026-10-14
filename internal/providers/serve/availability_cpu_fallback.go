//go:build !darwin || !cgo

package serve

import (
	"context"
	"time"
)

func (h *hostSampler) darwinCPU(ctx context.Context) *float64 {
	started := time.Now()
	// Host totals do not need per-process memory maps or shared-library scans.
	if out, ok := signalCommand(ctx, h.exec, "top", "-l", "2", "-s", "1", "-n", "0", "-R", "-F"); ok {
		cpu := parseHostCPU(out)
		if cpu == nil {
			debugAvailability(h.debug, "host_cpu_parse", "invalid_output", started)
		}
		return cpu
	}
	return nil
}
