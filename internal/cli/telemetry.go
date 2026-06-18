package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const leaseTelemetryTimeout = 5 * time.Second

type LeaseTelemetry struct {
	CapturedAt       string   `json:"capturedAt,omitempty"`
	Source           string   `json:"source,omitempty"`
	Load1            *float64 `json:"load1,omitempty"`
	Load5            *float64 `json:"load5,omitempty"`
	Load15           *float64 `json:"load15,omitempty"`
	CPUCount         *int64   `json:"cpuCount,omitempty"`
	MemoryUsedBytes  *int64   `json:"memoryUsedBytes,omitempty"`
	MemoryTotalBytes *int64   `json:"memoryTotalBytes,omitempty"`
	MemoryPercent    *float64 `json:"memoryPercent,omitempty"`
	DiskUsedBytes    *int64   `json:"diskUsedBytes,omitempty"`
	DiskTotalBytes   *int64   `json:"diskTotalBytes,omitempty"`
	DiskPercent      *float64 `json:"diskPercent,omitempty"`
	UptimeSeconds    *int64   `json:"uptimeSeconds,omitempty"`
}

type RunTelemetrySummary struct {
	Start   *LeaseTelemetry   `json:"start,omitempty"`
	End     *LeaseTelemetry   `json:"end,omitempty"`
	Samples []*LeaseTelemetry `json:"samples,omitempty"`
}

type leaseTelemetryCollector func(context.Context) (*LeaseTelemetry, error)

func leaseTelemetryCollectorForTarget(target SSHTarget) leaseTelemetryCollector {
	return func(ctx context.Context) (*LeaseTelemetry, error) {
		return collectLeaseTelemetry(ctx, target)
	}
}

func collectLeaseTelemetry(ctx context.Context, target SSHTarget) (*LeaseTelemetry, error) {
	if target.Host == "" {
		return nil, nil
	}
	if target.TargetOS != "" && target.TargetOS != targetLinux {
		return nil, nil
	}
	output, err := runSSHOutput(ctx, target, remoteLeaseTelemetryScript())
	if err != nil {
		return nil, err
	}
	return parseLeaseTelemetry(output, time.Now().UTC(), "ssh-linux"), nil
}

func collectLeaseTelemetryBestEffort(ctx context.Context, collector leaseTelemetryCollector) *LeaseTelemetry {
	if collector == nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, leaseTelemetryTimeout)
	defer cancel()
	telemetry, err := collector(callCtx)
	if err != nil {
		return nil
	}
	return telemetry
}

func remoteLeaseTelemetryScript() string {
	return `set +e
getconf _NPROCESSORS_ONLN 2>/dev/null | awk '$1 ~ /^[0-9]+$/ && $1 > 0 {print "cpuCount="$1; exit}'
if [ -r /proc/loadavg ]; then awk '{print "load1="$1; print "load5="$2; print "load15="$3}' /proc/loadavg; fi
if [ -r /proc/meminfo ]; then awk '
  /^MemTotal:/ { total=$2*1024 }
  /^MemAvailable:/ { available=$2*1024 }
  END {
    if (total > 0) {
      used=total-available
      if (used < 0) used=0
      printf "memoryTotalBytes=%.0f\n", total
      printf "memoryUsedBytes=%.0f\n", used
      printf "memoryPercent=%.2f\n", used*100/total
    }
  }' /proc/meminfo; fi
df -PB1 / 2>/dev/null | awk 'NR==2 { print "diskTotalBytes="$2; print "diskUsedBytes="$3; if ($2 > 0) printf "diskPercent=%.2f\n", $3*100/$2 }'
if [ -r /proc/uptime ]; then awk '{printf "uptimeSeconds=%.0f\n", $1}' /proc/uptime; fi`
}

func parseLeaseTelemetry(output string, capturedAt time.Time, source string) *LeaseTelemetry {
	telemetry := LeaseTelemetry{
		CapturedAt: capturedAt.Format(time.RFC3339),
		Source:     source,
	}
	hasMetric := false
	for _, line := range strings.Split(output, "\n") {
		key, raw, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" || raw == "" {
			continue
		}
		switch key {
		case "load1":
			hasMetric = setFloat(&telemetry.Load1, raw) || hasMetric
		case "load5":
			hasMetric = setFloat(&telemetry.Load5, raw) || hasMetric
		case "load15":
			hasMetric = setFloat(&telemetry.Load15, raw) || hasMetric
		case "cpuCount":
			hasMetric = setInt64(&telemetry.CPUCount, raw) || hasMetric
		case "memoryUsedBytes":
			hasMetric = setInt64(&telemetry.MemoryUsedBytes, raw) || hasMetric
		case "memoryTotalBytes":
			hasMetric = setInt64(&telemetry.MemoryTotalBytes, raw) || hasMetric
		case "memoryPercent":
			hasMetric = setFloat(&telemetry.MemoryPercent, raw) || hasMetric
		case "diskUsedBytes":
			hasMetric = setInt64(&telemetry.DiskUsedBytes, raw) || hasMetric
		case "diskTotalBytes":
			hasMetric = setInt64(&telemetry.DiskTotalBytes, raw) || hasMetric
		case "diskPercent":
			hasMetric = setFloat(&telemetry.DiskPercent, raw) || hasMetric
		case "uptimeSeconds":
			hasMetric = setInt64(&telemetry.UptimeSeconds, raw) || hasMetric
		}
	}
	if !hasMetric {
		return nil
	}
	return &telemetry
}

