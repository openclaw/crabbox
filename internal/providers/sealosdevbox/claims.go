package sealosdevbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) claimScope() string {
	return sealosClaimScope(b.cfg)
}

func (b *backend) claimScopeID() string {
	return sealosClaimScopeID(b.cfg)
}

func (b *backend) claimScopeLabel() string {
	return sealosClaimScopeLabel(b.cfg)
}

func sealosClaimScope(cfg core.Config) string {
	kubeconfig := strings.TrimSpace(cfg.SealosDevbox.Kubeconfig)
	if kubeconfig == "" {
		kubeconfig = strings.TrimSpace(os.Getenv("KUBECONFIG"))
	}
	if kubeconfig == "" {
		kubeconfig = "<default>"
	}
	contextName := strings.TrimSpace(cfg.SealosDevbox.Context)
	if contextName == "" {
		contextName = "<current>"
	}
	namespace := strings.TrimSpace(cfg.SealosDevbox.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	network := normalizeNetwork(cfg.SealosDevbox.Network)
	route := "gateway:" + strings.TrimSpace(cfg.SealosDevbox.SSHGatewayHost) + ":" + strings.TrimSpace(cfg.SealosDevbox.SSHGatewayPort)
	if network == networkNodePort {
		route = "node:" + strings.TrimSpace(cfg.SealosDevbox.NodeHost)
	}
	return "kubeconfig:" + kubeconfig + "|context:" + contextName + "|namespace:" + namespace + "|network:" + network + "|" + route
}

func sealosClaimScopeID(cfg core.Config) string {
	return sealosScopeFingerprint(sealosClaimScope(cfg))
}

func sealosClaimScopeLabel(cfg core.Config) string {
	return sealosClaimScopeID(cfg)[:63]
}

func sealosScopeFingerprint(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])
}

func (b *backend) allocateLeaseSlug(ctx context.Context, leaseID, requested string) (string, error) {
	items, err := b.listDevboxes(ctx)
	if err != nil {
		return "", err
	}
	base := core.NormalizeLeaseSlug(requested)
	checkClaims := base != ""
	if base == "" {
		base = core.NewLeaseSlug(leaseID)
	}
	slug := base
	for attempt := 0; attempt < 20; attempt++ {
		inUse := b.devboxSlugInUse(slug, leaseID, items)
		if !inUse && checkClaims {
			inUse, err = b.claimSlugInUse(slug, leaseID)
		}
		if err != nil {
			return "", err
		}
		if !inUse {
			return slug, nil
		}
		slug = core.SlugWithCollisionSuffix(base, fmt.Sprintf("%s-%d", leaseID, attempt))
	}
	return core.SlugWithCollisionSuffix(base, leaseID), nil
}

func (b *backend) devboxSlugInUse(slug, leaseID string, items []devboxItem) bool {
	slug = core.NormalizeLeaseSlug(slug)
	if slug == "" {
		return false
	}
	for _, item := range items {
		if !b.itemMatchesScope(item) {
			continue
		}
		itemSlug := core.NormalizeLeaseSlug(item.Metadata.Labels[slugLabel])
		itemLeaseID := strings.TrimSpace(item.Metadata.Labels[leaseIDLabel])
		if itemLeaseID != leaseID && itemSlug == slug {
			return true
		}
	}
	return false
}

func (b *backend) claimSlugInUse(slug, leaseID string) (bool, error) {
	slug = core.NormalizeLeaseSlug(slug)
	if slug == "" {
		return false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return false, err
	}
	for _, claim := range claims {
		if !b.claimMatchesScope(claim) {
			continue
		}
		if claim.LeaseID != "" && claim.LeaseID != leaseID && core.NormalizeLeaseSlug(claim.Slug) == slug {
			return true, nil
		}
	}
	return false, nil
}

func (b *backend) claimMatchesScope(claim core.LeaseClaim) bool {
	return claim.Provider == providerName && strings.TrimSpace(claim.ProviderScope) == b.claimScope()
}

func (b *backend) claimLeaseForRepo(leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScope(leaseID, slug, providerName, b.claimScope(), repoRoot, idleTimeout, reclaim)
}

func devboxNameFromClaim(claim core.LeaseClaim, cfg core.Config) string {
	if name := strings.TrimSpace(claim.Labels["devbox_name"]); name != "" {
		return name
	}
	if cloudID := strings.TrimSpace(claim.CloudID); cloudID != "" {
		return strings.TrimPrefix(cloudID, strings.TrimSpace(cfg.SealosDevbox.Namespace)+"/")
	}
	return core.LeaseProviderName(claim.LeaseID, claim.Slug)
}

