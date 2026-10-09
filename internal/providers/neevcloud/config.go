package neevcloud

import (
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	providerName = "neevcloud"
	leasePrefix  = "nvc_"
)

// validateConfig checks local shape only; org and project are required at client construction.
func validateConfig(cfg core.Config) error {
	if _, err := validateBaseURL(cfg.Neevcloud.BaseURL); err != nil {
		return err
	}
	if _, err := workdir(cfg); err != nil {
		return err
	}
	if cfg.Neevcloud.TimeoutSecs < 0 {
		return core.Exit(2, "neevcloud timeoutSecs must be non-negative")
	}
	return nil
}

// requiredScope returns the trimmed organization and project the API key acts in.
func requiredScope(cfg core.Config) (string, string, error) {
	orgID, projectID := strings.TrimSpace(cfg.Neevcloud.OrgID), strings.TrimSpace(cfg.Neevcloud.ProjectID)
	if orgID == "" {
		return "", "", core.Exit(2, "provider=neevcloud requires neevcloud.orgId (or CRABBOX_NEEVCLOUD_ORG_ID / --neevcloud-org-id)")
	}
	if projectID == "" {
		return "", "", core.Exit(2, "provider=neevcloud requires neevcloud.projectId (or CRABBOX_NEEVCLOUD_PROJECT_ID / --neevcloud-project-id)")
	}
	return orgID, projectID, nil
}

func validateBaseURL(raw string) (string, error) {
	return shared.NormalizeHTTPSBaseURL(core.Blank(strings.TrimSpace(raw), core.NeevcloudConfigDefaultBaseURL), shared.EndpointURLErrors{
		Invalid:    core.Exit(2, "provider=neevcloud base URL must be an absolute URL"),
		Components: core.Exit(2, "provider=neevcloud base URL must not contain userinfo, query parameters, or a fragment"),
		Insecure:   core.Exit(2, "provider=neevcloud base URL must use HTTPS except for loopback development endpoints"),
	})
}

// workdir returns the cleaned workdir, which must be a subdirectory of /workspace.
func workdir(cfg core.Config) (string, error) {
	raw := core.Blank(strings.TrimSpace(cfg.Neevcloud.Workdir), core.NeevcloudConfigDefaultWorkdir)
	clean := path.Clean(raw)
	if !strings.HasPrefix(clean, workspaceRoot+"/") {
		return "", core.Exit(2, "neevcloud workdir %q must be a subdirectory of %s", raw, workspaceRoot)
	}
	return clean, nil
}

// claimScope keys claims by endpoint, organization and project so another project never matches.
func claimScope(cfg core.Config) string {
	endpoint := shared.NormalizedSandboxClaimEndpoint(core.Blank(strings.TrimSpace(cfg.Neevcloud.BaseURL), core.NeevcloudConfigDefaultBaseURL))
	return "endpoint:" + endpoint + "/orgs/" + strings.TrimSpace(cfg.Neevcloud.OrgID) + "/projects/" + strings.TrimSpace(cfg.Neevcloud.ProjectID)
}

// sandboxTimeoutSeconds is the configured max lifetime, else Crabbox TTL rounded up; 0 sends none.
func sandboxTimeoutSeconds(cfg core.Config) int {
	if cfg.Neevcloud.TimeoutSecs > 0 {
		return cfg.Neevcloud.TimeoutSecs
	}
	if cfg.TTL <= 0 {
		return 0
	}
	return int((cfg.TTL + time.Second - 1) / time.Second)
}
