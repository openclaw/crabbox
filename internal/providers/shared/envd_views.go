package shared

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// EnvdSandboxViews projects compatible sandbox metadata into Crabbox views.
// The adapter supplies its provider identity and legacy synthetic-ID prefix.
type EnvdSandboxViews struct {
	Provider    string
	LeasePrefix string
}

func (v EnvdSandboxViews) Server(sandbox EnvdSandbox) core.Server {
	leaseID := v.LeaseID(sandbox)
	labels := LabelsWithDefaults(sandbox.Metadata, map[string]string{
		"slug":  core.NewLeaseSlug(leaseID),
		"state": sandbox.State,
	})
	labels["provider"] = v.Provider
	labels["lease"] = leaseID
	labels["target"] = core.TargetLinux
	server := core.Server{
		Provider: v.Provider,
		CloudID:  sandbox.SandboxID,
		Name:     sandbox.SandboxID,
		Status:   sandbox.State,
		Labels:   labels,
	}
	server.ServerType.Name = core.Blank(sandbox.Alias, sandbox.TemplateID)
	if server.ServerType.Name == "" {
		server.ServerType.Name = "base"
	}
	return server
}

func (v EnvdSandboxViews) Status(leaseID string, sandbox EnvdSandbox) core.StatusView {
	server := v.Server(sandbox)
	return core.StatusView{
		ID:         leaseID,
		Slug:       EnvdSandboxSlug(leaseID, sandbox),
		Provider:   v.Provider,
		TargetOS:   core.TargetLinux,
		State:      sandbox.State,
		ServerID:   sandbox.SandboxID,
		ServerType: server.ServerType.Name,
		Network:    core.NetworkPublic,
		Ready:      EnvdSandboxStatusReady(sandbox.State),
		Labels:     server.Labels,
	}
}

func (v EnvdSandboxViews) LeaseID(sandbox EnvdSandbox) string {
	if lease := strings.TrimSpace(sandbox.Metadata["lease"]); lease != "" {
		return lease
	}
	return v.LeasePrefix + sandbox.SandboxID
}

func EnvdSandboxSlug(leaseID string, sandbox EnvdSandbox) string {
	if slug := strings.TrimSpace(sandbox.Metadata["slug"]); slug != "" {
		return slug
	}
	return core.NewLeaseSlug(leaseID)
}

func EnvdSandboxStatusReady(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "running":
		return true
	default:
		return false
	}
}