func serverFromClaim(claim core.LeaseClaim, cfg core.Config) core.Server {
	labels := map[string]string{}
	for key, value := range claim.Labels {
		labels[key] = value
	}
	labels["provider"] = providerName
	labels["lease"] = strings.TrimSpace(claim.LeaseID)
	labels["slug"] = core.NormalizeLeaseSlug(claim.Slug)
	labels["provider_scope"] = strings.TrimSpace(claim.ProviderScope)
	namespace := core.Blank(strings.TrimSpace(labels["devbox_namespace"]), strings.TrimSpace(cfg.SealosDevbox.Namespace))
	name := devboxNameFromClaim(claim, cfg)
	labels["devbox_namespace"] = namespace
	labels["devbox_name"] = name
	server := core.Server{
		CloudID:  devboxCloudID(namespace, name),
		Provider: providerName,
		Name:     name,
		Status:   "missing",
		Labels:   labels,
	}
	server.ServerType.Name = "sealos-devbox"
	return server
}

func (b *backend) resolveClaim(identifier string) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, nil
	}
	if claim, err := core.ReadLeaseClaim(identifier); err != nil {
		return core.LeaseClaim{}, false, err
	} else if b.claimMatchesScope(claim) {
		return claim, true, nil
	} else if claim.LeaseID != "" && strings.HasPrefix(identifier, "cbx_") {
		return core.LeaseClaim{}, false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	slug := core.NormalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if !b.claimMatchesScope(claim) {
			continue
		}
		if claim.LeaseID == identifier || (slug != "" && core.NormalizeLeaseSlug(claim.Slug) == slug) || claim.CloudID == identifier {
			return claim, true, nil
		}
	}
	return core.LeaseClaim{}, false, nil
}

func (b *backend) itemMatchesScope(item devboxItem) bool {
	if item.Metadata.Labels[managedByLabel] != "crabbox" {
		return false
	}
	if provider := item.Metadata.Labels[providerLabel]; provider != "" && provider != providerName {
		return false
	}
	return b.itemHasActiveScope(item)
}

func (b *backend) itemHasActiveScope(item devboxItem) bool {
	scopeID := strings.TrimSpace(item.Metadata.Annotations[annotationBase+"provider-scope"])
	if scopeID != "" {
		return scopeID == b.claimScopeID()
	}
	scopeID = strings.TrimSpace(item.Metadata.Annotations[annotationBase+"provider_scope_id"])
	if scopeID != "" {
		return scopeID == b.claimScopeID()
	}
	scopeLabel := strings.TrimSpace(item.Metadata.Labels[providerScopeLabel])
	if scopeLabel != "" && scopeLabel == b.claimScopeLabel() {
		return true
	}
	legacyRawScope := strings.TrimSpace(item.Metadata.Annotations[annotationBase+"provider_scope"])
	return legacyRawScope != "" && legacyRawScope == b.claimScope()
}

func (b *backend) serverFromDevbox(item devboxItem) core.Server {
	labels := leaseLabelsFromDevbox(item)
	labels["provider"] = providerName
	labels["devbox_namespace"] = core.Blank(strings.TrimSpace(item.Metadata.Namespace), b.cfg.SealosDevbox.Namespace)
	labels["devbox_name"] = strings.TrimSpace(item.Metadata.Name)
	labels["network"] = normalizeNetwork(b.cfg.SealosDevbox.Network)
	labels["provider-scope"] = b.claimScopeID()
	labels["provider_scope_id"] = b.claimScopeID()
	labels["provider_scope"] = b.claimScope()
	leaseID := core.Blank(strings.TrimSpace(item.Metadata.Labels[leaseIDLabel]), labels["lease"])
	slug := core.Blank(core.NormalizeLeaseSlug(item.Metadata.Labels[slugLabel]), core.NormalizeLeaseSlug(labels["slug"]))
	if leaseID != "" {
		labels["lease"] = leaseID
	}
	if slug != "" {
		labels["slug"] = slug
	}
	status := devboxStatusLabel(item)
	labels["state"] = status
	server := core.Server{
		CloudID:  devboxCloudID(labels["devbox_namespace"], labels["devbox_name"]),
		Provider: providerName,
		Name:     labels["devbox_name"],
		Status:   status,
		Labels:   labels,
	}
	server.ServerType.Name = "sealos-devbox"
	return server
}

func leaseLabelsFromDevbox(item devboxItem) map[string]string {
	labels := map[string]string{}
	for key, value := range item.Metadata.Annotations {
		if strings.HasPrefix(key, annotationBase) {
			labels[strings.TrimPrefix(key, annotationBase)] = redactSensitive(value)
		}
	}
	for key, value := range item.Metadata.Labels {
		switch key {
		case leaseIDLabel:
			labels["lease"] = value
		case slugLabel:
			labels["slug"] = value
		case providerLabel:
			labels["provider"] = value
		}
	}
	if item.Metadata.CreationTimestamp != "" {
		labels["created"] = item.Metadata.CreationTimestamp
	}
	return labels
}

func devboxCloudID(namespace, name string) string {
	return strings.Trim(strings.TrimSpace(namespace)+"/"+strings.TrimSpace(name), "/")
}

