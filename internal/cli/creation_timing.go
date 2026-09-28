package cli

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CreationEvent is an observation, never authority for readiness or ownership.
// At belongs to Source's clock; events must not be subtracted across clocks.
type CreationEvent struct {
	Phase  string `json:"phase"`
	At     string `json:"at"`
	Source string `json:"source"`
}

type creationObservations struct{ events []CreationEvent }
type creationObservationsKey struct{}

func recordCreationObservation(ctx context.Context, phase string) {
	if observations, ok := ctx.Value(creationObservationsKey{}).(*creationObservations); ok {
		for _, event := range observations.events {
			if event.Phase == phase {
				return
			}
		}
		observations.events = append(observations.events, CreationEvent{phase, time.Now().UTC().Format(time.RFC3339Nano), "client"})
	}
}

func (c *CoordinatorClient) recordCreationEvents(ctx context.Context, leaseID, provider string, events []CreationEvent) {
	// Older coordinators ignore the additive heartbeat field. Telemetry failure
	// must not turn a usable lease into a failed acquisition.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = c.do(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(leaseID)+"/heartbeat", map[string]any{
		"expectedProvider": provider, "creationEvents": events,
	}, nil)
}

func readBootstrapComplete(ctx context.Context, target SSHTarget) *CreationEvent {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// /run is current-boot state; the persistent bootstrapped marker can be
	// inherited from a prepared image and is not a boot-completion timestamp.
	out, err := runSSHCombinedOutputLimit(ctx, target, "stat -c %Y /run/crabbox/workspace-ready 2>/dev/null", 64)
	if err != nil {
		return nil
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || seconds <= 0 || seconds > time.Now().Add(time.Minute).Unix() {
		return nil
	}
	return &CreationEvent{"bootstrap_complete", time.Unix(seconds, 0).UTC().Format(time.RFC3339), "guest"}
}

func creationTimingEvents(timing *runnerProviderTiming) []CreationEvent {
	if timing == nil {
		return nil
	}
	return append([]CreationEvent(nil), timing.Events...)
}
