package localcontainer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const memoryDiagnosticPrefix = "diagnostic.memory."

const memoryObservationBudget = 500 * time.Millisecond

// The existing bounded command runner allows 250ms for pipe draining on cancel.
const memoryObservationDrainReserve = 250 * time.Millisecond
const memoryObservationOutputLimit = 4096

type containerObservationCommand func(context.Context, []string) (core.LocalCommandResult, error)

// The collector keeps the same command route and environment as its baseline,
// even if config or the ambient default runtime changes during the workload.
type memoryRuntime struct {
	runner  core.CommandRunner
	request core.LocalCommandRequest
	scope   checkpointScope
	scoped  bool
}

func (b *backend) memoryRuntime() memoryRuntime {
	cfg := b.configForRun()
	req := containerRuntimeRequest(cfg, nil)
	if req.Env == nil {
		req.Env = os.Environ()
	}
	return memoryRuntime{
		runner: b.rt.Exec, request: req,
		scope:  checkpointScopeFromMetadata(cfg.LocalContainer.CheckpointMetadata, cfg.LocalContainer.Runtime),
		scoped: hasCompleteCapturedRuntimeScope(cfg.LocalContainer.CheckpointMetadata),
	}
}

func (r memoryRuntime) run(ctx context.Context, args []string) (core.LocalCommandResult, error) {
	req := r.request
	req.Args = append(append([]string(nil), req.Args...), args...)
	return r.runner.Run(ctx, req)
}

func (r memoryRuntime) memoryFailureEvidence(ctx context.Context, id string, container inspectContainer) core.RunFailureEvidence {
	details := r.memoryDetails(ctx, id, container, "before-command")
	return core.RunFailureEvidence{
		ResourceExhaustion: core.ResourceExhaustionMemory,
		Hint:               memoryHint(details), Details: details,
	}
}

func (r memoryRuntime) memoryDetails(ctx context.Context, id string, container inspectContainer, phase string) map[string]string {
	details := map[string]string{"settings_phase": phase, "capacity_status": "unknown"}
	// Never attribute settings returned for a different container to this command.
	var settings struct {
		Memory     json.RawMessage
		MemorySwap json.RawMessage
	}
	if container.ID == id {
		_ = json.Unmarshal(container.HostConfig, &settings)
	}
	if len(id) <= 128 && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) < 0 {
		details["container_id"] = id
	}
	addMemorySetting(details, "container_memory_limit", settings.Memory, false)
	addMemorySetting(details, "container_memory_swap", settings.MemorySwap, true)
	// No capacity work for unbound legacy routes, and no fallback to another engine.
	if !r.scoped || r.scope.Runtime != r.request.Name {
		return details
	}
	budget := memoryObservationBudget
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline))
	}
	if ctx.Err() != nil || budget <= memoryObservationDrainReserve {
		return details
	}
	bounded, cancel := context.WithTimeout(ctx, budget-memoryObservationDrainReserve)
	defer cancel()
	r.request.MaxCapturedOutputBytes = memoryObservationOutputLimit
	if !r.memoryRouteUnchanged(bounded) {
		return details
	}
	format := "{{.ID}}\n{{.MemTotal}}"
	if isPodmanRuntime(r.scope.Runtime) {
		format = "{{.Host.Hostname}}|{{.Store.GraphRoot}}|{{.Store.RunRoot}}|{{.Host.RemoteSocket.Path}}|{{.Host.Security.Rootless}}\n{{.Host.MemTotal}}"
	}
	result, err := r.run(bounded, []string{"info", "--format", format})
	if err != nil || bounded.Err() != nil || len(result.Stdout) > memoryObservationOutputLimit {
		return details
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 2 {
		return details
	}
	identity := strings.TrimSpace(lines[0])
	if isPodmanRuntime(r.scope.Runtime) {
		sum := sha256.Sum256([]byte(identity))
		identity = fmt.Sprintf("podman-%x", sum[:16])
	}
	ram, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err != nil || ram <= 0 || identity != r.scope.DaemonID {
		return details
	}
	details["capacity_status"] = "observed"
	details["runtime_memory_total_bytes"] = strconv.FormatInt(ram, 10)
	return details
}

func (r memoryRuntime) memoryRouteUnchanged(ctx context.Context) bool {
	if r.scope.Context == "" || r.scope.Context == "default" || isPodmanRuntime(r.scope.Runtime) && r.scope.Host != "" {
		return true // Explicit endpoints are frozen in the command environment.
	}
	if isDockerRuntime(r.scope.Runtime) {
		result, err := r.run(ctx, []string{"context", "inspect", r.scope.Context, "--format", `{{(index .Endpoints "docker").Host}}`})
		return err == nil && len(result.Stdout) <= memoryObservationOutputLimit && strings.TrimSpace(result.Stdout) == r.scope.Endpoint
	}
	if isPodmanRuntime(r.scope.Runtime) {
		result, err := r.run(ctx, []string{"system", "connection", "list", "--format", "json"})
		if err != nil || len(result.Stdout) > memoryObservationOutputLimit {
			return false
		}
		var connections []struct{ Name, URI string }
		if json.Unmarshal([]byte(result.Stdout), &connections) != nil {
			return false
		}
		for _, connection := range connections {
			if connection.Name == r.scope.Context {
				return connection.URI == r.scope.Endpoint
			}
		}
	}
	return false
}

func addMemorySetting(details map[string]string, key string, raw json.RawMessage, swap bool) {
	kind := "unknown"
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err == nil {
		switch {
		case value > 0:
			kind = "finite"
			details[key+"_bytes"] = strconv.FormatInt(value, 10)
		case value == 0 && swap:
			kind = "default"
		case value == 0 || value == -1 && swap:
			kind = "unlimited"
		}
	}
	details[key] = kind
}

func memoryHint(details map[string]string) string {
	caveat := " Runtime RAM is total, not free memory or an effective bound; parent cgroups and swap availability/settings may also constrain the workload. Reduce memory demand."
	limit, _ := strconv.ParseInt(details["container_memory_limit_bytes"], 10, 64)
	ram, _ := strconv.ParseInt(details["runtime_memory_total_bytes"], 10, 64)
	switch {
	case limit > 0 && ram > 0 && limit >= ram:
		return "The actual container limit is at or above observed runtime RAM; raising --local-container-memory does not add RAM. Check runtime/VM capacity." + caveat
	case limit > 0 && ram > limit:
		return "The actual container limit is below observed runtime RAM. If it is binding and there is headroom, recreate the lease with more --local-container-memory; retrying the same --id does not resize it." + caveat
	case details["container_memory_limit"] == "unlimited":
		return "No finite container memory limit was reported. Check active runtime/VM limits." + caveat
	default:
		return "Check the actual container limit and active runtime/VM capacity before changing --local-container-memory; retrying the same --id does not resize it." + caveat
	}
}
