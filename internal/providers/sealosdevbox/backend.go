package sealosdevbox

import (
	"context"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	checks := []core.DoctorCheck{}
	add := func(check core.DoctorCheck) {
		checks = append(checks, check)
	}
	if _, err := b.kubectl(ctx, nil, false, "version", "--client=true", "-o", "json"); err != nil {
		add(doctorCheck("failed", "kubectl", err.Error(), nil))
		return doctorResult(checks), nil
	}
	add(doctorCheck("ok", "kubectl", "client=ready", nil))
	if _, err := b.kubectl(ctx, nil, false, "config", "get-contexts", b.cfg.SealosDevbox.Context, "-o", "name"); err != nil {
		add(doctorCheck("failed", "context", err.Error(), map[string]string{"context": b.cfg.SealosDevbox.Context}))
		return doctorResult(checks), nil
	}
	add(doctorCheck("ok", "context", "found", map[string]string{"context": b.cfg.SealosDevbox.Context}))
	if _, err := b.kubectl(ctx, nil, false, "get", "namespace", b.cfg.SealosDevbox.Namespace, "-o", "name"); err != nil {
		add(doctorCheck("failed", "namespace", err.Error(), map[string]string{"namespace": b.cfg.SealosDevbox.Namespace}))
		return doctorResult(checks), nil
	}
	add(doctorCheck("ok", "namespace", "found", map[string]string{"namespace": b.cfg.SealosDevbox.Namespace}))
	if _, err := b.kubectl(ctx, nil, false, "get", "customresourcedefinition", devboxCRD, "-o", "jsonpath={.spec.versions[*].name}"); err != nil {
		add(doctorCheck("failed", "crd.devboxes", err.Error(), map[string]string{"groupVersion": devboxGroupVersion}))
		return doctorResult(checks), nil
	}
	add(doctorCheck("ok", "crd.devboxes", "found", map[string]string{"groupVersion": devboxGroupVersion}))
	for _, resource := range []string{devboxResource, "secrets", "pods", "events"} {
		for _, verb := range []string{"get", "list"} {
			add(b.canI(ctx, verb, resource))
		}
	}
	for _, verb := range []string{"create", "update", "delete"} {
		add(b.canI(ctx, verb, devboxResource))
	}
	networkDetails := map[string]string{"network": normalizeNetwork(b.cfg.SealosDevbox.Network)}
	switch normalizeNetwork(b.cfg.SealosDevbox.Network) {
	case networkSSHGate:
		networkDetails["host_configured"] = boolString(b.cfg.SealosDevbox.SSHGatewayHost != "")
		networkDetails["port_configured"] = boolString(b.cfg.SealosDevbox.SSHGatewayPort != "")
		add(doctorCheck("ok", "network", "SSHGate configured", networkDetails))
	case networkNodePort:
		networkDetails["node_host_configured"] = boolString(b.cfg.SealosDevbox.NodeHost != "")
		add(doctorCheck("ok", "network", "NodePort configured", networkDetails))
	default:
		add(doctorCheck("failed", "network", "network must be SSHGate or NodePort", networkDetails))
	}
	add(doctorCheck("ok", "automation_surface", AutomationSurfaceDecision, map[string]string{"surface": AutomationSurfaceDecision}))
	return doctorResult(checks), nil
}

func doctorResult(checks []core.DoctorCheck) core.DoctorResult {
	status := "ready"
	for _, check := range checks {
		if check.Status == "failed" || check.Status == "missing" {
			status = "blocked"
			break
		}
	}
	return core.DoctorResult{
		Provider: providerName,
		Status:   status,
		Message:  formatDoctorSummary(checks),
		Checks:   checks,
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func (b *backend) Acquire(context.Context, core.AcquireRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, unsupportedLifecycleError("acquire")
}

func (b *backend) Resolve(context.Context, core.ResolveRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, unsupportedLifecycleError("resolve")
}

func (b *backend) Touch(context.Context, core.TouchRequest) (core.Server, error) {
	return core.Server{}, unsupportedLifecycleError("touch")
}

func (b *backend) List(context.Context, core.ListRequest) ([]core.LeaseView, error) {
	return nil, unsupportedLifecycleError("list")
}

func (b *backend) ReleaseLease(context.Context, core.ReleaseLeaseRequest) error {
	return unsupportedLifecycleError("release")
}

func (b *backend) Cleanup(context.Context, core.CleanupRequest) error {
	return unsupportedLifecycleError("cleanup")
}
