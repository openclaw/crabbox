package cloudflare

import (
	"context"
	"flag"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

var checkpointIDPattern = regexp.MustCompile(`^chk_[a-f0-9]{16}$`)

var (
	_ core.NativeCheckpointProvider          = Provider{}
	_ core.NativeCheckpointLifecycleProvider = Provider{}
	_ core.NativeCheckpointForkProvider      = Provider{}
	_ core.NativeCheckpointForkFlagProvider  = Provider{}
	_ core.DelegatedCheckpointBackend        = (*cloudflareBackend)(nil)
)

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if targetOS := strings.TrimSpace(req.Config.TargetOS); targetOS != "" && targetOS != core.TargetLinux {
		return core.NativeCheckpointCapability{}, false
	}
	if req.Server.CloudID == "" {
		return core.NativeCheckpointCapability{}, false
	}
	// A container snapshot is a filesystem snapshot; there is no image strategy.
	if strings.EqualFold(strings.TrimSpace(req.Strategy), core.CheckpointStrategyImage) {
		return core.NativeCheckpointCapability{}, false
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindCloudflare, Direct: true}, true
}

func (Provider) NativeCheckpointWorkdir(req core.NativeCheckpointWorkdirRequest) string {
	if req.Override != "" {
		return req.Override
	}
	if workdir := strings.TrimSpace(req.Server.Labels["workdir"]); workdir != "" {
		return workdir
	}
	workdir, err := cloudflareWorkdir(req.Config)
	if err != nil {
		return core.CloudflareConfigDefaultWorkdir
	}
	return workdir
}

// CreateNativeCheckpoint captures the lease's full container filesystem. The
// snapshot is tied to the runner image version it was taken from.
func (Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (core.NativeCheckpointCreateResult, error) {
	var result core.NativeCheckpointCreateResult
	if _, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Config: req.Config, Server: req.Server}); !ok {
		return result, core.NativeCheckpointNotSubmittedError{Cause: core.Exit(2, "%s native checkpoints require a Linux container lease", providerName)}
	}
	if !checkpointIDPattern.MatchString(req.CheckpointID) {
		return result, core.NativeCheckpointNotSubmittedError{Cause: core.Exit(2, "%s snapshot requires a canonical checkpoint ID", providerName)}
	}
	claim, err := resolveCloudflareClaim(req.LeaseID)
	if err != nil {
		return result, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	if err := requireResolvedCheckpointClaim(claim, req.Server); err != nil {
		return result, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	client, err := newCloudflareClient(leaseConfig(req.Config, claim), core.RuntimeForProviderOperation(nil))
	if err != nil {
		return result, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	client.useInstanceType(cloudflareClaimInstanceType(claim))
	name := "crabbox-" + req.CheckpointID
	var snapshot containerSnapshot
	err = core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
		var createErr error
		snapshot, createErr = client.createSnapshot(ctx, claim.LeaseID, name)
		return createErr
	})
	if err != nil {
		return result, err
	}
	if snapshot.ID == "" || snapshot.LeaseID != claim.LeaseID {
		return result, fmt.Errorf("%s runner returned snapshot %q for lease %q; want lease %q", providerName, snapshot.ID, snapshot.LeaseID, claim.LeaseID)
	}
	result.Image = core.NativeCheckpointImage{
		ID: snapshot.ID, Name: core.Blank(snapshot.Name, name), State: "available",
		Provider: providerName, Kind: core.CheckpointKindCloudflare, ResourceID: snapshot.ID, Direct: true,
	}
	result.Metadata = map[string]string{
		"api_url":       client.baseURL,
		"source":        claim.LeaseID,
		"image":         snapshot.Image,
		"instance_type": snapshot.InstanceType,
		"workdir":       snapshot.Workdir,
		"size_bytes":    strconv.FormatInt(snapshot.Size, 10),
	}
	return result, nil
}

// VerifyNativeCheckpoint cannot query the runner: Cloudflare exposes no
// snapshot lookup, and snapshots expire 30 days after creation or last restore.
func (Provider) VerifyNativeCheckpoint(_ context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	if _, err := cloudflareCheckpointConfig(req.LoadConfig, req.Image, req.Metadata); err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	return core.NativeCheckpointVerifyResult{ProviderState: "unverified_ref", NextAction: "fork_or_delete_local"}, nil
}

func (Provider) DeleteNativeCheckpoint(_ context.Context, req core.NativeCheckpointResourceRequest) error {
	if _, err := cloudflareCheckpointConfig(req.LoadConfig, req.Image, req.Metadata); err != nil {
		return err
	}
	return core.Exit(2, "%s has no snapshot delete API; snapshot %s expires 30 days after creation or last restore. Remove the local record with crabbox checkpoint delete --local-only", providerName, req.Image.ID)
}

// ApplyNativeCheckpointForkConfig leaves the runner check to
// ForkNativeCheckpoint: fork selects this provider from the record after
// flags such as --cloudflare-url were applied for the configured provider, so
// they only take effect in ApplyNativeCheckpointForkFlags.
func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	if req.Record.Kind != core.CheckpointKindCloudflare || strings.TrimSpace(req.Record.ImageID) == "" {
		return core.Exit(2, "%s checkpoint record is not a container snapshot", providerName)
	}
	return nil
}

func (Provider) ApplyNativeCheckpointForkFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	_, err := core.ApplyProviderConfigFlags[core.CloudflareConfigFlagValues](cfg, fs, values, &cfg.Cloudflare, providerName)
	return err
}

// cloudflareCheckpointConfig binds a checkpoint to the runner that captured it;
// snapshot IDs are only meaningful on that runner's Cloudflare account.
func cloudflareCheckpointConfig(load func() (core.Config, error), image core.NativeCheckpointImage, metadata map[string]string) (core.Config, error) {
	if image.Kind != core.CheckpointKindCloudflare || strings.TrimSpace(image.ID) == "" {
		return core.Config{}, core.Exit(2, "%s checkpoint record is not a container snapshot", providerName)
	}
	if load == nil {
		return core.Config{}, core.Exit(2, "%s checkpoint requires provider configuration", providerName)
	}
	cfg, err := load()
	if err != nil {
		return core.Config{}, err
	}
	cfg.Provider = providerName
	client, err := newCloudflareClient(cfg, core.RuntimeForProviderOperation(nil))
	if err != nil {
		return core.Config{}, err
	}
	if err := requireCheckpointRunner(metadata, client.baseURL); err != nil {
		return core.Config{}, err
	}
	return cfg, nil
}

func requireCheckpointRunner(metadata map[string]string, runner string) error {
	if recorded := strings.TrimSpace(metadata["api_url"]); recorded != runner {
		return core.Exit(2, "%s checkpoint was captured on runner %s, but the configured runner is %s", providerName, core.Blank(recorded, "-"), runner)
	}
	return nil
}

func (b *cloudflareBackend) ResolveCheckpointSource(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	claim, err := resolveCloudflareClaim(req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" && claim.RepoRoot != req.Repo.Root {
		return core.LeaseTarget{}, core.Exit(2, "%s lease %s is claimed by %s, not this repository", providerName, claim.LeaseID, core.Blank(claim.RepoRoot, "another repository"))
	}
	client, err := b.leaseClient(claim)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	sandbox, err := client.getSandbox(ctx, claim.LeaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !cloudflareReady(sandbox.State) {
		return core.LeaseTarget{}, core.Exit(2, "%s lease %s is %s; checkpoints need a running container", providerName, claim.LeaseID, core.Blank(sandbox.State, "unknown"))
	}
	server := sandboxToServer(claim.LeaseID, claim.Slug, sandbox)
	server.Labels["workdir"] = sandbox.Workdir
	server.Labels[checkpointClaimRevisionLabel] = claim.Revision
	server.Labels[checkpointClaimRepoLabel] = claim.RepoRoot
	return core.LeaseTarget{Server: server, LeaseID: claim.LeaseID}, nil
}

// Source resolution authorizes one claim for this repository. Capture re-reads
// the claim and only proceeds while it is that exact revision, so a lease
// reclaimed by another repository in between is never snapshotted.
const (
	checkpointClaimRevisionLabel = "checkpoint_claim_revision"
	checkpointClaimRepoLabel     = "checkpoint_claim_repo"
)

func requireResolvedCheckpointClaim(claim core.LeaseClaim, server core.Server) error {
	revision, resolved := server.Labels[checkpointClaimRevisionLabel]
	if !resolved {
		return core.Exit(2, "%s checkpoint source %s was not resolved for this repository", providerName, claim.LeaseID)
	}
	if claim.Revision != revision || claim.RepoRoot != server.Labels[checkpointClaimRepoLabel] {
		return core.Exit(2, "%s lease %s claim changed after it was resolved; rerun crabbox checkpoint create", providerName, claim.LeaseID)
	}
	return nil
}

func (b *cloudflareBackend) ForkNativeCheckpoint(ctx context.Context, req core.DelegatedCheckpointForkRequest) (core.DelegatedCheckpointFork, error) {
	if req.Record.Kind != core.CheckpointKindCloudflare || strings.TrimSpace(req.Record.ImageID) == "" {
		return core.DelegatedCheckpointFork{}, core.Exit(2, "%s can only fork container snapshot checkpoints", providerName)
	}
	client, err := newCloudflareClient(b.cfg, b.rt)
	if err != nil {
		return core.DelegatedCheckpointFork{}, err
	}
	if err := requireCheckpointRunner(req.Record.Metadata, client.baseURL); err != nil {
		return core.DelegatedCheckpointFork{}, err
	}
	claim, sandbox, err := b.createSandbox(ctx, client, req.Repo, req.RequestedSlug, sandboxSource{
		snapshotID: req.Record.ImageID,
		workdir:    core.Blank(strings.TrimSpace(req.Workdir), req.Record.Metadata["workdir"]),
	})
	if err != nil {
		return core.DelegatedCheckpointFork{}, err
	}
	server := sandboxToServer(claim.LeaseID, claim.Slug, sandbox)
	return core.DelegatedCheckpointFork{
		Lease:   core.LeaseTarget{Server: server, LeaseID: claim.LeaseID},
		Workdir: sandbox.Workdir,
	}, nil
}