func setFloat(dst **float64, raw string) bool {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || value < 0 {
		return false
	}
	*dst = &value
	return true
}

func setInt64(dst **int64, raw string) bool {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || value < 0 {
		return false
	}
	rounded := int64(value)
	*dst = &rounded
	return true
}

func leaseTelemetryStatusSummary(telemetry *LeaseTelemetry) string {
	if telemetry == nil {
		return ""
	}
	parts := []string{}
	if telemetry.Load1 != nil {
		parts = append(parts, fmt.Sprintf("load=%.2f", *telemetry.Load1))
	}
	if telemetry.MemoryUsedBytes != nil && telemetry.MemoryTotalBytes != nil {
		parts = append(parts, fmt.Sprintf("mem=%s/%s", formatBytesCompact(*telemetry.MemoryUsedBytes), formatBytesCompact(*telemetry.MemoryTotalBytes)))
	} else if telemetry.MemoryPercent != nil {
		parts = append(parts, fmt.Sprintf("mem=%.0f%%", *telemetry.MemoryPercent))
	}
	if telemetry.DiskUsedBytes != nil && telemetry.DiskTotalBytes != nil {
		parts = append(parts, fmt.Sprintf("disk=%s/%s", formatBytesCompact(*telemetry.DiskUsedBytes), formatBytesCompact(*telemetry.DiskTotalBytes)))
	} else if telemetry.DiskPercent != nil {
		parts = append(parts, fmt.Sprintf("disk=%.0f%%", *telemetry.DiskPercent))
	}
	if telemetry.UptimeSeconds != nil {
		parts = append(parts, fmt.Sprintf("uptime=%s", formatSecondsDuration(int(*telemetry.UptimeSeconds))))
	}
	if telemetry.CapturedAt != "" {
		parts = append(parts, "telemetry="+blank(idleForString(telemetry.CapturedAt, time.Now()), "now"))
	}
	return strings.Join(parts, " ")
}

func runTelemetrySummary(start, end *LeaseTelemetry, samples []*LeaseTelemetry) *RunTelemetrySummary {
	if start == nil && end == nil && len(samples) == 0 {
		return nil
	}
	return &RunTelemetrySummary{Start: start, End: end, Samples: samples}
}

func runTelemetryStatusSummary(telemetry *RunTelemetrySummary) string {
	if telemetry == nil {
		return ""
	}
	current := telemetry.End
	if current == nil {
		current = latestTelemetrySample(telemetry.Samples)
	}
	if current == nil {
		current = telemetry.Start
	}
	if current == nil {
		return ""
	}
	parts := []string{}
	if current.Load1 != nil {
		parts = append(parts, fmt.Sprintf("load=%.2f", *current.Load1))
	}
	if current.MemoryPercent != nil {
		parts = append(parts, fmt.Sprintf("mem=%.0f%%", *current.MemoryPercent))
	} else if current.MemoryUsedBytes != nil && current.MemoryTotalBytes != nil {
		parts = append(parts, fmt.Sprintf("mem=%s/%s", formatBytesCompact(*current.MemoryUsedBytes), formatBytesCompact(*current.MemoryTotalBytes)))
	}
	if current.DiskPercent != nil {
		parts = append(parts, fmt.Sprintf("disk=%.0f%%", *current.DiskPercent))
	}
	if delta := telemetryDeltaBytes(telemetry.Start, telemetry.End, "memory"); delta != "" {
		parts = append(parts, "mem_delta="+delta)
	}
	return strings.Join(parts, " ")
}

func latestTelemetrySample(samples []*LeaseTelemetry) *LeaseTelemetry {
	if len(samples) == 0 {
		return nil
	}
	return samples[len(samples)-1]
}

func telemetryDeltaBytes(start, end *LeaseTelemetry, metric string) string {
	if start == nil || end == nil {
		return ""
	}
	var left, right *int64
	switch metric {
	case "memory":
		left, right = start.MemoryUsedBytes, end.MemoryUsedBytes
	case "disk":
		left, right = start.DiskUsedBytes, end.DiskUsedBytes
	default:
		return ""
	}
	if left == nil || right == nil {
		return ""
	}
	delta := *right - *left
	if delta == 0 {
		return "0B"
	}
	prefix := "+"
	if delta < 0 {
		prefix = "-"
		delta = -delta
	}
	return prefix + formatBytesCompact(delta)
}

func formatBytesCompact(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%dB", value)
	}
	f := float64(value)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		f /= unit
		if f < unit {
			return fmt.Sprintf("%.1f%s", f, suffix)
		}
	}
	return fmt.Sprintf("%.1fPiB", f/unit)
}
