package nvidiabrev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

type brevOrg struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	IsActive bool   `json:"is_active"`
}

type brevCredentials struct {
	APIKey      string `json:"api_key"`
	APIKeyOrgID string `json:"api_key_org_id"`
}

type brevWorkspaceMeta struct {
	WorkspaceID    string `json:"workspaceId"`
	OrganizationID string `json:"organizationId"`
}

var brevWorkspaceMetaPath = "/etc/meta/workspace.json"

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

func (c *brevClient) activeOrg(ctx context.Context) (brevOrg, error) {
	org, found, err := readLocalEffectiveBrevOrg()
	if err != nil {
		return brevOrg{}, err
	}
	if found {
		return org, nil
	}
	result, err := c.run(ctx, "ls", "orgs", "--json")
	if err != nil {
		return brevOrg{}, fmt.Errorf("brev active organization lookup failed: %w", err)
	}
	var orgs []brevOrg
	if err := json.Unmarshal([]byte(result.Stdout), &orgs); err != nil {
		return brevOrg{}, fmt.Errorf("parse brev organization JSON: %w", err)
	}
	var active brevOrg
	for _, candidate := range orgs {
		candidate.ID = strings.TrimSpace(candidate.ID)
		candidate.Name = strings.TrimSpace(candidate.Name)
		if !candidate.IsActive {
			continue
		}
		if candidate.ID == "" {
			return brevOrg{}, exit(2, "brev active organization has no id")
		}
		if active.ID != "" {
			return brevOrg{}, exit(2, "brev returned multiple active organizations")
		}
		active = candidate
	}
	if active.ID == "" {
		if len(orgs) == 0 {
			return brevOrg{}, exit(2, "brev returned no accessible organization")
		}
		return brevOrg{}, exit(2, "brev has no active organization; run `brev set` before nvidia-brev lifecycle operations")
	}
	return active, nil
}

func readLocalEffectiveBrevOrg() (brevOrg, bool, error) {
	credentials, foundCredentials, err := readBrevCredentials()
	if err != nil {
		return brevOrg{}, false, err
	}
	if foundCredentials && strings.HasPrefix(strings.TrimSpace(credentials.APIKey), "bak-") {
		orgID := strings.TrimSpace(credentials.APIKeyOrgID)
		if orgID == "" {
			return brevOrg{}, false, exit(2, "Brev API-key credentials have no organization id")
		}
		return brevOrg{ID: orgID, Name: orgID, IsActive: true}, true, nil
	}
	workspaceOrg, inWorkspace, err := readBrevWorkspaceOrg()
	if err != nil {
		return brevOrg{}, false, err
	}
	if inWorkspace {
		return workspaceOrg, true, nil
	}
	return brevOrg{}, false, nil
}

func readBrevWorkspaceOrg() (brevOrg, bool, error) {
	data, err := os.ReadFile(brevWorkspaceMetaPath)
	if errors.Is(err, os.ErrNotExist) {
		return brevOrg{}, false, nil
	}
	if err != nil {
		return brevOrg{}, false, nil
	}
	var meta brevWorkspaceMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return brevOrg{}, false, nil
	}
	meta.WorkspaceID = strings.TrimSpace(meta.WorkspaceID)
	meta.OrganizationID = strings.TrimSpace(meta.OrganizationID)
	if meta.WorkspaceID == "" || meta.OrganizationID == "" {
		return brevOrg{}, false, nil
	}
	return brevOrg{ID: meta.OrganizationID, Name: meta.OrganizationID, IsActive: true}, true, nil
}

func readBrevCredentials() (brevCredentials, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return brevCredentials{}, false, fmt.Errorf("resolve home directory for Brev credentials: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".brev", "credentials.json"))
	if errors.Is(err, os.ErrNotExist) {
		return brevCredentials{}, false, nil
	}
	if err != nil {
		return brevCredentials{}, false, nil
	}
	var credentials brevCredentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return brevCredentials{}, false, nil
	}
	return credentials, true, nil
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
	if strings.EqualFold(strings.TrimSpace(cfg.ReleaseAction), "stop") {
		args = append(args, "--stoppable")
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

func (c *brevClient) start(ctx context.Context, idOrName string) error {
	if err := c.rejectOrgScopedMutation("start"); err != nil {
		return err
	}
	if _, err := c.run(ctx, "start", idOrName, "--detached"); err != nil {
		return fmt.Errorf("brev start failed: %w", err)
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
	result, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: strings.TrimSpace(c.cfg.NvidiaBrev.CLI),
		Args: append([]string(nil), args...),
	})
	if err == nil {
		return result, nil
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if len(detail) > 4096 {
		detail = detail[:4096] + "..."
	}
	if detail != "" {
		return result, fmt.Errorf("%w: %s", err, detail)
	}
	return result, err
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
