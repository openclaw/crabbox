package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type azAccountInfo struct {
	ID       string `json:"id"`
	TenantID string `json:"tenantId"`
	Name     string `json:"name"`
}

// azAccountShow runs "az account show" and returns the active subscription.
// If subscription is non-empty it passes --subscription to select a specific one.
func azAccountShow(ctx context.Context, subscription string) (azAccountInfo, error) {
	azPath, err := exec.LookPath("az")
	if err != nil {
		return azAccountInfo{}, fmt.Errorf("az CLI not found on PATH; install it from https://aka.ms/installazurecli")
	}
	args := []string{"account", "show", "--output", "json"}
	if subscription != "" {
		args = append(args, "--subscription", subscription)
	}
	cmd := exec.CommandContext(ctx, azPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return azAccountInfo{}, fmt.Errorf("az account show: %s", stderr)
			}
		}
		return azAccountInfo{}, fmt.Errorf("az account show: %w", err)
	}
	var info azAccountInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return azAccountInfo{}, fmt.Errorf("az account show: parse output: %w", err)
	}
	if info.ID == "" {
		return azAccountInfo{}, fmt.Errorf("az account show: subscription ID is empty; run 'az login' first")
	}
	return info, nil
}
