package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestCreationTimingProjectionKeepsSourcesAndLegacyDurations(t *testing.T) {
	var lease CoordinatorLease
	if err := json.Unmarshal([]byte(`{"creationEvents":[{"phase":"admission_started","at":"2026-01-01T00:00:00Z","source":"coordinator"}],"provisioningTiming":{"requestMs":250,"totalMs":250}}`), &lease); err != nil {
		t.Fatal(err)
	}
	timing := coordinatorRunnerTiming(lease)
	timing.Events = lease.CreationEvents
	report := timingReportFromRun("aws", "cbx_test", "test", runTimings{lease: time.Second, providerTiming: timing}, time.Second, 0)
	if report.LeaseMs != 1000 || len(report.CreationEvents) != 1 || report.CreationEvents[0].Source != "coordinator" {
		t.Fatalf("projection: %+v", report)
	}
	timing.Events[0].Source = "changed"
	if report.CreationEvents[0].Source != "coordinator" {
		t.Fatal("report aliases mutable observations")
	}
	old := timingReportFromRun("aws", "cbx_test", "test", runTimings{}, time.Second, 0)
	if len(old.CreationEvents) != 0 {
		t.Fatal("invented legacy observations")
	}
}

func TestCreationObservationsRetainFirstSuccess(t *testing.T) {
	observations := &creationObservations{}
	ctx := context.WithValue(t.Context(), creationObservationsKey{}, observations)
	recordCreationObservation(ctx, "ssh_tcp_accept")
	first := observations.events[0]
	recordCreationObservation(ctx, "ssh_tcp_accept")
	recordCreationObservation(ctx, "ssh_authenticated")
	if len(observations.events) != 2 || observations.events[0] != first {
		t.Fatalf("events: %+v", observations.events)
	}
	recordCreationObservation(t.Context(), "workspace_ready")
}

func TestCreationStepTimingJSONPreservesZeroDurationAndCounts(t *testing.T) {
	var lease CoordinatorLease
	if err := json.Unmarshal([]byte(`{"creationEvents":[{"phase":"coordinator_step","at":"2026-01-01T00:00:00Z","source":"coordinator","step":"aws.image","durationMs":0,"count":2,"errors":0}],"provisioningTiming":{"requestMs":250,"totalMs":250}}`), &lease); err != nil {
		t.Fatal(err)
	}
	timing := coordinatorRunnerTiming(lease)
	timing.Events = lease.CreationEvents
	report := timingReportFromRun("aws", "cbx_test", "test", runTimings{providerTiming: timing}, time.Second, 0)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		CreationEvents []map[string]any `json:"creationEvents"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	event := decoded.CreationEvents[0]
	if event["step"] != "aws.image" || event["durationMs"] != float64(0) || event["count"] != float64(2) || event["errors"] != float64(0) {
		t.Fatalf("step timing lost from timing JSON: %s", encoded)
	}
}
