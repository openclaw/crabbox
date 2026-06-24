package nomad

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	nomadapi "github.com/hashicorp/nomad/api"
)

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	checks := []DoctorCheck{}
	add := func(status, check, class, message string, details map[string]string) {
		if details == nil {
			details = map[string]string{}
		}
		details["provider"] = providerName
		details["class"] = class
		details["mutation"] = "false"
		checks = append(checks, DoctorCheck{Status: status, Check: check, Message: message, Details: details})
	}
	finishFailed := func() DoctorResult {
		return DoctorResult{Provider: providerName, Status: "failed", Message: "readiness=failed mutation=false", Checks: checks}
	}

	if strings.TrimSpace(b.cfg.Nomad.Address) == "" {
		add("failed", "config", "missing_address", "provider=nomad class=missing_address hint=set NOMAD_ADDR or nomad.address mutation=false", map[string]string{"address": "missing", "hint": "set_NOMAD_ADDR_or_nomad.address"})
		return finishFailed(), nil
	}
	token, envName := nomadToken(b.cfg, os.Getenv)
	if token == "" {
		add("failed", "auth", "missing_token", fmt.Sprintf("provider=nomad class=missing_token token_env=%s hint=set %s mutation=false", envName, envName), map[string]string{"token_env": envName, "auth": "missing", "hint": "set_" + envName})
		return finishFailed(), nil
	}
	client, err := b.clientFactory(b.cfg, b.rt)
	if err != nil {
		class := classifyError(err)
		add("failed", "client", class, fmt.Sprintf("provider=nomad class=%s mutation=false %v", class, err), map[string]string{"error": err.Error()})
		return finishFailed(), nil
	}
	if _, err := client.AgentSelf(ctx); err != nil {
		class := classifyError(err)
		add("failed", "api", class, fmt.Sprintf("provider=nomad class=%s api=agent.self mutation=false %v", class, err), map[string]string{"api": "agent.self", "error": err.Error()})
		return finishFailed(), nil
	}
	add("ok", "api", "ready", "provider=nomad api=agent.self mutation=false", map[string]string{"api": "agent.self"})

	region := strings.TrimSpace(b.cfg.Nomad.Region)
	if region != "" {
		regions, err := client.Regions(ctx)
		if err != nil {
			class := classifyError(err)
			add("failed", "region", class, fmt.Sprintf("provider=nomad class=%s api=regions.list mutation=false %v", class, err), map[string]string{"api": "regions.list", "region": region, "error": err.Error()})
			return finishFailed(), nil
		}
		if !containsString(regions, region) {
			add("failed", "region", "missing_region", fmt.Sprintf("provider=nomad region=%s class=missing_region mutation=false", region), map[string]string{"region": region, "regions": strings.Join(regions, ",")})
			return finishFailed(), nil
		}
		add("ok", "region", "ready", fmt.Sprintf("provider=nomad region=%s mutation=false", region), map[string]string{"region": region})
	} else {
		add("ok", "region", "default", "provider=nomad region=default mutation=false", map[string]string{"region": "default"})
	}

	namespace := strings.TrimSpace(b.cfg.Nomad.Namespace)
	if namespace != "" {
		if _, err := client.NamespaceInfo(ctx, namespace); err != nil {
			class := classifyError(err)
			add("failed", "namespace", class, fmt.Sprintf("provider=nomad namespace=%s class=%s api=namespace.info mutation=false %v", namespace, class, err), map[string]string{"api": "namespace.info", "namespace": namespace, "error": err.Error()})
			return finishFailed(), nil
		}
		add("ok", "namespace", "ready", fmt.Sprintf("provider=nomad namespace=%s mutation=false", namespace), map[string]string{"namespace": namespace})
	} else {
		add("ok", "namespace", "default", "provider=nomad namespace=default mutation=false", map[string]string{"namespace": "default"})
	}

	return DoctorResult{Provider: providerName, Status: "ok", Message: "auth=ready control_plane=ready api=read mutation=false runtime=unchecked", Checks: checks}, nil
}

func classifyError(err error) string {
	if err == nil {
		return "ready"
	}
	var unexpected nomadapi.UnexpectedResponseError
	if errors.As(err, &unexpected) && unexpected.HasStatusCode() {
		switch unexpected.StatusCode() {
		case 401, 403:
			return "invalid_auth"
		case 404:
			return "not_found"
		}
		return fmt.Sprintf("http_%d", unexpected.StatusCode())
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "certificate") || strings.Contains(lower, "tls"):
		return "tls"
	case strings.Contains(lower, "connection refused"), strings.Contains(lower, "no such host"), strings.Contains(lower, "i/o timeout"), strings.Contains(lower, "deadline exceeded"):
		return "connectivity"
	default:
		return "api"
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
