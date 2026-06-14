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
)

const (
	codeSandboxCleanupTimeout = 30 * time.Second
	codeSandboxExecTimeout    = 3600
	codeSandboxNamePrefix     = "crabbox-"
	codeSandboxClaimTag       = "crabbox"
	codeSandboxScopeTagPrefix = "crabbox-scope:"
)

func (b *codeSandboxBackend) createSandbox(ctx context.Context, api codeSandboxAPI, repo Repo, reclaim bool, requestedSlug string) (string, string, string, error) {
	scope, err := newCodeSandboxClaimScope()
	if err != nil {
		return "", "", "", err
	}
	sb, err := api.CreateSandbox(ctx, CreateSandboxRequest{
		Title:                  newSandboxTitle(repo),
		Tags:                   []string{codeSandboxClaimTag, codeSandboxScopeTagPrefix + scope},
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
		return "", "", "", exit(5, "codesandbox create returned an empty sandbox id")
	}
	leaseID := leasePrefix + sb.ID
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return leaseID, sb.ID, "", b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, scope, b.cfg.Pond, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return leaseID, sb.ID, slug, b.cleanupCreateFailure(ctx, api, sb.ID, err)
	}
	return leaseID, sb.ID, slug, nil
}

func resolveLeaseID(id string) (string, string, string, LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", LeaseClaim{}, exit(2, "provider=codesandbox requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := id
	if !strings.HasPrefix(exactLeaseID, leasePrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := readLeaseClaim(exactLeaseID); err != nil {
		return "", "", "", LeaseClaim{}, err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return finishResolvedLease(claim)
	}
	claim, ok, err := resolveCodeSandboxLeaseClaim(id)
	if err != nil {
		return "", "", "", LeaseClaim{}, err
	}
	if ok {
		return finishResolvedLease(claim)
	}
	return "", "", "", LeaseClaim{}, exit(4, "codesandbox sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", id, leasePrefix)
}

func resolveCodeSandboxLeaseClaim(identifier string) (LeaseClaim, bool, error) {
	claims, err := listCodeSandboxLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID == identifier {
			if err := validateCodeSandboxClaimScope(claim); err != nil {
				return LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := normalizeLeaseSlug(identifier)
	if slug != "" {
		for _, claim := range claims {
			if claim.Provider == providerName && normalizeLeaseSlug(claim.Slug) == slug {
				if err := validateCodeSandboxClaimScope(claim); err != nil {
					return LeaseClaim{}, false, err
				}
				return claim, true, nil
			}
		}
	}
	return LeaseClaim{}, false, nil
}

func finishResolvedLease(claim LeaseClaim) (string, string, string, LeaseClaim, error) {
	if err := validateCodeSandboxClaimScope(claim); err != nil {
		return "", "", "", LeaseClaim{}, err
	}
	sandboxID := strings.TrimPrefix(claim.LeaseID, leasePrefix)
	if sandboxID == "" {
		return "", "", "", LeaseClaim{}, exit(4, "codesandbox lease %q has no provider sandbox id", claim.LeaseID)
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = newLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, sandboxID, slug, claim, nil
}

func validateCodeSandboxClaimScope(claim LeaseClaim) error {
	if claim.Provider != providerName || !strings.HasPrefix(claim.LeaseID, leasePrefix) {
		return exit(4, "codesandbox lease %q is not a CodeSandbox Crabbox claim", claim.LeaseID)
	}
	if !strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), "codesandbox/ownership:") {
		return exit(4, "codesandbox lease %q has an invalid ownership scope", claim.LeaseID)
	}
	return nil
}

func validateCodeSandboxSandboxOwnership(claim LeaseClaim, sb SandboxSummary) error {
	if strings.TrimSpace(sb.ID) != "" && strings.TrimSpace(sb.ID) != strings.TrimPrefix(claim.LeaseID, leasePrefix) {
		return exit(4, "codesandbox sandbox %q does not match local claim %q", sb.ID, claim.LeaseID)
	}
	remoteScope := ""
	for _, tag := range sb.Tags {
		if strings.HasPrefix(tag, codeSandboxScopeTagPrefix) {
			remoteScope = strings.TrimPrefix(tag, codeSandboxScopeTagPrefix)
			break
		}
	}
	if remoteScope != "" && remoteScope != claim.ProviderScope {
		return exit(4, "codesandbox sandbox %q ownership tag does not match its local claim", sb.ID)
	}
	return nil
}

func newCodeSandboxClaimScope() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", exit(5, "generate codesandbox ownership token: %v", err)
	}
	return "codesandbox/ownership:" + hex.EncodeToString(token[:]), nil
}

func newSandboxTitle(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
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
	return codeSandboxNamePrefix + base + "-" + randomSuffix()
}

func codeSandboxWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.CodeSandbox.Workdir)
	if workdir == "" {
		workdir = defaultWorkdir
	}
	if strings.IndexFunc(workdir, func(r rune) bool { return r == 0 || (!utf8.ValidRune(r)) || (r < 0x20) }) >= 0 {
		return "", exit(2, "codesandbox workdir contains control characters")
	}
	clean := path.Clean(workdir)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "codesandbox workdir %q must be an absolute path", workdir)
	}
	switch clean {
	case "/", "/project":
		return "", exit(2, "codesandbox workdir %q is too broad; choose a path under %s", clean, defaultWorkdir)
	}
	if clean != defaultWorkdir && !strings.HasPrefix(clean, defaultWorkdir+"/") {
		return "", exit(2, "codesandbox workdir %q must be under %s", clean, defaultWorkdir)
	}
	return clean, nil
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "", "ready", "running", "started", "active", "awake":
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

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}
	if shellMode {
		return []string{"bash", "-lc", strings.Join(command, " ")}, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		if len(command) == 1 {
			return []string{"bash", "-lc", command[0]}, nil
		}
		return []string{"bash", "-lc", shellScriptFromArgv(command)}, nil
	}
	return command, nil
}

func leadingEnvAssignment(command []string) bool {
	return len(command) > 1 && strings.Contains(command[0], "=") && !strings.HasPrefix(command[0], "-")
}

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}

func (b *codeSandboxBackend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
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
