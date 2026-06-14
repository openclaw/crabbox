package vercelsandbox

import (
	"context"
)

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	api, err := b.client()
	if err != nil {
		return DoctorResult{}, err
	}
	checks := []DoctorCheck{
		{Status: "ok", Check: "sdk", Message: "bridge=contract mutation=false", Details: map[string]string{"bridge": "contract", "mutation": "false"}},
	}
	if err := api.CheckSDK(ctx); err != nil {
		checks[0] = DoctorCheck{Status: "failed", Check: "sdk", Message: redactSecrets(err.Error()), Details: map[string]string{"class": "environment_blocked"}}
	}
	if path, err := api.CheckCLI(ctx); err != nil {
		checks = append(checks, DoctorCheck{Status: "failed", Check: "cli", Message: redactSecrets(err.Error()), Details: map[string]string{"class": "environment_blocked", "mutation": "false"}})
	} else {
		checks = append(checks, DoctorCheck{Status: "ok", Check: "cli", Message: "sandbox=" + path, Details: map[string]string{"path": path, "mutation": "false"}})
	}
	if err := api.CheckAuth(ctx); err != nil {
		checks = append(checks, DoctorCheck{Status: "failed", Check: "auth", Message: redactSecrets(err.Error()), Details: map[string]string{"class": "environment_blocked", "mutation": "false"}})
	} else {
		checks = append(checks, DoctorCheck{Status: "ok", Check: "auth", Message: "auth=ready mutation=false", Details: map[string]string{"auth": "ready", "mutation": "false"}})
	}
	if err := api.CheckProject(ctx); err != nil {
		checks = append(checks, DoctorCheck{Status: "warning", Check: "project", Message: redactSecrets(err.Error()), Details: map[string]string{"class": "environment_blocked", "mutation": "false"}})
	} else {
		checks = append(checks, DoctorCheck{Status: "ok", Check: "project", Message: "project=ready mutation=false", Details: map[string]string{"project": "ready", "mutation": "false"}})
	}
	sandboxes, err := api.ListSandboxes(ctx)
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "warning", Check: "inventory", Message: redactSecrets(err.Error()), Details: map[string]string{"class": "environment_blocked", "mutation": "false"}})
	} else {
		result := inventoryDoctorResult(providerName, len(sandboxes))
		checks = append(checks, DoctorCheck{Status: "ok", Check: "inventory", Message: result.Message, Details: map[string]string{"mutation": "false"}})
	}
	return DoctorResult{Provider: providerName, Status: aggregateStatus(checks), Checks: checks}, nil
}

func aggregateStatus(checks []DoctorCheck) string {
	for _, check := range checks {
		if check.Status == "failed" {
			return "failed"
		}
	}
	for _, check := range checks {
		if check.Status == "warning" {
			return "warning"
		}
	}
	return "ok"
}
