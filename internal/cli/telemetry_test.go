package cli

import (
	"strings"
	"testing"
	"time"
)

func TestParseLeaseTelemetry(t *testing.T) {
	telemetry := parseLeaseTelemetry(strings.Join([]string{
		"cpuCount=16",
		"load1=0.12",
		"load5=0.34",
		"load15=0.56",
		"memoryUsedBytes=1073741824",
		"memoryTotalBytes=2147483648",
		"memoryPercent=50.00",
		"diskUsedBytes=3221225472",
		"diskTotalBytes=10737418240",
		"diskPercent=30.00",
		"uptimeSeconds=3661",
	}, "\n"), time.Date(2026, 5, 5, 1, 2, 3, 0, time.UTC), "test")
	if telemetry == nil {
		t.Fatal("telemetry nil")
	}
	if telemetry.CapturedAt != "2026-05-05T01:02:03Z" || telemetry.Source != "test" {
		t.Fatalf("metadata=%#v", telemetry)
	}
	if telemetry.Load1 == nil || *telemetry.Load1 != 0.12 {
		t.Fatalf("load1=%v", telemetry.Load1)
	}
	if telemetry.CPUCount == nil || *telemetry.CPUCount != 16 {
		t.Fatalf("cpuCount=%v", telemetry.CPUCount)
	}
	if telemetry.MemoryUsedBytes == nil || *telemetry.MemoryUsedBytes != 1073741824 {
		t.Fatalf("memoryUsedBytes=%v", telemetry.MemoryUsedBytes)
	}
	if telemetry.DiskPercent == nil || *telemetry.DiskPercent != 30 {
		t.Fatalf("diskPercent=%v", telemetry.DiskPercent)
	}
	if telemetry.UptimeSeconds == nil || *telemetry.UptimeSeconds != 3661 {
		t.Fatalf("uptimeSeconds=%v", telemetry.UptimeSeconds)
	}
}

func TestParseLeaseTelemetrySkipsEmptyMetrics(t *testing.T) {
	if telemetry := parseLeaseTelemetry("noise\nload1=-1\n", time.Now(), "test"); telemetry != nil {
		t.Fatalf("telemetry=%#v, want nil", telemetry)
	}
}