func (b *backend) resolveDevbox(ctx context.Context, identifier string) (devboxItem, string, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return devboxItem{}, "", "", "", core.Exit(2, "provider=%s requires --id <devbox-name-or-slug>", providerName)
	}
	if claim, ok, err := b.resolveClaim(identifier); err != nil {
		return devboxItem{}, "", "", "", err
	} else if ok {
		name := devboxNameFromClaim(claim, b.cfg)
		item, err := b.getDevbox(ctx, name)
		if err != nil {
			return devboxItem{}, "", "", "", err
		}
		if !b.itemMatchesScope(item) {
			return devboxItem{}, "", "", "", core.Exit(4, "Sealos DevBox %q is outside the active provider scope", name)
		}
		actualName, leaseID, slug, err := identityFromDevbox(item, name)
		if err != nil {
			return devboxItem{}, "", "", "", err
		}
		if leaseID != claim.LeaseID {
			return devboxItem{}, "", "", "", core.Exit(4, "Sealos DevBox %q lease identity changed: expected %s, found %s", actualName, claim.LeaseID, leaseID)
		}
		if core.NormalizeLeaseSlug(claim.Slug) != "" && slug != core.NormalizeLeaseSlug(claim.Slug) {
			return devboxItem{}, "", "", "", core.Exit(4, "Sealos DevBox %q slug identity changed: expected %s, found %s", actualName, claim.Slug, slug)
		}
		return item, actualName, leaseID, slug, nil
	}
	if strings.HasPrefix(identifier, "cbx_") {
		items, err := b.listDevboxes(ctx)
		if err != nil {
			return devboxItem{}, "", "", "", err
		}
		for _, item := range items {
			if !b.itemMatchesScope(item) {
				continue
			}
			if item.Metadata.Labels[leaseIDLabel] == identifier {
				name, leaseID, slug, err := identityFromDevbox(item, identifier)
				return item, name, leaseID, slug, err
			}
		}
		return devboxItem{}, "", "", "", core.Exit(4, "Sealos DevBox lease %q was not found in namespace %s", identifier, b.cfg.SealosDevbox.Namespace)
	}
	item, err := b.findDevboxByNameOrSlug(ctx, identifier)
	if err != nil {
		return devboxItem{}, "", "", "", err
	}
	name, leaseID, slug, err := identityFromDevbox(item, identifier)
	return item, name, leaseID, slug, err
}

func (b *backend) findDevboxByNameOrSlug(ctx context.Context, identifier string) (devboxItem, error) {
	items, err := b.listDevboxes(ctx)
	if err != nil {
		return devboxItem{}, err
	}
	for _, item := range items {
		if !b.itemMatchesScope(item) {
			continue
		}
		if strings.TrimSpace(item.Metadata.Name) == identifier {
			return item, nil
		}
	}
	slug := core.NormalizeLeaseSlug(identifier)
	if slug == "" {
		return devboxItem{}, core.Exit(4, "Sealos DevBox or slug %q was not found in namespace %s", identifier, b.cfg.SealosDevbox.Namespace)
	}
	matches := make([]devboxItem, 0, 1)
	for _, item := range items {
		if !b.itemMatchesScope(item) {
			continue
		}
		if core.NormalizeLeaseSlug(item.Metadata.Labels[slugLabel]) == slug {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return devboxItem{}, core.Exit(4, "Sealos DevBox or slug %q was not found in namespace %s", identifier, b.cfg.SealosDevbox.Namespace)
	default:
		return devboxItem{}, core.Exit(4, "Sealos DevBox slug %q matched %d resources in namespace %s", identifier, len(matches), b.cfg.SealosDevbox.Namespace)
	}
}

func identityFromDevbox(item devboxItem, identifier string) (name, leaseID, slug string, err error) {
	name = strings.TrimSpace(item.Metadata.Name)
	leaseID = strings.TrimSpace(item.Metadata.Labels[leaseIDLabel])
	slug = core.NormalizeLeaseSlug(item.Metadata.Labels[slugLabel])
	if name == "" {
		return "", "", "", core.Exit(5, "Sealos DevBox %q has no metadata.name", identifier)
	}
	if leaseID == "" || slug == "" {
		return "", "", "", core.Exit(4, "Sealos DevBox %q is missing Crabbox identity labels", name)
	}
	return name, leaseID, slug, nil
}

func (b *backend) validateDevboxIdentity(ctx context.Context, name, expectedLeaseID, expectedSlug string) (devboxItem, string, string, string, error) {
	item, err := b.getDevbox(ctx, name)
	if err != nil {
		return devboxItem{}, "", "", "", err
	}
	actualName, leaseID, slug, err := identityFromDevbox(item, name)
	if err != nil {
		return devboxItem{}, "", "", "", err
	}
	if expectedLeaseID = strings.TrimSpace(expectedLeaseID); expectedLeaseID != "" && leaseID != expectedLeaseID {
		return devboxItem{}, "", "", "", core.Exit(4, "Sealos DevBox %q lease identity changed: expected %s, found %s", actualName, expectedLeaseID, leaseID)
	}
	if expectedSlug = core.NormalizeLeaseSlug(expectedSlug); expectedSlug != "" && slug != expectedSlug {
		return devboxItem{}, "", "", "", core.Exit(4, "Sealos DevBox %q slug identity changed: expected %s, found %s", actualName, expectedSlug, slug)
	}
	return item, actualName, leaseID, slug, nil
}
