package proxmox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// ControllerProviderScope binds adapter workspaces to one Proxmox API route,
// source node, token principal and clone profile. It hashes only non-secret
// settings. A new token secret for the same token ID keeps the scope; another
// token ID, route, node or clone setting changes it, so existing workspaces
// are refused until the original configuration is restored. A configuration
// without a token secret has no identity, so the adapter will not start.
func (Provider) ControllerProviderScope(cfg core.Config) (string, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return "", core.Exit(2, "provider=proxmox supports target=linux only")
	}
	cfg = withProxmoxGuestAccess(cfg)
	scope := proxmoxControllerScope{
		Endpoint:   normalizedProxmoxClaimEndpoint(cfg.Proxmox.APIURL),
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
	case scope.Endpoint == "":
		return "", core.Exit(3, "proxmox apiUrl is required (set proxmox.apiUrl or CRABBOX_PROXMOX_API_URL)")
	case scope.Node == "":
		return "", core.Exit(3, "proxmox node is required (set proxmox.node or CRABBOX_PROXMOX_NODE)")
	case scope.TokenID == "":
		return "", core.Exit(3, "proxmox tokenId is required (set proxmox.tokenId or CRABBOX_PROXMOX_TOKEN_ID)")
	case strings.TrimSpace(cfg.Proxmox.TokenSecret) == "":
		// Presence only: the secret never enters the persisted scope.
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

// SupportsControllerFixedLeaseID reports the fixed-ID contract for exactly the
// configurations that fixed acquisition accepts: complete API credentials and
// a Linux template on one source node and cluster route. Acquisition still
// verifies inventory access.
func (p Provider) SupportsControllerFixedLeaseID(cfg core.Config) bool {
	_, err := p.ControllerProviderScope(cfg)
	return err == nil
}
