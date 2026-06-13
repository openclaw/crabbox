package nvidiabrev

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type brevClient struct {
	cfg Config
	rt  Runtime
}

type brevWorkspace struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	BuildStatus    string `json:"build_status"`
	ShellStatus    string `json:"shell_status"`
	HealthStatus   string `json:"health_status"`
	InstanceType   string `json:"instance_type"`
	InstanceKind   string `json:"instance_kind"`
	GPU            string `json:"gpu"`
	WorkspaceClass string `json:"workspace_class"`
}

func newBrevClient(cfg Config, rt Runtime) (*brevClient, error) {
	applyNvidiaBrevDefaults(&cfg)
	if rt.Exec == nil {
		return nil, exit(2, "provider=%s requires Runtime.Exec", providerName)
	}
	if strings.TrimSpace(cfg.NvidiaBrev.CLI) == "" {
		return nil, exit(2, "provider=%s requires nvidiaBrev.cli", providerName)
	}
	return &brevClient{cfg: cfg, rt: rt}, nil
}

func (c *brevClient) version(ctx context.Context) (LocalCommandResult, error) {
	return c.run(ctx, "--version")
}

func (c *brevClient) list(ctx context.Context, all bool) ([]brevWorkspace, error) {
	args := []string{"ls", "--json"}
	if strings.TrimSpace(c.cfg.NvidiaBrev.Org) != "" {
		args = append(args, "--org", strings.TrimSpace(c.cfg.NvidiaBrev.Org))
	}
	if all {
		args = append(args, "--all")
	}
	result, err := c.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("brev ls failed: %w", err)
	}
	workspaces, err := parseBrevWorkspaces(result.Stdout)
	if err != nil {
		return nil, fmt.Errorf("parse brev ls JSON: %w", err)
	}
	return workspaces, nil
}

func (c *brevClient) create(ctx context.Context, name string) error {
	if err := c.rejectOrgScopedMutation("create"); err != nil {
		return err
	}
	args := []string{"create", name, "--detached"}
	cfg := c.cfg.NvidiaBrev
	if strings.TrimSpace(cfg.Type) != "" {
		args = append(args, "--type", strings.TrimSpace(cfg.Type))
	}
	if strings.TrimSpace(cfg.GPUName) != "" {
		args = append(args, "--gpu-name", strings.TrimSpace(cfg.GPUName))
	}
	if strings.TrimSpace(cfg.Provider) != "" {
		args = append(args, "--provider", strings.TrimSpace(cfg.Provider))
	}
	if strings.TrimSpace(cfg.Mode) != "" {
		args = append(args, "--mode", strings.TrimSpace(cfg.Mode))
	}
	if strings.TrimSpace(cfg.Launchable) != "" {
		args = append(args, "--launchable", strings.TrimSpace(cfg.Launchable))
	}
	if strings.TrimSpace(cfg.StartupScript) != "" {
		args = append(args, "--startup-script", strings.TrimSpace(cfg.StartupScript))
	}
	if _, err := c.run(ctx, args...); err != nil {
		return fmt.Errorf("brev create failed: %w", err)
	}
	return nil
}

func (c *brevClient) refresh(ctx context.Context) error {
	if _, err := c.run(ctx, "refresh"); err != nil {
		return fmt.Errorf("brev refresh failed: %w", err)
	}
	return nil
}

func (c *brevClient) stop(ctx context.Context, idOrName string) error {
	if err := c.rejectOrgScopedMutation("stop"); err != nil {
		return err
	}
	if _, err := c.run(ctx, "stop", idOrName); err != nil {
		return fmt.Errorf("brev stop failed: %w", err)
	}
	return nil
}

func (c *brevClient) delete(ctx context.Context, idOrName string) error {
	if err := c.rejectOrgScopedMutation("delete"); err != nil {
		return err
	}
	if _, err := c.run(ctx, "delete", idOrName); err != nil {
		return fmt.Errorf("brev delete failed: %w", err)
	}
	return nil
}

func (c *brevClient) rejectOrgScopedMutation(operation string) error {
	if strings.TrimSpace(c.cfg.NvidiaBrev.Org) == "" {
		return nil
	}
	return exit(2, "nvidiaBrev.org scopes read-only Brev inventory only; brev %s does not support --org, so lifecycle mutation is unsafe. Run `brev set` for the desired active org or remove nvidiaBrev.org before using nvidia-brev lifecycle commands", operation)
}

func (c *brevClient) run(ctx context.Context, args ...string) (LocalCommandResult, error) {
	return c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: strings.TrimSpace(c.cfg.NvidiaBrev.CLI),
		Args: append([]string(nil), args...),
	})
}

type brevWorkspaceListJSON struct {
	Workspaces json.RawMessage `json:"workspaces"`
}

func parseBrevWorkspaces(stdout string) ([]brevWorkspace, error) {
	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return nil, nil
	}
	if strings.HasPrefix(raw, "[") {
		var out []brevWorkspace
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, err
		}
		return normalizeBrevWorkspaces(out), nil
	}
	var object brevWorkspaceListJSON
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return nil, err
	}
	if len(object.Workspaces) == 0 {
		return nil, fmt.Errorf("missing workspaces field")
	}
	if strings.EqualFold(strings.TrimSpace(string(object.Workspaces)), "null") {
		return nil, nil
	}
	var out []brevWorkspace
	if err := json.Unmarshal(object.Workspaces, &out); err != nil {
		return nil, err
	}
	return normalizeBrevWorkspaces(out), nil
}

func normalizeBrevWorkspaces(items []brevWorkspace) []brevWorkspace {
	out := items[:0]
	for _, item := range items {
		item.ID = strings.TrimSpace(item.ID)
		item.Name = strings.TrimSpace(item.Name)
		item.Status = strings.TrimSpace(item.Status)
		item.BuildStatus = strings.TrimSpace(item.BuildStatus)
		item.ShellStatus = strings.TrimSpace(item.ShellStatus)
		item.HealthStatus = strings.TrimSpace(item.HealthStatus)
		item.InstanceType = strings.TrimSpace(item.InstanceType)
		item.InstanceKind = strings.TrimSpace(item.InstanceKind)
		item.GPU = strings.TrimSpace(item.GPU)
		item.WorkspaceClass = strings.TrimSpace(item.WorkspaceClass)
		out = append(out, item)
	}
	return out
}

func countBrevWorkspaces(stdout string) (int, error) {
	workspaces, err := parseBrevWorkspaces(stdout)
	if err != nil {
		return 0, err
	}
	return len(workspaces), nil
}
