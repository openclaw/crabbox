package proxmox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const proxmoxControllerScopePrefix = "proxmox-v1:sha256:"

// proxmoxControllerScope is a persisted runtime-adapter contract: field names,
// order and omission rules must not change once an adapter records a scope.
type proxmoxControllerScope struct {
	Endpoint   string `json:"endpoint"`
	Node       string `json:"node"`
	TokenID    string `json:"tokenId"`
	TemplateID int    `json:"templateId"`
	Storage    string `json:"storage,omitempty"`
	Pool       string `json:"pool,omitempty"`
	Bridge     string `json:"bridge,omitempty"`
	FullClone  bool   `json:"fullClone"`
	User       string `json:"user"`
	WorkRoot   string `json:"workRoot"`
}

// ControllerProviderScope binds workspaces to a route, principal and clone
// profile, excluding the token secret so secret rotation preserves access.
func (Provider) ControllerProviderScope(cfg core.Config) (string, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return "", core.Exit(2, "provider=proxmox supports target=linux only")
	}
	cfg = withProxmoxGuestAccess(cfg)
	// Match the client's suffix handling without collapsing escaped proxy paths.
	apiURL := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(cfg.Proxmox.APIURL), "/"), "/api2/json")
	endpoint, err := url.Parse(apiURL)
	if err != nil || endpoint.Hostname() == "" ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(apiURL, "#") {
		return "", core.Exit(3, "proxmox apiUrl must be an absolute HTTP(S) URL without userinfo, query or fragment")
	}
	endpoint.Host = strings.ToLower(endpoint.Host)
	scope := proxmoxControllerScope{
		Endpoint:   endpoint.String(),
		Node:       strings.TrimSpace(cfg.Proxmox.Node),
		TokenID:    strings.TrimSpace(cfg.Proxmox.TokenID),
		TemplateID: cfg.Proxmox.TemplateID,
		Storage:    strings.TrimSpace(cfg.Proxmox.Storage),
		Pool:       strings.TrimSpace(cfg.Proxmox.Pool),
		Bridge:     strings.TrimSpace(cfg.Proxmox.Bridge),
		FullClone:  cfg.Proxmox.FullClone,
		User:       strings.TrimSpace(cfg.SSHUser),
		WorkRoot:   strings.TrimSpace(cfg.WorkRoot),
	}
	switch {
	case scope.Node == "":
		return "", core.Exit(3, "proxmox node is required (set proxmox.node or CRABBOX_PROXMOX_NODE)")
	case scope.TokenID == "":
		return "", core.Exit(3, "proxmox tokenId is required (set proxmox.tokenId or CRABBOX_PROXMOX_TOKEN_ID)")
	case strings.TrimSpace(cfg.Proxmox.TokenSecret) == "":
		return "", core.Exit(3, "proxmox tokenSecret is required (set proxmox.tokenSecret or CRABBOX_PROXMOX_TOKEN_SECRET)")
	case scope.TemplateID <= 0:
		return "", core.Exit(3, "proxmox templateId is required (set proxmox.templateId or CRABBOX_PROXMOX_TEMPLATE_ID)")
	case scope.User == "" || scope.WorkRoot == "":
		return "", core.Exit(3, "proxmox guest user and work root are required")
	}
	data, err := json.Marshal(scope)
	if err != nil {
		return "", fmt.Errorf("encode Proxmox controller provider scope: %w", err)
	}
	sum := sha256.Sum256(data)
	return proxmoxControllerScopePrefix + hex.EncodeToString(sum[:]), nil
}

func (p Provider) SupportsControllerFixedLeaseID(cfg core.Config) bool {
	_, err := p.ControllerProviderScope(cfg)
	return err == nil
}
