package agentsandbox

import (
	"context"
	"fmt"
	"strings"
)

type backend struct {
	spec      ProviderSpec
	cfg       Config
	rt        Runtime
	newClient func(context.Context, Config, Runtime) (kubernetesClient, error)
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := b.client(ctx)
	if err != nil {
		return DoctorResult{}, err
	}
	checks, err := b.doctorChecks(ctx, client)
	result := DoctorResult{
		Provider: providerName,
		Status:   "ready",
		Checks:   checks,
		Message: fmt.Sprintf("kubernetes=ready crds=ready rbac=ready warm_pool=%s namespace=%s context=%s mutation=false",
			b.cfg.AgentSandbox.WarmPool, b.cfg.AgentSandbox.Namespace, b.cfg.AgentSandbox.Context),
	}
	if err != nil {
		result.Status = "blocked"
		result.Message = err.Error()
		return result, err
	}
	return result, nil
}

func (b *backend) Warmup(context.Context, WarmupRequest) error {
	return exit(2, "provider=%s warmup is implemented by PLAN-02; PLAN-01 only provides registration, config, doctor, and readiness foundation", providerName)
}

func (b *backend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{Provider: providerName, SyncDelegated: true}, exit(2, "provider=%s run is implemented by PLAN-02; PLAN-01 only provides registration, config, doctor, and readiness foundation", providerName)
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, exit(2, "provider=%s list is implemented by PLAN-02", providerName)
}

func (b *backend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, exit(2, "provider=%s status is implemented by PLAN-02", providerName)
}

func (b *backend) Stop(context.Context, StopRequest) error {
	return exit(2, "provider=%s stop is implemented by PLAN-02", providerName)
}

func (b *backend) Cleanup(context.Context, CleanupRequest) error {
	return exit(2, "provider=%s cleanup is implemented by PLAN-02", providerName)
}

func (b *backend) client(ctx context.Context) (kubernetesClient, error) {
	if b.newClient != nil {
		return b.newClient(ctx, b.cfg, b.rt)
	}
	return newKubernetesClient(ctx, b.cfg, b.rt)
}

func (b *backend) doctorChecks(ctx context.Context, client kubernetesClient) ([]DoctorCheck, error) {
	cfg := b.cfg.AgentSandbox
	checks := []DoctorCheck{}
	add := func(status, check, message string, details map[string]string) {
		checks = append(checks, DoctorCheck{Status: status, Check: check, Message: message, Details: details})
	}
	if err := client.CheckResource(ctx, agentSandboxCoreGroupVersion, sandboxResource); err != nil {
		add("blocked", "crd.sandboxes", err.Error(), nil)
		return checks, err
	}
	add("ok", "crd.sandboxes", "found", map[string]string{"groupVersion": agentSandboxCoreGroupVersion})
	for _, resource := range []string{sandboxClaimResource, warmPoolResource} {
		if err := client.CheckResource(ctx, agentSandboxExtensionsGroupVersion, resource); err != nil {
			add("blocked", "crd."+resource, err.Error(), nil)
			return checks, err
		}
		add("ok", "crd."+resource, "found", map[string]string{"groupVersion": agentSandboxExtensionsGroupVersion})
	}
	if _, err := client.Get(ctx, warmPoolGVR(), cfg.Namespace, cfg.WarmPool); err != nil {
		add("blocked", "warm_pool", err.Error(), map[string]string{"namespace": cfg.Namespace, "name": cfg.WarmPool})
		return checks, err
	}
	add("ok", "warm_pool", "found", map[string]string{"namespace": cfg.Namespace, "name": cfg.WarmPool})
	for _, rule := range doctorRBACRules(cfg.Namespace) {
		allowed, err := client.CanI(ctx, rule)
		if err != nil {
			add("blocked", "rbac."+rule.String(), err.Error(), nil)
			return checks, err
		}
		if !allowed {
			err := exit(5, "agent-sandbox RBAC denied: %s", rule.String())
			add("blocked", "rbac."+rule.String(), err.Error(), nil)
			return checks, err
		}
		add("ok", "rbac."+rule.String(), "allowed", nil)
	}
	return checks, nil
}

func doctorRBACRules(namespace string) []rbacRule {
	return []rbacRule{
		{Group: "extensions.agents.x-k8s.io", Resource: sandboxClaimResource, Namespace: namespace, Verbs: []string{"get", "list", "watch", "create", "delete"}},
		{Group: "extensions.agents.x-k8s.io", Resource: warmPoolResource, Namespace: namespace, Verbs: []string{"get"}},
		{Group: "agents.x-k8s.io", Resource: sandboxResource, Namespace: namespace, Verbs: []string{"get", "list", "watch"}},
		{Group: "", Resource: podResource, Namespace: namespace, Verbs: []string{"get", "list", "watch"}},
		{Group: "", Resource: "pods/exec", Namespace: namespace, Verbs: []string{"create"}},
	}
}

type rbacRule struct {
	Group     string
	Resource  string
	Namespace string
	Verbs     []string
}

func (r rbacRule) String() string {
	group := r.Group
	if group == "" {
		group = "core"
	}
	return strings.Join(r.Verbs, ",") + " " + group + "/" + r.Resource + " namespace=" + r.Namespace
}
