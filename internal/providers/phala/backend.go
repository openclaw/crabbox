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
// The live `phala cvms list --json` item emits camelCase keys (appId, cvmName,
// status, uptime) and carries the name under `cvmName` (there is NO `name`
// key), while `phala cvms get --json` emits a flat snake_case object (app_id,
// vm_uuid, instance_id, name). instance therefore reads BOTH spellings of every
// identifier -- including the `cvmName` list-name key -- via a custom
// unmarshaler.
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
	// The live `cvms list` item carries the CVM name under `cvmName` (camelCase);
	// `cvms get`/deploy carry it under `name`. Read all spellings so ownership and
	// recovery work against either shape (real list items have NO `name` key).
	i.Name = str("name", "cvmName", "appName")
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
	// Resolve the TLS SSH gateway host ONCE, here, and cache it on the lease so
	// every subsequent SSH connection (including the initial prepareSSH probe
	// below and `status --wait`'s short readiness probe) tunnels straight to the
	// gateway without a per-connection `phala cvms get` call. Best-effort: if
	// resolution fails the label is omitted and the proxy falls back to resolving
	// the host itself.
	if gatewayHost, err := b.resolveGatewayHost(ctx, id); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: could not cache phala gateway host for phala_cvm=%s: %v\n", id, err)
	} else if gatewayHost != "" {
		labels["gateway_host"] = gatewayHost
	} else {
		fmt.Fprintf(b.rt.Stderr, "warning: phala gateway host unresolved for phala_cvm=%s; SSH will resolve it per connection\n", id)
	}
	item, err := b.findInstance(ctx, id)
	if err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	// findInstance rebuilt item.Labels from listInstances (no claim exists yet),
	// so re-apply the cached gateway host AND the allocated slug directly. Without
	// the slug here, lease.Server.Labels["slug"] is blank and the claim's endpoint
	// labels persist an empty slug (claim.Slug is still set from the explicit slug
	// arg, but the labels diverge), which surfaces as slug=- on List and a later
	// resolve-by-slug miss.
	if item.Labels == nil {
		item.Labels = map[string]string{}
	}
	if host := labels["gateway_host"]; host != "" {
		item.Labels["gateway_host"] = host
	}
	if slug != "" {
		item.Labels["slug"] = slug
	}
	lease := b.lease(item, cfg, leaseID)
	if err := b.prepareSSH(ctx, cfg, &lease.SSH); err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	// TDX attestation gate: the box is reachable, so before trusting it as a
	// confidential CVM, prove it is a genuine Intel TDX enclave running OUR
	// authorized code. On any failure DESTROY the just-created CVM (rollback) and
	// refuse the lease so a non-attesting box is never leaked or trusted.
	if attestEnabled(cfg) {
		info, err := b.fetchAttestation(ctx, lease.SSH)
		if err != nil {
			return core.LeaseTarget{}, rollback(fmt.Errorf("refusing Phala lease: TDX attestation fetch failed for phala_cvm=%s: %w", id, err))
		}
		report, err := verifyAttestation(info, id, true)
		if err != nil {
			return core.LeaseTarget{}, rollback(fmt.Errorf("refusing Phala lease: TDX attestation verification failed for phala_cvm=%s: %w", id, err))
		}
		lease.Server.Labels["attested"] = "true"
		lease.Server.Labels["tdx_app_id"] = report.AppID
		lease.Server.Labels["tdx_compose_hash"] = report.ComposeHash
		lease.Server.Labels["tdx_os_image_hash"] = report.OSImageHash
		lease.Server.Labels["tdx_rtmr3"] = report.Rtmr3
		fmt.Fprintf(b.rt.Stderr, "attested phala_cvm=%s app_id=%s compose_hash=%s rtmr3=%s\n", id, report.AppID, report.ComposeHash, report.Rtmr3)
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
	// prepareSSH re-runs the FULL tool bootstrap over SSH. That belongs ONLY to a
	// non-status acquire/run resolve. For status checks (StatusOnly, including
	// `status --wait` which sets ReadyProbe) readiness is decided by the caller's
	// lightweight probeSSHReady in statusViewFromLeaseTarget -- re-bootstrapping on
	// every status poll re-runs apt/dnf over SSH and times the poll out. The cached
	// gateway_host on the lease target keeps that lightweight probe from paying a
	// per-connection `phala cvms get`.
	if !req.StatusOnly {
		if err := b.prepareSSH(ctx, cfg, &lease.SSH); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if req.Repo.Root != "" {
		// The slug is stored in BOTH the claim.Slug field and the claim labels, but
		// the claim labels can carry a blank slug (acquire records the endpoint
		// labels before any claim exists, so item.Labels["slug"] is empty then).
		// Re-claiming with item.Labels["slug"] would write an EMPTY slug and WIPE the
		// stored one. b.lease()/mergeClaimLabels surfaced the authoritative claim.Slug
		// onto lease.Server.Labels["slug"], so prefer that, and never overwrite a
		// non-empty stored slug with a blank.
		slug := firstNonBlank(lease.Server.Labels["slug"], item.Labels["slug"])
		if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
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
	// validateDestroyTarget returns (true,nil) when the CVM is confirmed present
	// and owned, (false,nil) ONLY when it is PROVABLY gone (a definitive
	// not-found), and a real error on any ambiguous lookup failure (e.g. a
	// transient transport error, even one whose text contains "not found"). The
	// local claim is the SOLE ownership anchor, so it must survive an ambiguous
	// failure: returning that error here retains the claim+key so a later
	// `crabbox stop` can retry.
	present, err := b.validateDestroyTarget(ctx, cfg, id, leaseID)
	if err != nil {
		return err
	}
	if present {
		if err := b.destroy(ctx, id); err != nil {
			// The destroy failed (e.g. a transient gateway error that destroy() did
			// NOT classify as already-gone): keep the claim+key so a retry can finish
			// the release rather than orphaning a live billing CVM with no anchor.
			return err
		}
	}
	// Reached only when the CVM was destroyed (present) or is PROVABLY gone
	// (present=false from a definitive not-found): both mean the resource no longer
	// exists, so the claim+key are safe to reap.
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
		// The claim write unconditionally overwrites claim.Slug with the slug arg, so
		// a blank server.Labels["slug"] (e.g. a lease target whose labels lost it)
		// would WIPE the stored slug on every idle keepalive. Prefer the existing
		// claim's slug so Touch never blanks it.
		slug := firstNonBlank(server.Labels["slug"], claim.Slug)
		if ok {
			if claim.RepoRoot != "" {
				_, err = core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, server, req.Lease.SSH, claim.RepoRoot, idleTimeout, false, claim, true)
			} else {
				_, err = core.ClaimLeaseTargetForConfigIfUnchanged(leaseID, slug, cfg, server, req.Lease.SSH, idleTimeout, claim, true)
			}
		} else {
			err = core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, req.Lease.SSH, idleTimeout)
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
		// Corroborate ownership the same way ReleaseLease does before any delete:
		// require a matching local claim AND a `cvms get` whose name encodes THIS
		// lease. On dstack app_id reuse a stale-but-unexpired claim recording a
		// reused app_id makes owned() true, so without this gate a Cleanup could
		// delete a FOREIGN CVM.
		present, err := b.validateDestroyTarget(ctx, cfg, item.cloudID(), claim.LeaseID)
		if err != nil {
			// An ownership/name-corroboration refusal (exit 4) on a reused app_id must
			// not abort the whole sweep nor drop the claim/key (it may be a foreign
			// reuse): skip this CVM and continue. Any other error (CLI/transport
			// failure) is genuine and propagates.
			var exitErr core.ExitError
			if core.AsExitError(err, &exitErr) && exitErr.Code == 4 {
				fmt.Fprintf(b.rt.Stderr, "skip phala_cvm=%s reason=ownership/name corroboration failed: %v\n", item.cloudID(), err)
				continue
			}
			return err
		}
		if !present {
			// The CVM is already gone (a definitive not-found at corroboration time).
			// Skip without dropping the claim/key here; the missing-from-inventory
			// reaping pass below removes claims whose CVM is absent.
			fmt.Fprintf(b.rt.Stderr, "skip phala_cvm=%s reason=CVM not present at corroboration\n", item.cloudID())
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

// ambiguousPhalaCreateOutcome reports whether a failed `phala deploy` MIGHT have
// nonetheless created a CVM, so create() should recover-by-lease rather than
// surface the error. Cancellation is detected structurally via errors.Is. The
// remaining markers are ANCHORED, multi-word transport/transient phrases: the
// bare "eof"/"unavailable" substrings were widened to "unexpected eof" /
// "service unavailable" so they cannot false-match inside larger words or
// unrelated messages (e.g. "eof" inside a path) and waste the ~30s recovery
// window. Detail is scanned over the CLI's stdout/stderr plus the wrapped error
// text, since the transport signal often lives in the error.
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
		"unexpected eof",
		"i/o timeout",
		"timed out",
		"transport is closing",
		"service unavailable",
		"server unavailable",
		"currently unavailable",
		"temporarily unavailable",
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
		// owned() re-resolves the claim by this CVM's cloud id; a match proves the
		// CVM belongs to this lease. Phala carries no server labels to cross-check,
		// so the local claim is the authority.
		if !owned(item, cfg) {
			return instance{}, "", core.Exit(4, "refusing Phala CVM %s: no local claim maps to lease %s", id, claim.LeaseID)
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
		ProxyCommand:           proxyCommand(cfg, item.cloudID(), item.Labels["gateway_host"]),
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

// owned reports whether a leased CVM belongs to this crabbox install. Phala
// exposes no server-side labels and `cvms list` omits even the CVM name (only
// `cvms get`/`deploy`/`delete` echo it), so ownership is established two ways:
//
//   - Post-acquire authority: a local lease claim under our state dir maps to
//     this CVM's cloud id (recorded at acquire time). This is the reliable path
//     for List/Resolve/Release, where cvms list returns no name.
//   - Pre-claim recovery: the CVM name carries the crabbox-<lease> prefix. The
//     claim is written only AFTER create succeeds, so recovering our own
//     just-created CVM (e.g. after garbled deploy output) can only match by
//     name.
//
// The name path alone is NOT sufficient for destructive ops -- validateDestroyTarget
// separately requires a matching local claim -- so a foreign crabbox-named CVM
// can never be deleted or surfaced as a lease, the ownership-safety property the
// review required.
func owned(item instance, cfg core.Config) bool {
	if id := item.cloudID(); id != "" {
		if claim, ok, err := resolvePhalaClaim(id, cfg); err == nil && ok && claim.CloudID == id {
			return true
		}
	}
	if leaseID := leaseIDFromName(item.Name); leaseID != "" {
		return nodeMatchesScope(item, cfg)
	}
	return false
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
	// Primary source of truth: the local claim keyed on this CVM's cloud id.
	// Phala returns no server labels and cvms list omits the name, so the claim
	// recorded at acquire time is the only reliable owner/lease/slug record.
	if claim, ok, _ := resolvePhalaClaim(item.cloudID(), cfg); ok && claim.CloudID == item.cloudID() {
		for key, value := range claim.Labels {
			labels[key] = value
		}
		labels["provider"] = providerName
		labels["crabbox"] = "true"
		labels["created_by"] = "crabbox"
		labels["lease"] = claim.LeaseID
		if claim.Slug != "" {
			labels["slug"] = claim.Slug
		}
		return labels
	}
	// Fallback for objects that DO carry a name (cvms get/delete): derive the
	// lease from the crabbox- name prefix and enrich from a claim by lease.
	if leaseID := leaseIDFromName(item.Name); leaseID != "" {
		labels["provider"] = providerName
		labels["crabbox"] = "true"
		labels["created_by"] = "crabbox"
		labels["lease"] = leaseID
		if claim, ok, _ := resolvePhalaClaim(leaseID, cfg); ok {
			for key, value := range claim.Labels {
				if _, exists := labels[key]; !exists {
					labels[key] = value
				}
			}
			if claim.Slug != "" {
				labels["slug"] = claim.Slug
			}
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

func proxyCommand(cfg core.Config, cvmID, gatewayHost string) string {
	executable, err := os.Executable()
	if err != nil {
		executable = os.Args[0]
	}
	words := []string{executable, "__phala-proxy", "--phala", cfg.Phala.CLIPath}
	if nodeID := strings.TrimSpace(cfg.Phala.NodeID); nodeID != "" {
		words = append(words, "--node-id", nodeID)
	}
	// A cached gateway host (resolved once at acquire time) lets each SSH
	// connection skip the per-connection `phala cvms get` lookup. When absent
	// the proxy falls back to resolving it itself.
	if host := strings.TrimSpace(gatewayHost); host != "" {
		words = append(words, "--gateway-host", host)
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

// missingCVMResponse reports whether the phala CLI's stdout/stderr unambiguously
// signals the CVM itself is gone (so the caller treats it as already-deleted /
// not-found), as opposed to some OTHER not-found-ish failure such as a gateway
// endpoint or route lookup error. It matches only anchored, CVM-scoped phrases
// and explicitly rejects generic gateway/route errors so a real delete failure
// (e.g. "gateway endpoint not found") is never swallowed as success. The
// command's wrapped error text is deliberately NOT scanned here: a transient
// transport error can carry the words "not found" in an unrelated context.
func missingCVMResponse(stdout, stderr string) bool {
	detail := strings.ToLower(stdout + "\n" + stderr)
	// A generic gateway/route/endpoint failure is NOT a missing CVM, even if it
	// contains "not found"; do not treat it as already-gone.
	for _, generic := range []string{"gateway", "endpoint", "route", "upstream", "dns"} {
		if strings.Contains(detail, generic) {
			return false
		}
	}
	for _, phrase := range []string{
		"cvm not found",
		"no such cvm",
		"cvm does not exist",
		"could not find cvm",
		"does not exist",
	} {
		if strings.Contains(detail, phrase) {
			return true
		}
	}
	return false
}

func (b *backend) destroy(ctx context.Context, id string) error {
	cfg := b.configForRun()
	result, err := b.phala(ctx, cfg, []string{"cvms", "delete", "--cvm-id", id, "--force"}, b.rt.Stderr)
	if err == nil {
		return nil
	}
	// Only swallow the error when the CLI's own output unambiguously reports the
	// CVM is already gone. A generic failure (e.g. "gateway endpoint not found")
	// must propagate so a live billing CVM is never orphaned on a false success.
	if missingCVMResponse(result.Stdout, result.Stderr) {
		return nil
	}
	return commandError("phala cvms delete", result, err)
}

// getInstance fetches a single CVM via `phala cvms get --cvm-id <id> --json`.
// Unlike `cvms list` (which omits the CVM name on real hardware), `cvms get`
// echoes the name, which the destroy path needs to corroborate crabbox
// ownership against the local lease claim. A DEFINITIVE not-found response (the
// CLI exits non-zero and its OWN output unambiguously names a missing CVM) is
// treated as ok=false with a nil error so a CVM that is already gone is not an
// error. Any other failure -- including a transient/transport error whose
// wrapped text merely happens to contain "not found" -- propagates as a real
// error so the destroy path does NOT mistake it for an already-gone CVM and
// orphan a live billing CVM. The payload is a snake_case object that may be flat
// or nested under a top-level cvm object; instance's tolerant unmarshaler reads
// both spellings, and deployOutput's decoder already handles the optional cvm
// nesting, so it is reused here.
func (b *backend) getInstance(ctx context.Context, id string) (instance, bool, error) {
	cfg := b.configForRun()
	result, err := b.phala(ctx, cfg, []string{"cvms", "get", "--cvm-id", id, "--json"}, nil)
	if err != nil {
		// Scan only the CLI's own stdout/stderr (NOT err.Error()): a wrapped
		// transport error text can contain "not found" in an unrelated message.
		if missingCVMResponse(result.Stdout, result.Stderr) {
			return instance{}, false, nil
		}
		return instance{}, false, commandError("phala cvms get", result, err)
	}
	payload := jsonObjectPrefix(result.Stdout)
	if payload == "" {
		return instance{}, false, nil
	}
	// `cvms get` returns the CVM either as a flat object or nested under a cvm
	// key; deployOutput's decoder already merges both, ranking the nested object
	// first. Reuse it so a name carried only on the nested object is not lost.
	var output deployOutput
	if err := json.Unmarshal([]byte(payload), &output); err != nil {
		return instance{}, false, core.Exit(5, "parse phala cvms get output: %v", err)
	}
	item := output.Top
	if output.CVM != nil && output.CVM.cloudID() != "" {
		item = *output.CVM
	}
	if item.cloudID() == "" {
		return instance{}, false, nil
	}
	item.Labels = phalaLabels(item, cfg)
	return item, true, nil
}

// gatewayGetOutput models the subset of `phala cvms get --cvm-id <id> --json`
// crabbox needs to derive the TLS SSH gateway host: the CVM app id and the
// gateway base domain. The payload is snake_case and may carry the gateway both
// nested under a gateway object and at the top level on some CLI versions, so
// every spelling is decoded and the first non-blank wins. This mirrors the
// proxy-side resolver (resolvePhalaProxyHost) field preference exactly so the
// cached host and the proxy-resolved fallback host are identical.
type gatewayGetOutput struct {
	AppID         string
	AppIDAlt      string
	ID            string
	InstanceID    string
	GatewayDomain string
	BaseDomain    string
	Domain        string
	TopGateway    string
	CVM           *gatewayGetOutput
}

func (g *gatewayGetOutput) UnmarshalJSON(data []byte) error {
	var raw struct {
		AppID         string          `json:"app_id"`
		AppIDAlt      string          `json:"appId"`
		ID            string          `json:"id"`
		InstanceID    string          `json:"instance_id"`
		GatewayDomain string          `json:"gateway_domain"`
		CVM           json.RawMessage `json:"cvm"`
		Gateway       struct {
			GatewayDomain string `json:"gateway_domain"`
			BaseDomain    string `json:"base_domain"`
			Domain        string `json:"domain"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	g.AppID = raw.AppID
	g.AppIDAlt = raw.AppIDAlt
	g.ID = raw.ID
	g.InstanceID = raw.InstanceID
	g.GatewayDomain = raw.Gateway.GatewayDomain
	g.BaseDomain = raw.Gateway.BaseDomain
	g.Domain = raw.Gateway.Domain
	g.TopGateway = raw.GatewayDomain
	if len(raw.CVM) > 0 && string(raw.CVM) != "null" {
		nested := &gatewayGetOutput{}
		if err := json.Unmarshal(raw.CVM, nested); err != nil {
			return err
		}
		g.CVM = nested
	}
	return nil
}

// appID returns the CVM app id used as the `<appId>-22` host label, preferring
// the canonical app_id then its camelCase alias, then id/instance_id, falling
// through to the nested cvm object. This fallback order mirrors the proxy-side
// phalaCVM.appID() EXACTLY so the cached host and the proxy-resolved fallback
// host are identical (the gateway domain preference already matches).
func (g *gatewayGetOutput) appID() string {
	id := firstNonBlank(g.AppID, g.AppIDAlt, g.ID, g.InstanceID)
	if id == "" && g.CVM != nil {
		id = g.CVM.appID()
	}
	return id
}

// gatewayDomain returns the gateway base domain, preferring gateway_domain then
// the nested base_domain/domain, then a top-level gateway_domain, falling
// through to the nested cvm object. This preference matches resolvePhalaProxyHost.
func (g *gatewayGetOutput) gatewayDomain() string {
	domain := firstNonBlank(g.GatewayDomain, g.BaseDomain, g.Domain, g.TopGateway)
	if domain == "" && g.CVM != nil {
		domain = g.CVM.gatewayDomain()
	}
	return domain
}

// resolveGatewayHost queries `phala cvms get --cvm-id <id> --json` once and
// derives the cached TLS SSH gateway host `<appId>-22.<gateway-domain>`. Callers
// treat it best-effort: a not-found CVM or a payload missing the app id/domain
// returns ("", nil) so the per-connection proxy fallback still applies.
func (b *backend) resolveGatewayHost(ctx context.Context, id string) (string, error) {
	cfg := b.configForRun()
	result, err := b.phala(ctx, cfg, []string{"cvms", "get", "--cvm-id", id, "--json"}, nil)
	if err != nil {
		// Best-effort: a definitively missing CVM yields ("", nil) so SSH falls back
		// to per-connection resolution. Scan only the CLI's own output, mirroring
		// getInstance, so a transient transport error is surfaced rather than masked.
		if missingCVMResponse(result.Stdout, result.Stderr) {
			return "", nil
		}
		return "", commandError("phala cvms get", result, err)
	}
	payload := jsonObjectPrefix(result.Stdout)
	if payload == "" {
		return "", nil
	}
	var parsed gatewayGetOutput
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return "", core.Exit(5, "parse phala cvms get output: %v", err)
	}
	appID := parsed.appID()
	domain := parsed.gatewayDomain()
	if appID == "" || domain == "" {
		return "", nil
	}
	return appID + "-22." + domain, nil
}

func (b *backend) validateDestroyTarget(ctx context.Context, cfg core.Config, id, leaseID string) (bool, error) {
	// Phala has no server-side ownership label: the local lease claim is the
	// destructive-op authority, so require a matching claim before any CLI call.
	if _, ok, err := resolvePhalaClaim(leaseID, cfg); err != nil {
		return false, err
	} else if !ok {
		return false, core.Exit(4, "refusing to destroy Phala CVM %s: no local claim for lease %s", id, leaseID)
	}
	// Source the CVM (and its name) from `cvms get`, not `cvms list`: real
	// `cvms list` omits the name, so the name-based ownership corroboration below
	// can only succeed off the `cvms get` payload.
	item, found, err := b.getInstance(ctx, id)
	if err != nil {
		return false, err
	}
	if !found {
		// Already gone: nothing to destroy, no error.
		return false, nil
	}
	// Corroborate ownership against the CVM name: a foreign CVM carries no
	// crabbox- prefix, and a crabbox- CVM for a DIFFERENT lease must not be
	// deleted under this claim.
	nameLease := leaseIDFromName(item.Name)
	if nameLease == "" {
		return false, core.Exit(4, "refusing to destroy Phala CVM %s without Crabbox ownership labels", id)
	}
	if nameLease != leaseID {
		return false, core.Exit(4, "refusing to destroy Phala CVM %s: lease %q does not match %q", id, nameLease, leaseID)
	}
	return true, nil
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
