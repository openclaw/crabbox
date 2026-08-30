package modal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type modalScope struct {
	Endpoint      string `json:"endpoint"`
	Workspace     string `json:"workspace"`
	EnvironmentID string `json:"environment_id"`
	Environment   string `json:"environment"`
	AppID         string `json:"app_id"`
	App           string `json:"app"`
}

type modalBinding struct {
	ID      string     `json:"id"`
	LeaseID string     `json:"lease"`
	Slug    string     `json:"slug"`
	Scope   modalScope `json:"scope"`
}

func modalObjectID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) || len(id) > 256 {
		return false
	}
	for _, r := range strings.TrimPrefix(id, prefix) {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (s modalScope) canonical() (string, error) {
	for _, endpoint := range strings.Split(s.Endpoint, ",") {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return "", exit(2, "Modal ownership requires API endpoints without credentials, query, or fragment")
		}
	}
	if !modalObjectID(s.EnvironmentID, "en-") || !modalObjectID(s.AppID, "ap-") {
		return "", exit(2, "Modal ownership requires native environment and app IDs")
	}
	for _, value := range []string{s.Workspace, s.Environment, s.App} {
		if value == "" || len(value) > 256 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
			return "", exit(2, "Modal ownership requires an authenticated workspace and resolved environment/app names")
		}
	}
	data, err := json.Marshal(s)
	return string(data), err
}

func (b modalBinding) validate(sandbox modalSandbox) error {
	if !modalObjectID(b.ID, "sb-") || !core.IsCanonicalLeaseID(b.LeaseID) || b.Slug == "" {
		return exit(2, "Modal ownership requires an exact sandbox, lease, and slug")
	}
	if _, err := b.Scope.canonical(); err != nil {
		return err
	}
	if sandbox.ID != b.ID || sandbox.Scope != b.Scope {
		return exit(2, "Modal sandbox identity or authority changed; retaining resource")
	}
	for key, want := range map[string]string{"provider": providerName, "crabbox": "true", "lease": b.LeaseID, "slug": b.Slug, "app": b.Scope.App} {
		if sandbox.Tags[key] != want {
			return exit(2, "Modal sandbox ownership tag %s does not match the exact claim", key)
		}
	}
	if sandbox.Status != "running" && sandbox.Status != "finished" {
		return exit(2, "Modal sandbox terminal state is unknown")
	}
	return nil
}

func modalClaimBinding(claim core.LeaseClaim) (modalBinding, shared.ClaimBinding, error) {
	binding := modalBinding{ID: claim.CloudID, LeaseID: claim.LeaseID, Slug: claim.Slug}
	want := shared.ClaimBinding{Provider: providerName, LeaseID: claim.LeaseID, Slug: claim.Slug, CloudID: claim.CloudID, ProviderScope: claim.ProviderScope, ExactProviderScope: true}
	if json.Unmarshal([]byte(claim.ProviderScope), &binding.Scope) != nil || claim.Revision == "" {
		return binding, want, exit(2, "Modal lease %q has no exact sandbox/scope binding; inspect and clean up through Modal directly; --reclaim cannot adopt legacy ownership", claim.LeaseID)
	}
	scope, err := binding.Scope.canonical()
	if err != nil || scope != claim.ProviderScope {
		return binding, want, exit(2, "Modal lease %q has an incomplete or invalid authority binding", claim.LeaseID)
	}
	if err := binding.validate(modalSandbox{ID: claim.CloudID, Scope: binding.Scope, Tags: claim.Labels, Status: "running"}); err != nil {
		return binding, want, err
	}
	return binding, want, shared.ValidateClaimBinding(claim, want)
}

func resolveModalClaim(id string) (core.LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.LeaseClaim{}, exit(2, "provider=modal requires a claimed lease id, slug, or sandbox id")
	}
	var claim core.LeaseClaim
	var ok bool
	var err error
	if strings.HasPrefix(id, "sb-") {
		claims, listErr := core.ListLeaseClaims()
		if listErr != nil {
			return claim, listErr
		}
		for _, candidate := range claims {
			if candidate.CloudID == id {
				if ok {
					return claim, exit(2, "Modal sandbox %q has ambiguous local claims", id)
				}
				claim, ok = candidate, true
			}
		}
	} else {
		var exact bool
		claim, ok, exact, err = core.ResolveLeaseClaimForProviderWithExact(id, providerName)
		if (exact || core.IsCanonicalLeaseID(id)) && (!exact || !ok || claim.LeaseID != id) {
			return claim, shared.ErrStrictClaimMismatch
		}
	}
	if err != nil {
		return claim, err
	}
	if !ok {
		return claim, exit(4, "Modal resource %q has no exact local ownership claim; use read-only status/list and inspect or clean up through Modal directly", id)
	}
	_, _, err = modalClaimBinding(claim)
	return claim, err
}

func (b *modalBackend) resolveOwnedSandbox(ctx context.Context, client modalAPI, id, repoRoot string, reclaim bool) (core.LeaseClaim, error) {
	claim, err := resolveModalClaim(id)
	if err != nil {
		return claim, err
	}
	binding, _, err := modalClaimBinding(claim)
	if err != nil {
		return claim, err
	}
	verify := func() error {
		sandbox, err := client.InspectSandbox(ctx, binding)
		if err != nil {
			return err
		}
		if err := binding.validate(sandbox); err != nil {
			return err
		}
		if sandbox.Status != "running" {
			return exit(2, "Modal sandbox has finished; create a new lease")
		}
		return nil
	}
	if repoRoot == "" {
		return claim, core.WithLeaseClaimUnchanged(claim.LeaseID, claim, verify)
	}
	idleTimeout := b.cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	server := Server{Provider: providerName, CloudID: claim.CloudID, Labels: shared.CloneLabels(claim.Labels)}
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(claim.LeaseID, claim.Slug, b.cfg, claim.ProviderScope, server, core.SSHTarget{}, repoRoot, idleTimeout, reclaim, claim, true, verify)
}

func removeModalClaim(ctx context.Context, client modalAPI, claim core.LeaseClaim) error {
	binding, want, err := modalClaimBinding(claim)
	if err != nil {
		return err
	}
	return shared.RemoveExactClaimAfter(claim, want, func() error {
		ctx, cancel := context.WithTimeout(ctx, modalCleanupTimeout)
		defer cancel()
		if err := client.Terminate(ctx, binding); err != nil {
			return fmt.Errorf("Modal termination is unconfirmed; retaining exact claim: %w", err)
		}
		return nil
	})
}

var publishModalClaim = func(b *modalBackend, ctx context.Context, client modalAPI, binding modalBinding, sandbox modalSandbox, repo Repo, reclaim bool) (core.LeaseClaim, error) {
	scope, err := binding.Scope.canonical()
	if err != nil {
		return core.LeaseClaim{}, err
	}
	server := Server{Provider: providerName, CloudID: sandbox.ID, Labels: shared.CloneLabels(sandbox.Tags)}
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(binding.LeaseID, binding.Slug, b.cfg, scope, server, core.SSHTarget{}, repo.Root, b.cfg.IdleTimeout, reclaim, core.LeaseClaim{}, false, func() error {
		current, err := client.InspectSandbox(ctx, binding)
		if err != nil {
			return err
		}
		if err := binding.validate(current); err != nil {
			return err
		}
		if current.Status != "running" {
			return exit(2, "Modal sandbox finished before ownership publication")
		}
		return nil
	})
}
