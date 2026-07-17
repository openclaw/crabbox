package cua

import (
	"context"
	"strings"
)

func (b backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client := newBridgeClient(b.cfg, b.rt)
	resp, err := client.RoundTrip(ctx, bridgeRequest{Action: "doctor"})
	checks := []DoctorCheck{{
		Status:  "ok",
		Check:   "config",
		Message: "provider=cua config=ready experimental=true provisioning=false mutation=false",
		Details: map[string]string{"provider": providerName, "experimental": "true", "provisioning": "false", "mutation": "false"},
	}, {
		Status:  "warning",
		Check:   "provisioning",
		Message: "experimental doctor-only mode; provisioning disabled until CUA exposes an idempotency key or client-assigned identity",
		Details: map[string]string{"provider": providerName, "experimental": "true", "provisioning": "false", "tracking_issue": cuaTrackingIssue, "mutation": "false"},
	}}
	if err != nil {
		checks = append(checks, DoctorCheck{
			Status:  "failed",
			Check:   "bridge",
			Message: redactSecrets(err.Error()),
			Details: map[string]string{"provider": providerName, "class": "environment_blocked", "mutation": "false"},
		})
		return DoctorResult{Provider: providerName, Status: "failed", Message: "bridge=failed mutation=false", Checks: checks}, nil
	}
	for _, item := range resp.Doctor.Checks {
		status := strings.TrimSpace(item.Status)
		if status == "" {
			status = "ok"
		}
		details := map[string]string{"provider": providerName, "mutation": "false"}
		if item.Class != "" {
			details["class"] = item.Class
		}
		for key, value := range item.Details {
			details[key] = value
		}
		checks = append(checks, DoctorCheck{
			Status:  status,
			Check:   strings.TrimSpace(blank(item.Check, "bridge")),
			Message: redactSecrets(item.Message),
			Details: details,
		})
	}
	if resp.Error != nil {
		checks = append(checks, DoctorCheck{
			Status:  "failed",
			Check:   strings.TrimSpace(blank(resp.Error.Code, "bridge")),
			Message: redactSecrets(resp.Error.Message),
			Details: map[string]string{"provider": providerName, "class": blank(resp.Error.Class, "environment_blocked"), "mutation": "false"},
		})
	}
	status := aggregateStatus(checks)
	message := "bridge=ready mutation=false"
	if status != "ok" {
		message = "bridge=classified mutation=false"
	}
	if resp.Doctor.ImportPath != "" {
		message += " import=" + resp.Doctor.ImportPath
	}
	if resp.Doctor.Auth != "" {
		message += " auth=" + resp.Doctor.Auth
	}
	return DoctorResult{Provider: providerName, Status: status, Message: message, Checks: checks}, nil
}
