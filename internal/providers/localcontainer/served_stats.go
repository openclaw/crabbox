package localcontainer

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// CPUCores reports runtime CPU usage in host-core units: 100% equals one core.
// The caller supplies only its owned running containers, never a runtime-wide total.
func (r *ServedRuntime) CPUCores(ctx context.Context, containers []ServedContainer) (float64, error) {
	if len(containers) == 0 {
		return 0, nil
	}
	args := []string{"stats", "--no-stream", "--no-trunc", "--format", "{{.ID}} {{.CPUPerc}}"}
	ids := map[string]bool{}
	for _, c := range containers {
		ids[c.ID] = true
		args = append(args, c.ID)
	}
	result, err := r.b.docker(ctx, args, nil, nil)
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !ids[fields[0]] || !strings.HasSuffix(fields[1], "%") {
			return 0, fmt.Errorf("invalid owned-container CPU stats")
		}
		value, err := strconv.ParseFloat(strings.TrimSuffix(fields[1], "%"), 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("invalid owned-container CPU usage")
		}
		total += value / 100
		delete(ids, fields[0])
	}
	if len(ids) > 0 {
		return 0, fmt.Errorf("missing owned-container CPU stats")
	}
	return total, nil
}
