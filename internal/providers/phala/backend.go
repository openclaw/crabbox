package phala

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "phala"

// defaultComposeYAML is the Compose file crabbox supplies when the lease has no
// configured compose. The Phala CLI v1.1.19 deploy handler refuses to provision
// a CVM in non-interactive mode without a Compose file, so a confidential
// SSH-lease box needs a minimal long-lived service: a small base image whose
// container stays alive so the dstack dev OS keeps the CVM running while crabbox
// drives it over SSH.
//
//go:embed default-compose.yml
var defaultComposeYAML string

// defaultComposeFileName is the basename written into the per-lease temp dir for
// the embedded default compose.
const defaultComposeFileName = "crabbox-default-compose.yml"

const phalaRollbackTimeout = 30 * time.Second
const phalaAmbiguousCreateRecoveryGrace = 5 * time.Minute

// defaultInstanceType is the smallest confidential TDX shape Phala Cloud
// advertises. dstack provisions Intel TDX CVMs; tdx.small is the cheapest.
const defaultInstanceType = "tdx.small"

// defaultWorkRoot is the remote crabbox work root on a leased CVM. The dstack
// --dev-os guest mounts its root as a read-only squashfs, so the work root must
// live on a writable mount; /var/volatile is a writable tmpfs present on every
// dstack guest. The earlier /work/crabbox default sat on the read-only root and
// failed live at "write sync manifests: exit status 1" (the manifest mkdir).
const defaultWorkRoot = "/var/volatile/crabbox"

// crabboxCVMNamePrefix marks Phala CVMs created by crabbox. Phala's deploy CLI
// has no arbitrary label facility, so ownership is carried by the CVM name and
// cross-checked against the local lease claim. Underscores are not accepted in
// CVM names, so the lease id's underscores are normalized to dashes here.
const crabboxCVMNamePrefix = "crabbox-"

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

// instance models the subset of `phala cvms list/get` JSON that crabbox needs.
// Phala exposes several identifiers for one CVM; AppID is the canonical handle
// passed back to ssh/get/delete (a real live run confirmed `cvms get/delete
// --cvm-id <app_id>` works, with name and vm_uuid also accepted).
//
// The live `phala cvms list --json` payload emits camelCase keys (appId,
// vmUuid, instanceId, name, status), while `phala cvms get --json` emits
// snake_case (app_id, vm_uuid, instance_id). instance therefore reads BOTH
// spellings of every identifier via a custom unmarshaler.
type instance struct {
	ID           string
	VMUUID       string
	AppID        string
	InstanceID   string
	Name         string
	Status       string
	InstanceType string
	Node         string
	NodeID       string
	Region       string
	CreatedAt    string

	// Labels are synthesized locally from the CVM name and the matching lease
	// claim; Phala does not store crabbox ownership labels on the resource.
	Labels map[string]string
}

// UnmarshalJSON decodes a CVM list/get item tolerant of BOTH snake_case (the
// `cvms get` shape) and camelCase (the `cvms list` shape) keys for every
// identifier. Where both spellings are present the first non-blank wins, so a
// payload mixing the two never drops a field.
func (i *instance) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	str := func(keys ...string) string {
		for _, key := range keys {
			value, ok := raw[key]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(value, &s); err == nil && strings.TrimSpace(s) != "" {
				return s
			}
		}
		return ""
	}
	i.ID = str("id")
	i.VMUUID = str("vm_uuid", "vmUuid")
	i.AppID = str("app_id", "appId")
	i.InstanceID = str("instance_id", "instanceId")
	i.Name = str("name", "appName")
	i.Status = str("status")
	i.InstanceType = str("instance_type", "instanceType")
	i.Node = str("node")
	i.NodeID = str("node_id", "nodeId")
	i.Region = str("region")
	i.CreatedAt = str("created_at", "createdAt")
	return nil
}

// cloudID is the canonical handle crabbox passes back to ssh/get/delete. The
// app_id is preferred (confirmed working against live `cvms get/delete
// --cvm-id`); vm_uuid, instance_id, and name are accepted fallbacks.
func (i instance) cloudID() string {
	return firstNonBlank(i.AppID, i.VMUUID, i.ID, i.InstanceID, i.Name)
}

// matchesID reports whether identifier names this CVM under any of the handles
// Phala accepts.
func (i instance) matchesID(identifier string) bool {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return false
	}
	for _, candidate := range []string{i.ID, i.VMUUID, i.AppID, i.InstanceID, i.Name} {
		if strings.TrimSpace(candidate) == identifier {
			return true
		}
	}
	return false
}

// listOutput is the wrapper object `phala cvms list --json` returns. Unlike the
// nsc list, Phala nests the resources under items rather than returning a bare
// array.
type listOutput struct {
	Success bool       `json:"success"`
	Items   []instance `json:"items"`
}

