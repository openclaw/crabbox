package railway

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ClaimScope(cfg core.Config) string {
	endpoint := railwayClaimEndpoint(cfg.Railway.APIURL)
	projectID := strings.TrimSpace(cfg.Railway.ProjectID)
	environmentID := strings.TrimSpace(cfg.Railway.EnvironmentID)
	if endpoint != "" && projectID != "" && environmentID != "" {
		return "endpoint:" + endpoint + "|project:" + projectID + "|environment:" + environmentID
	}
	return ""
}

func railwayClaimEndpoint(raw string) string {
	return strings.TrimRight(strings.TrimSpace(core.ClaimScopeURL(raw)), "/")
}
