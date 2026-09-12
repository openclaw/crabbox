package sprites

import (
	"context"
	"flag"
	"fmt"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type spritesFlagValues struct {
	APIURL   *string
	WorkRoot *string
}

func RegisterSpritesProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return spritesFlagValues{
		APIURL:   fs.String("sprites-api-url", defaults.Sprites.APIURL, "Sprites API URL"),
		WorkRoot: fs.String("sprites-work-root", defaults.Sprites.WorkRoot, "Sprites remote work root"),
	}
}

func ApplySpritesProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == spritesProvider {
		if core.FlagWasSet(fs, "class") {
			return core.Exit(2, "--class is not supported for provider=sprites")
		}
		if core.FlagWasSet(fs, "type") {
			return core.Exit(2, "--type is not supported for provider=sprites")
		}
		if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
			return core.Exit(2, "provider=sprites supports target=linux only")
		}
		if err := validateSpritesOptions(*cfg); err != nil {
			return err
		}
	}
	v, ok := values.(spritesFlagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "sprites-api-url") {
		cfg.Sprites.APIURL = *v.APIURL
		core.RecordProviderFlagInputs(cfg, true, "sprites")
	}
	if core.FlagWasSet(fs, "sprites-work-root") {
		cfg.Sprites.WorkRoot = *v.WorkRoot
		core.RecordProviderFlagInputs(cfg, true, "sprites")
	}
	return nil
}

func NewSpritesBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := validateSpritesOptions(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = spritesProvider
	cfg.TargetOS = targetLinux
	cfg.SSHUser = "sprite"
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	cfg.Network = networkPublic
	if strings.TrimSpace(cfg.Sprites.WorkRoot) != "" {
		cfg.WorkRoot = cfg.Sprites.WorkRoot
	}
	if strings.TrimSpace(cfg.Sprites.Token) == "" {
		return nil, core.Exit(2, "provider=sprites requires SPRITES_TOKEN, SPRITE_TOKEN, SETUP_SPRITE_TOKEN, or CRABBOX_SPRITES_TOKEN")
	}
	client, err := newSpritesClient(cfg, rt)
	if err != nil {
		return nil, err
	}
	return &spritesBackend{spec: spec, cfg: cfg, rt: rt, client: client}, nil
}

func validateSpritesOptions(cfg core.Config) error {
	if cfg.Tailscale.Enabled {
		return core.Exit(2, "--tailscale is not supported for provider=sprites; Sprites exposes SSH through sprite proxy")
	}
	if err := cleanSpritesWorkRoot(cfg.Sprites.WorkRoot); err != nil {
		return err
	}
	return nil
}

type spritesBackend struct {
	spec   core.ProviderSpec
	cfg    core.Config
	rt     core.Runtime
	client spritesAPI
}

func (b *spritesBackend) Spec() core.ProviderSpec { return b.spec }

func (b *spritesBackend) RebindResolvedLeaseTarget(target *core.LeaseTarget, leaseID string) error {
	core.UseStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b *spritesBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	if err := b.ensureCLI(ctx); err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	name := core.LeaseProviderName(leaseID, slug)
	keyPath, publicKey, err := core.EnsureTestboxKey(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg := b.configForRun()
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=sprites lease=%s slug=%s sprite=%s keep=%v\n", leaseID, slug, name, req.Keep)
	sprite, err := b.client.CreateSprite(ctx, name, spritesAPILabels(leaseID, slug))
	if err != nil {
		return core.LeaseTarget{}, spritesError("create sprite", err)
	}
	if sprite.Name == "" {
		sprite.Name = name
	}
	claimed := false
	cleanupFailedAcquire := func() {
		if req.Keep {
			return
		}
		deleteSprite := func() error { return b.client.DeleteSprite(context.Background(), sprite.Name) }
		if claimed {
			binding := b.claimBinding(leaseID, slug, sprite.Name)
			claim, err := shared.RequireExactClaim(binding)
			if err != nil || shared.RemoveExactClaimAfter(claim, binding, deleteSprite) != nil {
				return
			}
		} else if deleteSprite() != nil {
			return
		}
		core.RemoveStoredTestboxKey(leaseID)
	}
	server := b.spriteToServer(sprite, req.Keep)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		cleanupFailedAcquire()
		return core.LeaseTarget{}, err
	}
	claimed = true
	lease, err := b.prepareLease(ctx, sprite, leaseID, slug, req.Keep, keyPath, publicKey)
	if err != nil {
		cleanupFailedAcquire()
		return core.LeaseTarget{}, err
	}
	if err := core.UpdateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
		cleanupFailedAcquire()
		return core.LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s sprite=%s state=ready\n", leaseID, sprite.Name)
	return lease, nil
}

