//go:build !darwin || !cgo

package serve

import "context"

func (h *hostSampler) darwinCPU(ctx context.Context) *float64 {
	if out, ok := signalCommand(ctx, h.exec, "top", "-l", "2", "-s", "1", "-n", "0"); ok {
		return parseHostCPU(out)
	}
	return nil
}