// deployOutput is the wrapper object `phala deploy --json` returns. A live run
// against real Phala TDX hardware emits the created CVM under a top-level
// snake_case shape:
//
//	{"success":true,"vm_uuid":"...","name":"...","app_id":"...","dashboard_url":"..."}
//
// app_id is the canonical handle (`cvms get/delete --cvm-id <app_id>` is
// confirmed working), so resolution prefers it. The identifier may also surface
// nested under cvm or in camelCase on other CLI versions, so deployOutput reads
// both spellings via instance's tolerant unmarshaler.
type deployOutput struct {
	Success bool
	Top     instance
	CVM     *instance
}

// UnmarshalJSON decodes the deploy wrapper: the top-level CVM identifiers (via
// instance's snake/camel-tolerant decoder) plus an optionally-nested cvm object
// and the success flag.
func (d *deployOutput) UnmarshalJSON(data []byte) error {
	if err := d.Top.UnmarshalJSON(data); err != nil {
		return err
	}
	var wrapper struct {
		Success bool      `json:"success"`
		CVM     *instance `json:"cvm"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	d.Success = wrapper.Success
	d.CVM = wrapper.CVM
	return nil
}

type ambiguousPhalaCreateError struct {
	cause error
}

func (e *ambiguousPhalaCreateError) Error() string { return e.cause.Error() }
func (e *ambiguousPhalaCreateError) Unwrap() error { return e.cause }

func applyDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Network = "public"
	cfg.SSHUser = "root"
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	if cfg.Phala.CLIPath == "" {
		cfg.Phala.CLIPath = "phala"
	}
	if cfg.Phala.InstanceType == "" {
		cfg.Phala.InstanceType = defaultInstanceType
	}
	if cfg.Phala.WorkRoot == "" {
		// The dstack --dev-os guest has a read-only squashfs root, so /work (and
		// any path on /) is NOT writable. /var/volatile is a writable tmpfs mount
		// present on every dstack guest; a dedicated subdir under it is where the
		// rsync'd repo and run workspace live. Users who need encrypted-at-rest
		// persistence can point --phala-work-root at /var/volatile/dstack/persistent/...
		cfg.Phala.WorkRoot = defaultWorkRoot
	}
	cfg.WorkRoot = cfg.Phala.WorkRoot
	cfg.ServerType = (Provider{}).ServerTypeForConfig(*cfg)
}

func instanceTypeForClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "", "standard":
		return "tdx.small"
	case "fast":
		return "tdx.medium"
	case "large":
		return "tdx.large"
	case "beast":
		return "tdx.xlarge"
	default:
		return strings.TrimSpace(class)
	}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) RebindResolvedLeaseTarget(target *core.LeaseTarget, leaseID string) error {
	core.UseStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b *backend) configForRun() core.Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	leaseID := core.NewLeaseID()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	for _, item := range instances {
		if owned(item, cfg) {
			servers = append(servers, b.server(item, cfg))
		}
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, _, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()

	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, b.now())
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s instance_type=%s keep=%v\n", providerName, leaseID, slug, cfg.ServerType, req.Keep)
	id, err := b.create(ctx, cfg, keyPath+".pub", labels)
	if err != nil {
		var ambiguous *ambiguousPhalaCreateError
		if errors.As(err, &ambiguous) {
			recoveryLabels := make(map[string]string, len(labels)+2)
			for key, value := range labels {
				recoveryLabels[key] = value
			}
			recoveryLabels["recovery"] = "ambiguous-create"
			recoveryLabels["state"] = "provisioning"
			server := core.Server{Provider: providerName, Name: slug, Labels: recoveryLabels}
			var claimErr error
			if req.Repo.Root != "" {
				claimErr = core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{}, req.Repo.Root, cfg.IdleTimeout, req.Reclaim)
			} else {
				claimErr = core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, core.SSHTarget{}, cfg.IdleTimeout)
			}
			if claimErr != nil {
				return core.LeaseTarget{}, errors.Join(err, fmt.Errorf("persist Phala ambiguous-create recovery: %w", claimErr))
			}
			cleanupKey = false
		}
		return core.LeaseTarget{}, err
	}
	persistRecovery := func(recovery string) error {
		recoveryLabels := make(map[string]string, len(labels)+3)
		for key, value := range labels {
			recoveryLabels[key] = value
		}
		recoveryLabels["phala_cvm"] = id
		recoveryLabels["recovery"] = recovery
		recoveryLabels["state"] = "provisioning"
		item := instance{ID: id, Name: phalaCVMName(leaseID), Labels: recoveryLabels}
		lease := b.lease(item, cfg, leaseID)
		if req.Repo.Root != "" {
			return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim)
		}
		return core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, lease.Server, lease.SSH, cfg.IdleTimeout)
	}
	rollback := func(cause error) error {
		if req.Keep {
			cleanupKey = false
			if claimErr := persistRecovery("kept-after-failure"); claimErr != nil {
				return errors.Join(cause, fmt.Errorf("persist kept Phala recovery: %w", claimErr))
			}
			return cause
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), phalaRollbackTimeout)
		defer cancel()
		if destroyErr := b.destroy(cleanupCtx, id); destroyErr != nil {
			cleanupKey = false
			claimErr := persistRecovery("rollback-cleanup")
			return errors.Join(cause, fmt.Errorf("destroy leaked Phala CVM %s: %w", id, destroyErr), claimErr)
		}
		return cause
	}
	item, err := b.findInstance(ctx, id)
	if err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	lease := b.lease(item, cfg, leaseID)
	if err := b.prepareSSH(ctx, cfg, &lease.SSH); err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	lease.Server.Status = "ready"
	lease.Server.Labels["state"] = "ready"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s phala_cvm=%s state=ready\n", leaseID, id)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	item, leaseID, err := b.resolve(ctx, req.ID, cfg, req.ReleaseOnly)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	lease := b.lease(item, cfg, leaseID)
	if req.ReleaseOnly || req.StatusOnly && !req.ReadyProbe {
		return lease, nil
	}
	if leaseID == "" {
		return core.LeaseTarget{}, core.Exit(4, "Phala CVM %s has no Crabbox lease id", item.cloudID())
	}
	if req.ReadyProbe || !req.StatusOnly {
		if err := b.prepareSSH(ctx, cfg, &lease.SSH); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if req.Repo.Root != "" {
		if err := core.ClaimLeaseTargetForRepoConfig(leaseID, item.Labels["slug"], cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := phalaClaims(cfg)
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseView, 0, len(instances))
	for _, item := range instances {
		// Phala has no server-side ownership label, so owned() only proves the
		// crabbox- name prefix. A name-prefixed CVM with no matching local claim
		// could be a foreign resource; require the local claim before treating it
		// as ours and surfacing it in the listing.
		if !owned(item, cfg) {
			continue
		}
		claim, ok := claims[item.Labels["lease"]]
		if !ok {
			continue
		}
		server := b.server(item, cfg)
		mergeClaimLabels(&server, claim)
		out = append(out, server)
	}
	return out, nil
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.configForRun()
	if result, err := b.phala(ctx, cfg, []string{"--version"}, nil); err != nil {
		return core.DoctorResult{}, commandError("phala --version", result, err)
	}
	if result, err := b.phala(ctx, cfg, []string{"status", "--json"}, nil); err != nil {
		return core.DoctorResult{}, commandError("phala status", result, err)
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.CLIDoctorResult(providerName, len(instances), "phala"), nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	cfg := b.configForRun()
	id := strings.TrimSpace(req.Lease.Server.CloudID)
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if id == "" {
		item, resolvedLeaseID, err := b.resolve(ctx, leaseID, cfg, true)
		if err != nil {
			return err
		}
		id, leaseID = item.cloudID(), resolvedLeaseID
	}
	if leaseID == "" {
		return core.Exit(4, "refusing to destroy Phala CVM %s without a Crabbox lease id", id)
	}
	present, err := b.validateDestroyTarget(ctx, cfg, id, leaseID)
	if err != nil {
		return err
	}
	if present {
		if err := b.destroy(ctx, id); err != nil {
			return err
		}
	}
	if leaseID != "" {
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
	}
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("released lease=%s phala_cvm=%s", lease.LeaseID, lease.Server.CloudID)
}

func (b *backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	cfg := b.configForRun()
	now := b.now()
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, cfg, req.State, now)
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID != "" {
		claim, ok, err := resolvePhalaClaim(leaseID, cfg)
		if err != nil {
			return core.Server{}, err
		}
		idleTimeout := req.IdleTimeout
		if idleTimeout <= 0 {
			idleTimeout = cfg.IdleTimeout
		}
		if ok {
			if claim.RepoRoot != "" {
				_, err = core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, server.Labels["slug"], cfg, server, req.Lease.SSH, claim.RepoRoot, idleTimeout, false, claim, true)
			} else {
				_, err = core.ClaimLeaseTargetForConfigIfUnchanged(leaseID, server.Labels["slug"], cfg, server, req.Lease.SSH, idleTimeout, claim, true)
			}
		} else {
			err = core.ClaimLeaseTargetForConfig(leaseID, server.Labels["slug"], cfg, server, req.Lease.SSH, idleTimeout)
		}
		if err != nil {
			return core.Server{}, err
		}
	}
	return server, nil
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	instances, err := b.listInstances(ctx)
	if err != nil {
		return err
	}
	claims, err := phalaClaims(cfg)
	if err != nil {
		return err
	}
	live := make(map[string]struct{}, len(instances))
	for _, item := range instances {
		if !owned(item, cfg) {
			continue
		}
		leaseID := item.Labels["lease"]
		live[leaseID] = struct{}{}
		claim := claims[leaseID]
		if claim.LeaseID == "" {
			refreshed, ok, err := resolvePhalaClaim(leaseID, cfg)
			if err != nil {
				return err
			}
			if ok {
				claim = refreshed
			}
		}
		// Phala has no server-side ownership label, so a name-prefixed CVM with no
		// matching local claim cannot be proven to be ours. Never delete it; the
		// local claim is the destructive-op authority for this provider.
		if claim.LeaseID == "" {
			fmt.Fprintf(b.rt.Stderr, "skip phala_cvm=%s reason=no local claim for lease %s\n", item.cloudID(), leaseID)
			continue
		}
		server := b.server(item, cfg)
		mergeClaimLabels(&server, claim)
		remove, reason := core.ShouldCleanupServer(server, b.now())
		if recoveryRemove, recoveryReason, handled := phalaRecoveryCleanup(claim, b.now()); handled {
			remove, reason = recoveryRemove, recoveryReason
		}
		if !remove {
			fmt.Fprintf(b.rt.Stderr, "skip phala_cvm=%s reason=%s\n", item.cloudID(), reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would destroy phala_cvm=%s lease=%s reason=%s\n", item.cloudID(), item.Labels["lease"], reason)
			continue
		}
		if claim.LeaseID != "" {
			labels := make(map[string]string, len(claim.Labels)+1)
			for key, value := range claim.Labels {
				labels[key] = value
			}
			labels["state"] = "releasing"
			claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
			if err != nil {
				return fmt.Errorf("claim Phala CVM %s for cleanup: %w", item.cloudID(), err)
			}
		}
		fmt.Fprintf(b.rt.Stdout, "destroy phala_cvm=%s lease=%s reason=%s\n", item.cloudID(), item.Labels["lease"], reason)
		if err := b.destroy(ctx, item.cloudID()); err != nil {
			return err
		}
		if claim.LeaseID != "" {
			if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
				return fmt.Errorf("finalize Phala CVM cleanup claim: %w", err)
			}
		}
		core.RemoveStoredTestboxKey(leaseID)
	}
	for leaseID, claim := range claims {
		if _, ok := live[leaseID]; ok || claim.LeaseID == "" {
			continue
		}
		if claim.Labels["recovery"] == "ambiguous-create" && phalaRecoveryPending(claim, b.now()) {
			fmt.Fprintf(b.rt.Stderr, "skip claim lease=%s reason=ambiguous create recovery pending\n", claim.LeaseID)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s reason=missing Phala CVM\n", claim.LeaseID)
			continue
		}
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return fmt.Errorf("remove missing Phala CVM claim: %w", err)
		}
		core.RemoveStoredTestboxKey(claim.LeaseID)
	}
	return nil
}

func (b *backend) create(ctx context.Context, cfg core.Config, publicKeyPath string, labels map[string]string) (string, error) {
	leaseID := labels["lease"]
	name := phalaCVMName(leaseID)
	// --dev-os boots the dstack dev OS image which runs sshd and accepts the
	// injected key; --wait blocks until the CVM is provisioned.
	args := []string{"deploy", "--json", "--dev-os", "--ssh-pubkey", publicKeyPath, "--wait"}
	if name != "" {
		args = append(args, "-n", name)
	}
	if instanceType := strings.TrimSpace(cfg.ServerType); instanceType != "" {
		args = append(args, "-t", instanceType)
	}
	if nodeID := strings.TrimSpace(cfg.Phala.NodeID); nodeID != "" {
		args = append(args, "--node-id", nodeID)
	}
	// The Phala CLI deploy handler requires a Compose file before it provisions a
	// CVM in non-interactive mode. Use the configured compose when present, else
	// materialize the embedded default into the per-lease temp dir so deploy
	// always carries --compose.
	compose, err := composeFileForDeploy(cfg, publicKeyPath)
	if err != nil {
		return "", err
	}
	args = append(args, "--compose", compose)
	result, err := b.phala(ctx, cfg, args, b.rt.Stderr)
	if err != nil {
		if ambiguousPhalaCreateOutcome(result, err) {
			recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if recovered, recoverErr := b.recoverByLease(recoveryCtx, cfg, leaseID, 30*time.Second); recoverErr == nil {
				fmt.Fprintf(b.rt.Stderr, "warning: phala deploy returned an error, recovered phala_cvm=%s from lease name\n", recovered.cloudID())
				return recovered.cloudID(), nil
			}
			return "", &ambiguousPhalaCreateError{cause: commandError("phala deploy", result, err)}
		}
		return "", commandError("phala deploy", result, err)
	}
	id, parseErr := parseDeployID(result.Stdout)
	if parseErr != nil {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if recovered, recoverErr := b.recoverByLease(recoveryCtx, cfg, leaseID, 30*time.Second); recoverErr == nil {
			fmt.Fprintf(b.rt.Stderr, "warning: recovered phala_cvm=%s after invalid phala deploy output\n", recovered.cloudID())
			return recovered.cloudID(), nil
		}
		return "", &ambiguousPhalaCreateError{cause: parseErr}
	}
	return id, nil
}

func parseDeployID(stdout string) (string, error) {
	payload := jsonObjectPrefix(stdout)
	if payload == "" {
		return "", core.Exit(5, "phala deploy produced no JSON output")
	}
	var output deployOutput
	if err := json.Unmarshal([]byte(payload), &output); err != nil {
		return "", core.Exit(5, "parse phala deploy output: %v", err)
	}
	// Prefer a nested cvm object when present, else the top-level identifiers.
	// instance.cloudID() ranks app_id first, matching the canonical --cvm-id.
	if output.CVM != nil {
		if id := output.CVM.cloudID(); id != "" {
			return id, nil
		}
	}
	if id := output.Top.cloudID(); id != "" {
		return id, nil
	}
	return "", core.Exit(5, "phala deploy output did not include a CVM identifier")
}

// composeFileForDeploy resolves the Compose file path passed to `phala deploy
// --compose`. When the lease configures a compose path it is used verbatim;
// otherwise the embedded default is written into the per-lease temp dir (the
// directory that already holds the lease SSH key) so deploy never runs without a
// Compose file. publicKeyPath is the lease public key (<dir>/id_ed25519.pub);
// its directory is the per-lease temp dir.
func composeFileForDeploy(cfg core.Config, publicKeyPath string) (string, error) {
	if compose := strings.TrimSpace(cfg.Phala.Compose); compose != "" {
		return compose, nil
	}
	dir := filepath.Dir(publicKeyPath)
	if dir == "" || dir == "." {
		var err error
		dir, err = os.MkdirTemp("", "crabbox-phala-compose-")
		if err != nil {
			return "", core.Exit(2, "create phala default compose dir: %v", err)
		}
	}
	path := filepath.Join(dir, defaultComposeFileName)
	if err := os.WriteFile(path, []byte(defaultComposeYAML), 0o600); err != nil {
		return "", core.Exit(2, "write phala default compose: %v", err)
	}
	return path, nil
}

func ambiguousPhalaCreateOutcome(result core.LocalCommandResult, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	detail := strings.ToLower(result.Stdout + "\n" + result.Stderr + "\n" + err.Error())
	for _, marker := range []string{
		"broken pipe",
		"connection closed",
		"connection reset",
		"context deadline exceeded",
		"eof",
		"i/o timeout",
		"timed out",
		"transport is closing",
		"unavailable",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

func (b *backend) listInstances(ctx context.Context) ([]instance, error) {
	cfg := b.configForRun()
	// `phala cvms list` accepts no node flag (only --cvm-id/--page/--page-size/
	// --search), so node-scoping is applied client-side via nodeMatchesScope in
	// owned() rather than passed to the CLI.
	args := []string{"cvms", "list", "--json"}
	result, err := b.phala(ctx, cfg, args, nil)
	if err != nil {
		return nil, commandError("phala cvms list", result, err)
	}
	payload := jsonObjectPrefix(result.Stdout)
	if payload == "" {
		return nil, nil
	}
	var output listOutput
	if err := json.Unmarshal([]byte(payload), &output); err != nil {
		return nil, core.Exit(5, "parse phala cvms list output: %v", err)
	}
	// crabbox ownership is derived from the CVM name prefix (crabbox-<lease>), so
	// a list item with no name can never be owned and would also have no usable
	// handle. Skip such items defensively rather than surfacing a blank instance.
	items := make([]instance, 0, len(output.Items))
	for _, item := range output.Items {
		if strings.TrimSpace(item.Name) == "" && item.cloudID() == "" {
			continue
		}
		item.Labels = phalaLabels(item, cfg)
		items = append(items, item)
	}
	return items, nil
}

func (b *backend) findInstance(ctx context.Context, id string) (instance, error) {
	item, ok, err := b.lookupInstance(ctx, id)
	if err != nil {
		return instance{}, err
	}
	if ok {
		return item, nil
	}
	return instance{}, core.Exit(4, "Phala CVM not found: %s", id)
}

func (b *backend) lookupInstance(ctx context.Context, id string) (instance, bool, error) {
	instances, err := b.listInstances(ctx)
	if err != nil {
		return instance{}, false, err
	}
	for _, item := range instances {
		if item.matchesID(id) {
			return item, true, nil
		}
	}
	return instance{}, false, nil
}

func (b *backend) findByLease(ctx context.Context, cfg core.Config, leaseID string) (instance, error) {
	instances, err := b.listInstances(ctx)
	if err != nil {
		return instance{}, err
	}
	var found []instance
	for _, item := range instances {
		if owned(item, cfg) && item.Labels["lease"] == leaseID {
			found = append(found, item)
		}
	}
	if len(found) != 1 {
		return instance{}, core.Exit(4, "expected one Phala CVM for lease %s, found %d", leaseID, len(found))
	}
	return found[0], nil
}

func (b *backend) recoverByLease(ctx context.Context, cfg core.Config, leaseID string, timeout time.Duration) (instance, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		item, err := b.findByLease(ctx, cfg, leaseID)
		if err == nil {
			return item, nil
		}
		lastErr = err
		select {
		case <-ticker.C:
		case <-deadline.C:
			return instance{}, lastErr
		case <-ctx.Done():
			return instance{}, ctx.Err()
		}
	}
}

func (b *backend) resolve(ctx context.Context, identifier string, cfg core.Config, allowMissing bool) (instance, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return instance{}, "", core.Exit(2, "provider=%s requires --id", providerName)
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return instance{}, "", err
	}
	for _, item := range instances {
		if owned(item, cfg) && (item.matchesID(identifier) || item.Labels["lease"] == identifier) {
			return item, item.Labels["lease"], nil
		}
	}
	if claim, ok, err := resolvePhalaClaim(identifier, cfg); err != nil {
		return instance{}, "", err
	} else if ok {
		id := strings.TrimSpace(claim.CloudID)
		if id == "" {
			id = strings.TrimSpace(claim.Labels["phala_cvm"])
		}
		if id == "" && claim.Labels["recovery"] == "ambiguous-create" {
			item, recoveryErr := b.findByLease(ctx, cfg, claim.LeaseID)
			if recoveryErr != nil {
				return instance{}, "", core.Exit(4, "Phala ambiguous-create recovery is still pending for lease=%s; credentials retained", claim.LeaseID)
			}
			return item, claim.LeaseID, nil
		}
		var item instance
		found := false
		for _, candidate := range instances {
			if candidate.matchesID(id) {
				item, found = candidate, true
				break
			}
		}
		if !found {
			if allowMissing {
				return instance{ID: id, Name: phalaCVMName(claim.LeaseID), Labels: claim.Labels}, claim.LeaseID, nil
			}
			return instance{}, "", core.Exit(4, "Phala CVM not found: %s", id)
		}
		if !owned(item, cfg) || item.Labels["lease"] != claim.LeaseID {
			return instance{}, "", core.Exit(4, "refusing Phala CVM %s: ownership labels do not match lease %s", id, claim.LeaseID)
		}
		return item, claim.LeaseID, nil
	}
	normalized := core.NormalizeLeaseSlug(identifier)
	var matched instance
	for _, item := range instances {
		if !owned(item, cfg) {
			continue
		}
		if core.NormalizeLeaseSlug(item.Labels["slug"]) == normalized {
			if matched.cloudID() != "" {
				return instance{}, "", core.Exit(2, "multiple Phala CVM leases match slug %s", identifier)
			}
			matched = item
		}
	}
	if matched.cloudID() != "" {
		return matched, matched.Labels["lease"], nil
	}
	return instance{}, "", core.Exit(4, "Phala CVM lease not found: %s", identifier)
}

func (b *backend) lease(item instance, cfg core.Config, leaseID string) core.LeaseTarget {
	target := core.SSHTarget{
		User:                   "root",
		Host:                   item.cloudID(),
		Key:                    cfg.SSHKey,
		Port:                   "22",
		TargetOS:               core.TargetLinux,
		ReadyCheck:             "command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
		NoControlMaster:        true,
		DisableHostKeyChecking: true,
		NetworkKind:            "public",
		SSHConfigProxy:         true,
		ProxyCommand:           proxyCommand(cfg, item.cloudID()),
	}
	if leaseID != "" {
		core.UseStoredTestboxKey(&target, leaseID)
	}
	server := b.server(item, cfg)
	if claim, ok, _ := resolvePhalaClaim(leaseID, cfg); ok {
		mergeClaimLabels(&server, claim)
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
}

func (b *backend) prepareSSH(ctx context.Context, cfg core.Config, target *core.SSHTarget) error {
	probe := *target
	probe.ReadyCheck = "true"
	if err := core.WaitForSSHReady(ctx, &probe, b.rt.Stderr, "phala cvm ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		return err
	}
	target.Port = probe.Port
	if err := core.RunSSHQuiet(ctx, *target, phalaToolBootstrapCommand()); err != nil {
		return core.Exit(1, "Phala CVM tool bootstrap failed: %v", err)
	}
	return core.WaitForSSHReady(ctx, target, b.rt.Stderr, "phala cvm tools", core.BootstrapWaitTimeout(cfg))
}

// phalaToolBootstrapCommand prepares a leased Phala CVM for crabbox's
// rsync-based sync. The canonical dstack --dev-os guest is an immutable
// confidential-compute appliance (read-only squashfs root, no package manager,
// no network egress) that already ships rsync, tar and python3 -- exactly what
// the sync and exec path needs. crabbox does NOT need git on the box: the file
// manifest is computed locally and the tree is rsync'd (not git-cloned) over the
// SSH gateway tunnel. So the REQUIRED set is rsync+tar+python3; git is installed
// only opportunistically when a package manager happens to exist (e.g. a
// non-dev-os image) and is never required -- otherwise the bootstrap could never
// succeed on the appliance guest, which is the supported deployment.
func phalaToolBootstrapCommand() string {
	return strings.Join([]string{
		"set -e",
		"if command -v rsync >/dev/null 2>&1 && command -v tar >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1; then exit 0; fi",
		"if command -v apt-get >/dev/null 2>&1; then",
		"  apt-get update >/tmp/crabbox-phala-apt-update.log 2>&1",
		"  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends git rsync tar python3 >/tmp/crabbox-phala-apt-install.log 2>&1",
		"elif command -v dnf >/dev/null 2>&1; then",
		"  dnf install -y git rsync tar python3 >/tmp/crabbox-phala-dnf-install.log 2>&1",
		"elif command -v yum >/dev/null 2>&1; then",
		"  yum install -y git rsync tar python3 >/tmp/crabbox-phala-yum-install.log 2>&1",
		"elif command -v apk >/dev/null 2>&1; then",
		"  apk add --no-cache git rsync tar python3 >/tmp/crabbox-phala-apk-install.log 2>&1",
		"fi",
		"command -v rsync >/dev/null && command -v tar >/dev/null && command -v python3 >/dev/null",
	}, "\n")
}

func (b *backend) server(item instance, cfg core.Config) core.Server {
	labels := make(map[string]string, len(item.Labels)+4)
	for key, value := range item.Labels {
		labels[key] = value
	}
	labels["phala_cvm"] = item.cloudID()
	labels["work_root"] = cfg.WorkRoot
	if labels["state"] == "" {
		labels["state"] = phalaState(item.Status)
	}
	labels["server_type"] = firstNonBlank(labels["server_type"], item.InstanceType, cfg.ServerType)
	server := core.Server{
		CloudID:  item.cloudID(),
		Provider: providerName,
		Name:     firstNonBlank(labels["slug"], item.Name, item.cloudID()),
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = labels["server_type"]
	return server
}

func owned(item instance, cfg core.Config) bool {
	return item.Labels["provider"] == providerName &&
		item.Labels["crabbox"] == "true" &&
		item.Labels["created_by"] == "crabbox" &&
		item.Labels["lease"] != "" &&
		nodeMatchesScope(item, cfg)
}

// nodeMatchesScope keeps ownership scoped to the configured node when one is
// pinned, mirroring how the Namespace provider scopes ownership by tenant.
func nodeMatchesScope(item instance, cfg core.Config) bool {
	nodeID := strings.TrimSpace(cfg.Phala.NodeID)
	if nodeID == "" {
		return true
	}
	return strings.TrimSpace(item.NodeID) == nodeID || strings.TrimSpace(item.Node) == nodeID
}

// phalaLabels synthesizes the crabbox ownership labels from the CVM name. Phala
// deploy carries no arbitrary key/value labels, so the lease id and ownership
// markers are recovered from the crabbox- name prefix and cross-checked against
// the local claim during resolution.
func phalaLabels(item instance, cfg core.Config) map[string]string {
	labels := map[string]string{}
	if leaseID := leaseIDFromName(item.Name); leaseID != "" {
		labels["provider"] = providerName
		labels["crabbox"] = "true"
		labels["created_by"] = "crabbox"
		labels["lease"] = leaseID
	}
	if claim, ok, _ := resolvePhalaClaim(labels["lease"], cfg); ok {
		for key, value := range claim.Labels {
			if _, exists := labels[key]; !exists {
				labels[key] = value
			}
		}
		if claim.Slug != "" {
			labels["slug"] = claim.Slug
		}
	}
	return labels
}

func phalaCVMName(leaseID string) string {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return ""
	}
	return crabboxCVMNamePrefix + strings.ReplaceAll(leaseID, "_", "-")
}

func leaseIDFromName(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, crabboxCVMNamePrefix) {
		return ""
	}
	suffix := strings.TrimPrefix(name, crabboxCVMNamePrefix)
	if suffix == "" {
		return ""
	}
	// crabbox lease ids are cbx_<hex>; the deploy name dashed the underscore.
	return strings.Replace(suffix, "-", "_", 1)
}

func phalaState(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "":
		return "running"
	case "running", "ready", "started":
		return "running"
	case "starting", "provisioning", "pending", "creating", "deploying":
		return "provisioning"
	case "stopped", "stopping", "paused":
		return "stopped"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func phalaClaims(cfg core.Config) (map[string]core.LeaseClaim, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	return filterPhalaClaims(claims, core.ProviderClaimScope(providerName, cfg)), nil
}

func filterPhalaClaims(claims []core.LeaseClaim, scope string) map[string]core.LeaseClaim {
	out := make(map[string]core.LeaseClaim)
	for _, claim := range claims {
		if claim.Provider == providerName && claim.ProviderScope == scope && claim.LeaseID != "" {
			out[claim.LeaseID] = claim
		}
	}
	return out
}

func resolvePhalaClaim(identifier string, cfg core.Config) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	scope := core.ProviderClaimScope(providerName, cfg)
	var exact core.LeaseClaim
	var slugMatch core.LeaseClaim
	normalized := core.NormalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope {
			continue
		}
		if claim.LeaseID == identifier || claim.CloudID == identifier {
			if exact.LeaseID != "" {
				return core.LeaseClaim{}, false, core.Exit(2, "multiple provider=%s scope=%s claims exactly match %s", providerName, scope, identifier)
			}
			exact = claim
			continue
		}
		if normalized != "" && core.NormalizeLeaseSlug(claim.Slug) == normalized {
			if slugMatch.LeaseID != "" {
				return core.LeaseClaim{}, false, core.Exit(2, "multiple provider=%s scope=%s claims match slug %s", providerName, scope, identifier)
			}
			slugMatch = claim
		}
	}
	if exact.LeaseID != "" {
		return exact, true, nil
	}
	return slugMatch, slugMatch.LeaseID != "", nil
}

func phalaRecoveryPending(claim core.LeaseClaim, now time.Time) bool {
	createdSeconds, err := strconv.ParseInt(strings.TrimSpace(claim.Labels["created_at"]), 10, 64)
	if err != nil || createdSeconds <= 0 {
		return true
	}
	return now.UTC().Before(time.Unix(createdSeconds, 0).UTC().Add(phalaAmbiguousCreateRecoveryGrace))
}

func phalaRecoveryCleanup(claim core.LeaseClaim, now time.Time) (bool, string, bool) {
	switch claim.Labels["recovery"] {
	case "ambiguous-create":
		if phalaRecoveryPending(claim, now) {
			return false, "ambiguous create recovery pending", true
		}
		return true, "ambiguous create recovery grace expired", true
	case "rollback-cleanup":
		return true, "failed acquisition rollback", true
	case "kept-after-failure":
		return false, "kept after failed acquisition", true
	default:
		return false, "", false
	}
}

func mergeClaimLabels(server *core.Server, claim core.LeaseClaim) {
	if claim.LeaseID == "" {
		return
	}
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	for key, value := range claim.Labels {
		server.Labels[key] = value
	}
	server.Labels["lease"] = claim.LeaseID
	if claim.Slug != "" {
		server.Labels["slug"] = claim.Slug
	}
}

func proxyCommand(cfg core.Config, cvmID string) string {
	executable, err := os.Executable()
	if err != nil {
		executable = os.Args[0]
	}
	words := []string{executable, "__phala-proxy", "--phala", cfg.Phala.CLIPath}
	if nodeID := strings.TrimSpace(cfg.Phala.NodeID); nodeID != "" {
		words = append(words, "--node-id", nodeID)
	}
	words = append(words, cvmID)
	for i := range words {
		words[i] = quoteProxyWord(words[i])
	}
	return strings.Join(words, " ")
}

func quoteProxyWord(word string) string {
	word = strings.ReplaceAll(word, "%", "%%")
	if word != "" && strings.IndexFunc(word, func(r rune) bool {
		return !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("_-./:,@%+=", r))
	}) == -1 {
		return word
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(word) + `"`
}

func (b *backend) destroy(ctx context.Context, id string) error {
	cfg := b.configForRun()
	result, err := b.phala(ctx, cfg, []string{"cvms", "delete", "--cvm-id", id, "--force"}, b.rt.Stderr)
	if err == nil {
		return nil
	}
	detail := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	if strings.Contains(detail, "not found") || strings.Contains(detail, "does not exist") {
		return nil
	}
	return commandError("phala cvms delete", result, err)
}

func (b *backend) validateDestroyTarget(ctx context.Context, cfg core.Config, id, leaseID string) (bool, error) {
	// Phala has no server-side ownership label: owned() only proves the crabbox-
	// name prefix, which a foreign CVM could also carry. The local lease claim is
	// the destructive-op authority, so require a matching claim before issuing a
	// delete.
	if _, ok, err := resolvePhalaClaim(leaseID, cfg); err != nil {
		return false, err
	} else if !ok {
		return false, core.Exit(4, "refusing to destroy Phala CVM %s: no local claim for lease %s", id, leaseID)
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range instances {
		if !item.matchesID(id) {
			continue
		}
		if !owned(item, cfg) {
			return false, core.Exit(4, "refusing to destroy Phala CVM %s without Crabbox ownership labels", id)
		}
		if leaseID != "" && item.Labels["lease"] != leaseID {
			return false, core.Exit(4, "refusing to destroy Phala CVM %s: lease label %q does not match %q", id, item.Labels["lease"], leaseID)
		}
		return true, nil
	}
	return false, nil
}

func (b *backend) phala(ctx context.Context, cfg core.Config, args []string, stderr io.Writer) (core.LocalCommandResult, error) {
	return b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:   cfg.Phala.CLIPath,
		Args:   append([]string(nil), args...),
		Stderr: stderr,
	})
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func commandError(action string, result core.LocalCommandResult, err error) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return core.Exit(result.ExitCode, "%s failed: %v: %s", action, err, detail)
	}
	return core.Exit(result.ExitCode, "%s failed: %v", action, err)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// jsonObjectPrefix returns the first top-level JSON object/array embedded in a
// CLI stdout stream, discarding BOTH leading and trailing non-JSON noise. The
// phala CLI prints a leading human progress line (e.g. "Provisioning CVM
// <name>...") before the JSON payload on `deploy`, and appends a libuv
// assertion line after the payload on some platforms; scanning from the first
// top-level brace keeps both kinds of noise from corrupting decoding.
func jsonObjectPrefix(stdout string) string {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return ""
	}
	start := strings.IndexAny(trimmed, "{[")
	if start < 0 {
		return ""
	}
	trimmed = trimmed[start:]
	var open, close byte
	switch trimmed[0] {
	case '{':
		open, close = '{', '}'
	case '[':
		open, close = '[', ']'
	default:
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return trimmed[:i+1]
			}
		}
	}
	return trimmed
}
