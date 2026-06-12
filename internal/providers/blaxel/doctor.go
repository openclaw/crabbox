package blaxel

import (
	"context"
	"errors"
)

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	checks := []DoctorCheck{}
	record := func(status, check, message string) {
		checks = append(checks, DoctorCheck{
			Status:  status,
			Check:   check,
			Message: message,
			Details: map[string]string{"provider": providerName},
		})
	}
	if _, err := ValidateAPIURL(blank(b.cfg.Blaxel.APIURL, defaultAPIURL)); err != nil {
		record("failed", "api_url", err.Error())
		return DoctorResult{Provider: providerName, Status: "failed", Message: "api_url=failed mutation=false", Checks: checks}, err
	}
	record("ok", "api_url", "api_url=ready mutation=false")
	if BlaxelAPIKey(b.cfg) == "" {
		err := exit(1, "provider=blaxel needs an API key; load CRABBOX_BLAXEL_API_KEY or BL_API_KEY from a secret manager")
		record("failed", "auth", err.Error())
		return DoctorResult{Provider: providerName, Status: "failed", Message: "auth=missing mutation=false", Checks: checks}, err
	}
	record("ok", "auth", "auth=configured")
	if b.cfg.Blaxel.Workspace == "" {
		record("warning", "workspace", "workspace=not_configured header=omitted")
	} else {
		record("ok", "workspace", "workspace=configured")
	}
	client, err := b.clientFactory(b.cfg, b.rt)
	if err != nil {
		record("failed", "client", err.Error())
		return DoctorResult{Provider: providerName, Status: "failed", Message: "client=failed mutation=false", Checks: checks}, err
	}
	if err := client.Probe(ctx); err != nil {
		record("failed", "api", err.Error())
		return DoctorResult{Provider: providerName, Status: "failed", Message: "api=failed mutation=false", Checks: checks}, err
	}
	record("ok", "api", "api=list mutation=false")
	result, err := client.ListSandboxes(ctx, ListSandboxesRequest{Limit: 100})
	if err != nil {
		record("failed", "inventory", err.Error())
		return DoctorResult{Provider: providerName, Status: "failed", Message: "inventory=failed mutation=false", Checks: checks}, err
	}
	doctor := inventoryDoctorResult(providerName, len(result.Sandboxes))
	doctor.Checks = checks
	if doctor.Message == "" {
		return doctor, errors.New("blaxel doctor did not produce a summary")
	}
	return doctor, nil
}
