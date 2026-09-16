package codesandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	codeSandboxCleanupTimeout   = 30 * time.Second
	codeSandboxExecTimeout      = 3600
	codeSandboxNamePrefix       = "crabbox-"
	codeSandboxClaimTag         = "crabbox"
	codeSandboxClaimScopePrefix = "codesandbox/ownership:"
	codeSandboxScopeTagPrefix   = "crabboxscope"
)

func (b *codeSandboxBackend) createSandbox(ctx context.Context, api codeSandboxAPI, repo core.Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	scope, err := newCodeSandboxClaimScope()
	if err != nil {
		return "", "", "", err
	}
	sb, err := api.CreateSandbox(ctx, CreateSandboxRequest{
		Title:                  newSandboxTitle(repo),
		Tags:                   []string{codeSandboxClaimTag, codeSandboxScopeTag(scope)},
		TemplateID:             strings.TrimSpace(b.cfg.CodeSandbox.TemplateID),
		Privacy:                strings.TrimSpace(b.cfg.CodeSandbox.Privacy),
		VMTier:                 strings.TrimSpace(b.cfg.CodeSandbox.VMTier),
		HibernationTimeoutSecs: b.cfg.CodeSandbox.HibernationTimeoutSecs,
		AutomaticWakeupHTTP:    b.cfg.CodeSandbox.AutomaticWakeupHTTP,
		AutomaticWakeupWS:      b.cfg.CodeSandbox.AutomaticWakeupWebSocket,
	})
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(sb.ID) == "" {
		return "", "", "", core.Exit(5, "codesandbox create returned an empty sandbox id")
	}
	leaseID := leasePrefix + sb.ID
	slug, err := core.AllocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return leaseID, sb.ID, "", b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, providerName, scope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, sb.ID, slug, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	return leaseID, sb.ID, slug, nil
}

func resolveLeaseID(id string) (string, string, string, core.LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", core.LeaseClaim{}, core.Exit(2, "provider=codesandbox requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := core.ReadLeaseClaim(exactLeaseID); err != nil {
		return "", "", "", core.LeaseClaim{}, err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return finishResolvedLease(claim)
	}
	claim, ok, err := resolveCodeSandboxLeaseClaim(id)
	if err != nil {
		return "", "", "", core.LeaseClaim{}, err
	}
	if ok {
		return finishResolvedLease(claim)
	}
	return "", "", "", core.LeaseClaim{}, core.Exit(4, "codesandbox sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func resolveCodeSandboxLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return shared.ResolveScopedLeaseClaim(identifier, providerName, listCodeSandboxLeaseClaims, validateCodeSandboxClaimScope)
}

func finishResolvedLease(claim core.LeaseClaim) (string, string, string, core.LeaseClaim, error) {
	if err := validateCodeSandboxClaimScope(claim); err != nil {
		return "", "", "", core.LeaseClaim{}, err
	}
	sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
	if sandboxID == "" {
		return "", "", "", core.LeaseClaim{}, core.Exit(4, "codesandbox lease %q has no provider sandbox id", claim.LeaseID)
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = core.NewLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, sandboxID, slug, claim, nil
}

func validateCodeSandboxClaimScope(claim core.LeaseClaim) error {
	if claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
		return core.Exit(4, "codesandbox lease %q is not a CodeSandbox Crabbox claim", claim.LeaseID)
	}
	if !strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), codeSandboxClaimScopePrefix) {
		return core.Exit(4, "codesandbox lease %q has an invalid ownership scope", claim.LeaseID)
	}
	return nil
}

func validateCodeSandboxSandboxOwnership(claim core.LeaseClaim, sb SandboxSummary) error {
	if strings.TrimSpace(sb.ID) != "" && strings.TrimSpace(sb.ID) != strings.TrimPrefix(claim.LeaseID, leasePrefix) {
		return core.Exit(4, "codesandbox sandbox %q does not match local claim %q", sb.ID, claim.LeaseID)
	}
	remoteScope := ""
	for _, tag := range sb.Tags {
		if scope, ok := codeSandboxScopeFromTag(tag); ok {
			remoteScope = scope
			break
		}
	}
	if remoteScope == "" {
		return core.Exit(4, "codesandbox sandbox %q is missing its Crabbox ownership tag", sb.ID)
	}
	if remoteScope != claim.ProviderScope {
		return core.Exit(4, "codesandbox sandbox %q ownership tag does not match its local claim", sb.ID)
	}
	return nil
}

func codeSandboxScopeTag(scope string) string {
	return codeSandboxScopeTagPrefix + strings.TrimPrefix(scope, codeSandboxClaimScopePrefix)
}

func codeSandboxScopeFromTag(tag string) (string, bool) {
	if !strings.HasPrefix(tag, codeSandboxScopeTagPrefix) {
		return "", false
	}
	token := strings.TrimPrefix(tag, codeSandboxScopeTagPrefix)
	if token == "" {
		return "", false
	}
	return codeSandboxClaimScopePrefix + token, true
}

func newCodeSandboxClaimScope() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", core.Exit(5, "generate codesandbox ownership token: %v", err)
	}
	return codeSandboxClaimScopePrefix + hex.EncodeToString(token[:]), nil
}

func newSandboxTitle(repo core.Repo) string {
	base := core.NormalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "workspace"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(codeSandboxNamePrefix, "-")+"-")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	if base == "" {
		base = "workspace"
	}
	return codeSandboxNamePrefix + base + "-" + shared.RandomSuffix()
}

func codeSandboxWorkdir(cfg core.Config) (string, error) {
	workdir := strings.TrimSpace(cfg.CodeSandbox.Workdir)
	if workdir == "" {
		workdir = defaultWorkdir
	}
	if strings.IndexFunc(workdir, func(r rune) bool { return r == 0 || (!utf8.ValidRune(r)) || (r < 0x20) }) >= 0 {
		return "", core.Exit(2, "codesandbox workdir contains control characters")
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", core.Exit(2, "codesandbox workdir %q must be an absolute path", workdir)
	}
	switch clean {
	case "/", "/project":
		return "", core.Exit(2, "codesandbox workdir %q is too broad; choose a path under %s", clean, defaultWorkdir)
	}
	if clean != defaultWorkdir && !strings.HasPrefix(clean, defaultWorkdir+"/") {
		return "", core.Exit(2, "codesandbox workdir %q must be under %s", clean, defaultWorkdir)
	}
	return clean, nil
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "ready", "running", "started", "active", "awake":
		return true
	default:
		return false
	}
}

func isTerminalState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "deleted", "deleting", "failed", "error", "stopped", "terminated":
		return true
	default:
		return false
	}
}

func (b *codeSandboxBackend) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), codeSandboxCleanupTimeout)
}

func (b *codeSandboxBackend) cleanupCreateFailure(ctx context.Context, api codeSandboxAPI, sandboxID string, cause error) error {
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	if err := api.DeleteSandbox(cleanupCtx, sandboxID); err != nil {
		return errors.Join(cause, fmt.Errorf("codesandbox cleanup failed for sandbox %s; delete it in the CodeSandbox console: %w", sandboxID, err))
	}
	return cause
}

func (b *codeSandboxBackend) execTimeoutSecs() int {
	return codeSandboxExecTimeout
}