func (b *spritesBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	name, leaseID, slug, err := b.resolveSpriteName(ctx, req.ID, req.Reclaim)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		sprite := spritesInfo{Name: name, Labels: spritesAPILabels(leaseID, slug)}
		return core.LeaseTarget{Server: b.spriteToServer(sprite, true), LeaseID: leaseID}, nil
	}
	sprite, err := b.client.GetSprite(ctx, name)
	if err != nil {
		return core.LeaseTarget{}, spritesError("get sprite", err)
	}
	if sprite.Name != name {
		return core.LeaseTarget{}, core.Exit(4, "sprite %q returned a different resource name %q", name, sprite.Name)
	}
	claim, hasClaim, err := core.ResolveLeaseClaim(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if hasClaim {
		if err := b.validateResolvedClaim(claim, sprite, req); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	adopted := hasClaim && claim.Labels["sprites_ownership"] == "adopted" &&
		claim.Labels["sprites_resource_id"] != "" && claim.Labels["sprites_resource_id"] == sprite.ID
	if !spriteHasExactOwnership(sprite, leaseID, slug) {
		if !req.Reclaim && !adopted {
			return core.LeaseTarget{}, core.Exit(4, "sprite %q has incomplete Crabbox ownership labels; use --reclaim to adopt it", sprite.Name)
		}
		if strings.TrimSpace(sprite.ID) == "" {
			return core.LeaseTarget{}, core.Exit(4, "refusing to adopt sprite %q without an immutable provider resource identity", sprite.Name)
		}
		adopted = true
	}
	target, err := b.sshTarget(name, "")
	if err != nil {
		return core.LeaseTarget{}, err
	}
	core.UseStoredTestboxKey(&target, leaseID)
	resolved := core.LeaseTarget{Server: b.spriteToServer(sprite, true), SSH: target, LeaseID: leaseID}
	resolved.Server.Labels["lease"], resolved.Server.Labels["slug"] = leaseID, slug
	if err := core.ValidateLeaseTargetProviderIdentity(resolved, req.ExpectedProviderIdentity); err != nil {
		return core.LeaseTarget{}, err
	}
	if req.StatusOnly || req.NoLocalStateMutations {
		return resolved, nil
	}
	if !hasClaim {
		if !req.Reclaim {
			return core.LeaseTarget{}, core.Exit(4, "sprite %q has no local ownership claim; use --reclaim to adopt it", sprite.Name)
		}
		if strings.TrimSpace(sprite.ID) == "" {
			return core.LeaseTarget{}, core.Exit(4, "refusing to adopt sprite %q without an immutable provider resource identity", sprite.Name)
		}
		adopted = true
	}
	if err := b.ensureCLI(ctx); err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKey(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, sprite, leaseID, slug, true, keyPath, publicKey)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if adopted {
			lease.Server.Labels["sprites_ownership"] = "adopted"
		}
		if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, b.configForRun(), lease.Server, lease.SSH, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *spritesBackend) validateResolvedClaim(claim core.LeaseClaim, sprite spritesInfo, req core.ResolveRequest) error {
	if err := shared.ValidateClaimBinding(claim, b.claimBinding(claim.LeaseID, claim.Slug, sprite.Name)); err != nil {
		return core.Exit(4, "sprite %q does not match its ownership claim: %v", sprite.Name, err)
	}
	if id := claim.Labels["sprites_resource_id"]; id != "" && id != sprite.ID {
		return core.Exit(4, "sprite %q immutable provider resource identity does not match its ownership claim", sprite.Name)
	}
	if org := claim.Labels["sprites_organization"]; org != "" && org != sprite.Organization {
		return core.Exit(4, "sprite %q organization does not match its ownership claim", sprite.Name)
	}
	if liveLease := spritesLeaseID(sprite); liveLease != "" && liveLease != claim.LeaseID {
		return core.Exit(4, "sprite %q belongs to a different live lease %q", sprite.Name, liveLease)
	}
	if req.Repo.Root != "" && claim.RepoRoot != "" && req.Repo.Root != claim.RepoRoot && !req.Reclaim {
		return core.Exit(4, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", claim.LeaseID, claim.RepoRoot, req.Repo.Root)
	}
	return nil
}

func (b *spritesBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	sprites, err := b.client.ListSprites(ctx, "crabbox-")
	if err != nil {
		return nil, spritesError("list sprites", err)
	}
	out := make([]core.Server, 0, len(sprites))
	for _, sprite := range sprites {
		if !isCrabboxSprite(sprite) {
			continue
		}
		out = append(out, b.spriteToServer(sprite, true))
	}
	return out, nil
}

func (b *spritesBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.InventoryDoctorResult(spritesProvider, len(servers)), nil
}

func (b *spritesBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	name := strings.TrimSpace(req.Lease.Server.Name)
	if name == "" {
		var err error
		name, _, _, err = b.resolveSpriteName(ctx, req.Lease.LeaseID, false)
		if err != nil {
			return err
		}
	}
	binding := b.claimBinding(req.Lease.LeaseID, req.Lease.Server.Labels["slug"], name)
	claim, err := shared.RequireExactClaim(binding)
	if err != nil {
		return err
	}
	if err := shared.RemoveExactClaimAfter(claim, binding, func() error {
		sprite, err := b.client.GetSprite(ctx, name)
		if err != nil {
			if isSpritesNotFound(err) {
				return b.confirmAbsentSprite(ctx, claim, name)
			}
			return spritesError("get sprite", err)
		}
		if sprite.Name != name {
			return core.Exit(4, "refusing to delete sprite %q: live sprite identity is %q", name, sprite.Name)
		}
		if resourceID := claim.Labels["sprites_resource_id"]; resourceID != "" && sprite.ID != resourceID {
			return core.Exit(4, "refusing to delete sprite %q: immutable provider resource identity does not match its ownership claim", name)
		}
		liveLeaseID := spritesLeaseID(sprite)
		if liveLeaseID != "" && liveLeaseID != req.Lease.LeaseID {
			return core.Exit(4, "refusing to delete sprite %q: live lease %q does not match %q", name, liveLeaseID, req.Lease.LeaseID)
		}
		if !spriteHasExactOwnership(sprite, req.Lease.LeaseID, claim.Slug) &&
			(claim.Labels["sprites_ownership"] != "adopted" || claim.Labels["sprites_resource_id"] == "") {
			return core.Exit(4, "refusing to delete sprite %q without exact Crabbox ownership labels or an explicitly adopted immutable identity", name)
		}
		if organization := claim.Labels["sprites_organization"]; organization != "" && sprite.Organization != organization {
			return core.Exit(4, "refusing to delete sprite %q: organization does not match its ownership claim", name)
		}
		if err := b.client.DeleteSprite(ctx, name); err != nil && !isSpritesNotFound(err) {
			return spritesError("delete sprite", err)
		}
		return nil
	}); err != nil {
		return err
	}
	core.RemoveStoredTestboxKey(req.Lease.LeaseID)
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sprite=%s\n", req.Lease.LeaseID, name)
	return nil
}

// A 404 alone cannot identify the account: another valid organization's token
// can see the same name as missing. Confirm the original account and recheck
// absence before removing only the exact fenced local claim and key.
func (b *spritesBackend) confirmAbsentSprite(ctx context.Context, claim core.LeaseClaim, name string) error {
	organization := claim.Labels["sprites_organization"]
	if organization == "" || claim.Labels["sprites_resource_id"] == "" {
		return core.Exit(4, "cannot confirm absent sprite %q without its original organization and immutable identity; preserving the local claim", name)
	}
	current, err := b.client.GetOrganization(ctx)
	if err != nil {
		return spritesError("confirm organization for absent sprite", err)
	}
	if current != organization {
		return core.Exit(4, "refusing local cleanup for sprite %q: organization does not match its ownership claim", name)
	}
	if _, err := b.client.GetSprite(ctx, name); isSpritesNotFound(err) {
		return nil
	} else if err != nil {
		return spritesError("confirm absent sprite", err)
	}
	return core.Exit(4, "sprite %q appeared during cleanup; preserving its local claim for revalidation", name)
}

func (b *spritesBackend) claimBinding(leaseID, slug, name string) shared.ClaimBinding {
	return shared.ClaimBinding{
		Provider:           spritesProvider,
		ProviderScope:      core.ProviderClaimScope(spritesProvider, b.configForRun()),
		ExactProviderScope: true,
		LeaseID:            leaseID,
		Slug:               slug,
		CloudID:            name,
		RequiredLabels:     map[string]string{"name": name},
	}
}

func spriteHasExactOwnership(sprite spritesInfo, leaseID, slug string) bool {
	for _, expected := range spritesAPILabels(leaseID, slug) {
		found := false
		for _, label := range sprite.Labels {
			if label == expected {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (b *spritesBackend) Touch(_ context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.cfg, req.State, time.Now().UTC())
	return server, nil
}

func (b *spritesBackend) configForRun() core.Config {
	cfg := b.cfg
	cfg.Provider = spritesProvider
	cfg.TargetOS = targetLinux
	cfg.SSHUser = "sprite"
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	cfg.Network = networkPublic
	if strings.TrimSpace(cfg.Sprites.WorkRoot) != "" {
		cfg.WorkRoot = cfg.Sprites.WorkRoot
	}
	return cfg
}

func (b *spritesBackend) prepareLease(ctx context.Context, sprite spritesInfo, leaseID, slug string, keep bool, keyPath, publicKey string) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	if err := cleanSpritesWorkRoot(cfg.WorkRoot); err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.bootstrapSSH(ctx, sprite.Name, publicKey); err != nil {
		return core.LeaseTarget{}, err
	}
	target, err := b.sshTarget(sprite.Name, keyPath)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	server := b.spriteToServer(sprite, keep)
	server.Labels["lease"] = leaseID
	server.Labels["slug"] = slug
	server.Labels["keep"] = fmt.Sprint(keep)
	server.Labels["work_root"] = cfg.WorkRoot
	server.Labels["state"] = "ready"
	server.Status = "ready"
	if err := core.WaitForSSHReady(ctx, &target, b.rt.Stderr, "sprites ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *spritesBackend) bootstrapSSH(ctx context.Context, spriteName, publicKey string) error {
	script := strings.Join([]string{
		"set -euo pipefail",
		"if [ ! -x /usr/sbin/sshd ] || ! command -v git >/dev/null 2>&1 || ! command -v rsync >/dev/null 2>&1 || ! command -v tar >/dev/null 2>&1 || ! command -v python3 >/dev/null 2>&1; then sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server git rsync tar python3; fi",
		"mkdir -p ~/.ssh",
		"chmod 700 ~/.ssh",
		"touch ~/.ssh/authorized_keys",
		"grep -qxF \"$CRABBOX_SSH_PUBLIC_KEY\" ~/.ssh/authorized_keys || printf '%s\\n' \"$CRABBOX_SSH_PUBLIC_KEY\" >> ~/.ssh/authorized_keys",
		"chmod 600 ~/.ssh/authorized_keys",
		"sudo mkdir -p /run/sshd",
		"sudo /usr/sbin/sshd || true",
		"if command -v sprite-env >/dev/null 2>&1; then sprite-env services create sshd --cmd /usr/sbin/sshd || true; sprite-env services start sshd || true; fi",
		"command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
	}, "\n")
	result, err := b.runSprite(ctx, []string{"exec", "-s", spriteName, "--env", "CRABBOX_SSH_PUBLIC_KEY=" + publicKey, "--", "/bin/bash", "-lc", script}, nil, b.rt.Stderr)
	if err != nil {
		return core.ExitError{Code: result.ExitCode, Message: fmt.Sprintf("sprites ssh bootstrap failed: %v", err)}
	}
	return nil
}

func (b *spritesBackend) ensureCLI(ctx context.Context) error {
	result, err := b.runSprite(ctx, []string{"--version"}, nil, nil)
	if err != nil {
		return core.ExitError{Code: result.ExitCode, Message: fmt.Sprintf("provider=sprites requires the sprite CLI on PATH: %v", err)}
	}
	return nil
}

func (b *spritesBackend) resolveSpriteName(ctx context.Context, identifier string, reclaim bool) (string, string, string, error) {
	if strings.TrimSpace(identifier) == "" {
		return "", "", "", core.Exit(2, "provider=sprites requires a Crabbox lease id, slug, or Sprite name")
	}
	if claim, ok, err := core.ResolveLeaseClaim(identifier); err != nil {
		return "", "", "", err
	} else if ok {
		if claim.Provider != "" && claim.Provider != spritesProvider {
			return "", "", "", core.Exit(4, "lease %q is claimed for provider=%s, not sprites", identifier, claim.Provider)
		}
		if name, ok := spriteNameFromClaim(claim); ok {
			return name, claim.LeaseID, claim.Slug, nil
		}
	}
	if strings.HasPrefix(identifier, "cbx_") {
		sprite, err := b.findSpriteByLease(ctx, identifier)
		if err != nil {
			return "", "", "", err
		}
		return sprite.Name, identifier, spritesSlug(identifier, sprite), nil
	}
	spriteIdentifier := identifier
	if strings.HasPrefix(spriteIdentifier, "spr_") {
		spriteIdentifier = strings.TrimPrefix(spriteIdentifier, "spr_")
	}
	// Preserve an existing immutable binding even if a raw-name lookup has lost
	// its lease labels or now returns a same-name replacement.
	if claim, ok, err := core.ResolveLeaseClaimForProviderCloudIDScope(spriteIdentifier, spritesProvider, core.ProviderClaimScope(spritesProvider, b.cfg)); err != nil {
		return "", "", "", err
	} else if ok {
		return spriteIdentifier, claim.LeaseID, claim.Slug, nil
	}
	if sprite, err := b.client.GetSprite(ctx, spriteIdentifier); err == nil {
		if !isCrabboxSprite(sprite) && !reclaim {
			if isLegacyCrabboxSpriteName(sprite) {
				return "", "", "", core.Exit(4, "sprite %q uses a legacy Crabbox name but has no Crabbox labels; use --reclaim to adopt it", spriteIdentifier)
			}
			return "", "", "", core.Exit(4, "sprite %q is not Crabbox-managed; use --reclaim to adopt it", spriteIdentifier)
		}
		leaseID := spritesLeaseID(sprite)
		if leaseID == "" {
			leaseID = "spr_" + core.NormalizeLeaseSlug(sprite.Name)
		}
		return sprite.Name, leaseID, spritesSlug(leaseID, sprite), nil
	} else if !isSpritesNotFound(err) {
		return "", "", "", spritesError("get sprite", err)
	}
	return "", "", "", core.Exit(4, "sprites lease or sprite %q was not found", identifier)
}

func spriteNameFromClaim(claim core.LeaseClaim) (string, bool) {
	if name := strings.TrimSpace(claim.CloudID); name != "" {
		return name, true
	}
	if strings.HasPrefix(claim.LeaseID, "spr_") {
		return strings.TrimPrefix(claim.LeaseID, "spr_"), true
	}
	if strings.HasPrefix(claim.LeaseID, "cbx_") {
		return core.LeaseProviderName(claim.LeaseID, claim.Slug), true
	}
	return "", false
}

func (b *spritesBackend) findSpriteByLease(ctx context.Context, leaseID string) (spritesInfo, error) {
	sprites, err := b.client.ListSprites(ctx, "crabbox-")
	if err != nil {
		return spritesInfo{}, spritesError("list sprites", err)
	}
	for _, sprite := range sprites {
		if spritesLeaseID(sprite) == leaseID {
			return sprite, nil
		}
	}
	return spritesInfo{}, core.Exit(4, "sprites lease %q was not found", leaseID)
}

func (b *spritesBackend) spriteToServer(sprite spritesInfo, keep bool) core.Server {
	leaseID := spritesLeaseID(sprite)
	slug := spritesSlug(leaseID, sprite)
	cfg := b.configForRun()
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, spritesProvider, "", keep, time.Now().UTC())
	labels["name"] = sprite.Name
	labels["state"] = spritesState(sprite.Status)
	labels["work_root"] = cfg.WorkRoot
	if sprite.ID != "" {
		labels["sprites_resource_id"] = sprite.ID
	}
	if sprite.Organization != "" {
		labels["sprites_organization"] = sprite.Organization
	}
	if sprite.URL != "" {
		labels["url"] = sprite.URL
	}
	server := core.Server{
		CloudID:  sprite.Name,
		Provider: spritesProvider,
		Name:     sprite.Name,
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = "sprite"
	server.PublicNet.IPv4.IP = sprite.Name
	return server
}

func spritesSSHTarget(name, keyPath string) core.SSHTarget {
	return core.SSHTarget{
		User:           "sprite",
		Host:           name,
		Key:            keyPath,
		Port:           "22",
		TargetOS:       targetLinux,
		NetworkKind:    networkPublic,
		SSHConfigProxy: true,
		ProxyCommand:   "sprite proxy -s %h -W 22",
		ReadyCheck:     "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
	}
}

func spritesState(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return "ready"
	}
	return status
}

func cleanSpritesWorkRoot(workRoot string) error {
	clean := path.Clean(strings.TrimSpace(workRoot))
	if clean == "" || !strings.HasPrefix(clean, "/") {
		return core.Exit(2, "sprites.workRoot %q must resolve to an absolute path", workRoot)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/home/sprite", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return core.Exit(2, "sprites.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}
