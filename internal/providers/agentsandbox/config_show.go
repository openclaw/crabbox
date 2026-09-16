package agentsandbox

import (
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ConfigShowSection describes loaded values without consulting Kubernetes.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.AgentSandbox
	return core.ProviderConfigShowSection{
		JSONKey: "agentSandbox", TextLabel: "agent_sandbox", Providers: []string{"agent-sandbox"},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "kubectl", JSONValue: c.Kubectl, TextName: "kubectl", TextValue: core.Blank(c.Kubectl, "-")},
			{JSONName: "kubeconfig", JSONValue: c.Kubeconfig, TextName: "kubeconfig", TextValue: core.Blank(c.Kubeconfig, "-")},
			{JSONName: "context", JSONValue: c.Context, TextName: "context", TextValue: core.Blank(c.Context, "-")},
			{JSONName: "namespace", JSONValue: c.Namespace, TextName: "namespace", TextValue: core.Blank(c.Namespace, "-")},
			{JSONName: "warmPool", JSONValue: c.WarmPool, TextName: "warm_pool", TextValue: core.Blank(c.WarmPool, "-")},
			{JSONName: "container", JSONValue: c.Container, TextName: "container", TextValue: core.Blank(c.Container, "-")},
			{JSONName: "workdir", JSONValue: c.Workdir, TextName: "workdir", TextValue: core.Blank(c.Workdir, "-")},
			{JSONName: "sandboxReadyTimeout", JSONValue: c.SandboxReadyTimeout.String(), TextName: "sandbox_ready_timeout", TextValue: c.SandboxReadyTimeout.String()},
			{JSONName: "podReadyTimeout", JSONValue: c.PodReadyTimeout.String(), TextName: "pod_ready_timeout", TextValue: c.PodReadyTimeout.String()},
			{JSONName: "execTimeoutSecs", JSONValue: c.ExecTimeoutSecs, TextName: "exec_timeout_secs", TextValue: strconv.Itoa(c.ExecTimeoutSecs)},
			{JSONName: "deleteOnRelease", JSONValue: c.DeleteOnRelease, TextName: "delete_on_release", TextValue: strconv.FormatBool(c.DeleteOnRelease)},
			{JSONName: "forgetMissing", JSONValue: c.ForgetMissing, TextName: "forget_missing", TextValue: strconv.FormatBool(c.ForgetMissing)},
		},
	}
}
