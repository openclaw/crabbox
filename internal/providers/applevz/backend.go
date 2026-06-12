package applevz

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/openclaw/crabbox/internal/applevzhelper"
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	defaultUser               = "crabbox"
	defaultWorkRoot           = "/work/crabbox"
	defaultCPUs               = 4
	defaultMemoryMiB          = 8192
	defaultDiskGiB            = 30
	managedHelperDigestSuffix = ".digests.json"
	diagnosticTailBytes       = 8 * 1024
	rollbackTimeout           = 30 * time.Second
	helperCancelGracePeriod   = 30 * time.Second
	unclaimedInstanceGrace    = 3 * time.Hour
	managedHelperRecentGrace  = time.Hour
	managedHelperKeepVersions = 4
)

var (
	hostGOOS         = runtime.GOOS
	hostGOARCH       = runtime.GOARCH
	hostMacOSVersion = readHostMacOSVersion
)

type backend struct {
	spec                  core.ProviderSpec
	cfg                   core.Config
	rt                    core.Runtime
	prepareHelper         func(context.Context, core.Config) (string, error)
	prepareExistingHelper func(context.Context, core.Config) (string, error)
	stateRoot             func() (string, error)
	waitForSSH            func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error
}

type managedHelperDigests struct {
	SourceSHA256       string `json:"sourceSHA256"`
	ManagedSHA256      string `json:"managedSHA256"`
	EntitlementsSHA256 string `json:"entitlementsSHA256"`
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	applyDefaults(&cfg)
	b := &backend{spec: spec, cfg: cfg, rt: rt}
	b.prepareHelper = b.ensureHelperBinary
	b.prepareExistingHelper = b.ensureExistingHelperBinary
	b.stateRoot = b.appleVZStateRoot
	b.waitForSSH = core.WaitForSSHReady
	return b
}

func applyDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.TargetOS == core.TargetLinux {
		cfg.WindowsMode = ""
	}
	cfg.SSHFallbackPorts = []string{}
	if cfg.AppleVZ.Image == "" {
		cfg.AppleVZ.Image = defaultAppleVZImage(cfg.OSImage)
	}
	if cfg.AppleVZ.ImageSHA256 == "" && cfg.AppleVZ.Image == defaultAppleVZImage(cfg.OSImage) {
		cfg.AppleVZ.ImageSHA256 = defaultAppleVZImageSHA256(cfg.OSImage)
	}
	if cfg.AppleVZ.User == "" {
		cfg.AppleVZ.User = defaultUser
	}
	if cfg.AppleVZ.WorkRoot == "" {
		if workRoot := strings.TrimSpace(cfg.WorkRoot); workRoot != "" && workRoot != defaultWorkRoot {
			cfg.AppleVZ.WorkRoot = workRoot
		} else {
			cfg.AppleVZ.WorkRoot = defaultWorkRoot
		}
	} else if workRoot := strings.TrimSpace(cfg.WorkRoot); workRoot != "" && workRoot != defaultWorkRoot && cfg.AppleVZ.WorkRoot == defaultWorkRoot {
		cfg.AppleVZ.WorkRoot = workRoot
	}
	if cfg.AppleVZ.CPUs == 0 && !core.AppleVZCPUsExplicit(*cfg) {
		cfg.AppleVZ.CPUs = defaultCPUs
	}
	if cfg.AppleVZ.MemoryMiB == 0 && !core.AppleVZMemoryExplicit(*cfg) {
		cfg.AppleVZ.MemoryMiB = defaultMemoryMiB
	}
	if cfg.AppleVZ.DiskGiB == 0 && !core.AppleVZDiskExplicit(*cfg) {
		cfg.AppleVZ.DiskGiB = defaultDiskGiB
	}
	cfg.SSHUser = cfg.AppleVZ.User
	cfg.SSHPort = strconv.Itoa(int(applevzhelper.GuestSSHPort))
	cfg.WorkRoot = cfg.AppleVZ.WorkRoot
	cfg.ServerType = applevzhelper.ImageIdentity(cfg.AppleVZ.Image, cfg.AppleVZ.ImageSHA256)
}

func defaultAppleVZImage(osImage string) string {
	if image, err := core.OSImageDefaultAppleVZImage(osImage); err == nil && strings.TrimSpace(image) != "" {
		return image
	}
	return "https://cloud-images.ubuntu.com/releases/resolute/release-20260520/ubuntu-26.04-server-cloudimg-arm64.img"
}

func defaultAppleVZImageSHA256(osImage string) string {
	if checksum, err := core.OSImageDefaultAppleVZSHA256(osImage); err == nil && strings.TrimSpace(checksum) != "" {
		return checksum
	}
	return "5e091e27d60116efbb0c743b8dd5cb2d15618e414ef04db0817ed43c8e2d7c7b"
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
	if err := requireHost(); err != nil {
		return core.LeaseTarget{}, err
	}
	instances, err := b.listInstances(ctx, cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(instances))
	for _, inst := range instances {
		servers = append(servers, b.serverFromInstance(inst, claims[inst.Name], cfg))
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	name := core.LeaseProviderName(leaseID, slug)
	rollback := func(cause error) error {
		rollbackErr, cleaned := b.rollbackInstance(cfg, name, leaseID, cause)
		if !cleaned {
			cleanupKey = false
		}
		return rollbackErr
	}
	displayImage := applevzhelper.ImageIdentity(cfg.AppleVZ.Image, cfg.AppleVZ.ImageSHA256)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s image=%s cpus=%d memory=%dMiB disk=%dGiB keep=%v\n", providerName, leaseID, slug, displayImage, cfg.AppleVZ.CPUs, cfg.AppleVZ.MemoryMiB, cfg.AppleVZ.DiskGiB, req.Keep)
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, providerName, instanceScope(name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	inst, err := b.startInstance(ctx, cfg, name, leaseID, slug, publicKey)
	if err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", req.Keep, time.Now().UTC())
	labels["instance"] = name
	labels["image"] = displayImage
	labels["server_type"] = displayImage
	labels["ssh_user"] = cfg.AppleVZ.User
	if inst.SSHPort > 0 {
		labels["ssh_port"] = strconv.Itoa(inst.SSHPort)
	}
	labels["work_root"] = cfg.AppleVZ.WorkRoot
	claim := core.LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: instanceScope(name), Labels: labels}
	lease, err := b.prepareLease(ctx, cfg, inst, claim, true)
	if err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, instanceScope(name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, lease.Server, lease.SSH); err != nil {
		return core.LeaseTarget{}, rollback(err)
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, name)
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	if err := requireHost(); err != nil {
		return core.LeaseTarget{}, err
	}
	inst, claim, err := b.resolveInstance(ctx, cfg, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := firstNonBlank(claim.LeaseID, inst.LeaseID)
	slug := firstNonBlank(claim.Slug, inst.Slug)
	claim.LeaseID = leaseID
	claim.Slug = slug
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: b.serverFromInstance(inst, claim, cfg), LeaseID: leaseID}, nil
	}
	if leaseID == "" {
		return core.LeaseTarget{}, exit(4, "apple-vz instance %q has no Crabbox lease metadata; stop it with `crabbox stop --provider apple-vz %s`", inst.Name, inst.Name)
	}
	if !appleVZRunning(inst.Status) && !req.StatusOnly {
		return core.LeaseTarget{}, exit(5, "apple-vz instance %s is %s; start a new lease with `crabbox run` or clean it up with `crabbox cleanup --provider apple-vz`", inst.Name, core.Blank(inst.Status, "stopped"))
	}
	if req.StatusOnly && (inst.SSHHost == "" || inst.SSHPort <= 0) {
		return core.LeaseTarget{Server: b.serverFromInstance(inst, claim, cfg), LeaseID: leaseID}, nil
	}
	lease, err := b.prepareLease(ctx, cfg, inst, claim, false)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, instanceScope(inst.Name), cfg.Pond, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, lease.Server, lease.SSH); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	cfg := b.configForRun()
	if err := requireHost(); err != nil {
		return nil, err
	}
	instances, err := b.listInstances(ctx, cfg)
	if err != nil {
		return nil, err
	}
	claims, err := providerClaims()
	if err != nil {
		return nil, err
	}
	views := make([]core.LeaseView, 0, len(instances))
	for _, inst := range instances {
		views = append(views, b.serverFromInstance(inst, claims[inst.Name], cfg))
	}
	return views, nil
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.configForRun()
	if err := requireHost(); err != nil {
		return core.DoctorResult{}, err
	}
	root, err := b.stateRoot()
	if err != nil {
		return core.DoctorResult{}, err
	}
	helperPath, err := b.prepareHelper(ctx, cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	var resp applevzhelper.DoctorResponse
	if err := b.runHelperJSONInput(ctx, helperPath, []string{"doctor", "--state-root", root, "--image-request-stdin"}, applevzhelper.ImageRequest{
		Image:  cfg.AppleVZ.Image,
		SHA256: cfg.AppleVZ.ImageSHA256,
	}, &resp); err != nil {
		return core.DoctorResult{}, err
	}
	if strings.TrimSpace(resp.Status) != "ok" {
		return core.DoctorResult{}, exit(2, "apple-vz doctor failed: %s", core.Blank(resp.Message, "unknown error"))
	}
	if image := strings.TrimSpace(resp.Details["image"]); image != "" {
		resp.Details["image"] = applevzhelper.RedactImageRef(image)
	}
	runtimeLabel := core.Blank(resp.Details["runtime"], "ready")
	msg := fmt.Sprintf("helper=ready control_plane=local inventory=ready api=list mutation=false leases=%d runtime=%s image=%s path=%s", resp.Instances, runtimeLabel, applevzhelper.ImageIdentity(cfg.AppleVZ.Image, cfg.AppleVZ.ImageSHA256), helperPath)
	return core.DoctorResult{
		Provider: providerName,
		Message:  msg,
		Checks: []core.DoctorCheck{
			{Status: "ok", Check: "host", Message: "Apple Silicon macOS ready", Details: map[string]string{"host": hostGOOS + "/" + hostGOARCH}},
			{Status: "ok", Check: "helper", Message: "helper ready", Details: map[string]string{"path": helperPath}},
			{Status: "ok", Check: "runtime", Message: core.Blank(resp.Message, "runtime ready"), Details: resp.Details},
		},
	}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	cfg := b.configForRun()
	if err := requireHost(); err != nil {
		return err
	}
	leaseID := firstNonBlank(req.Lease.LeaseID, req.Lease.Server.Labels["lease"])
	name := strings.TrimSpace(firstNonBlank(req.Lease.Server.CloudID, req.Lease.Server.Labels["instance"]))
	if name == "" && leaseID != "" {
		inst, claim, err := b.resolveInstance(ctx, cfg, leaseID)
		if err != nil {
			var missing *missingInstanceError
			if !errors.As(err, &missing) {
				return err
			}
			leaseID = firstNonBlank(leaseID, claim.LeaseID)
		} else {
			name = inst.Name
			leaseID = firstNonBlank(leaseID, claim.LeaseID, inst.LeaseID)
		}
	}
	if name != "" {
		if err := b.deleteInstance(ctx, cfg, name); err != nil {
			return err
		}
	}
	if leaseID != "" {
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
	}
	if name == "" && leaseID == "" {
		return exit(2, "provider=%s release requires an apple-vz instance name or lease id", providerName)
	}
	return nil
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("released lease=%s instance=%s", lease.LeaseID, core.Blank(firstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]), "-"))
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	if err := requireHost(); err != nil {
		return err
	}
	instances, err := b.listInstances(ctx, cfg)
	if err != nil {
		return err
	}
	claims, err := providerClaims()
	if err != nil {
		return err
	}
	live := map[string]struct{}{}
	now := time.Now().UTC()
	removed := 0
	for _, inst := range instances {
		claim, hasClaim := claims[inst.Name]
		leaseID := firstNonBlank(claim.LeaseID, inst.LeaseID)
		if hasClaim && claim.LeaseID != "" {
			live[claim.LeaseID] = struct{}{}
			claim.LeaseID = leaseID
		}
		server := b.serverFromInstance(inst, claim, cfg)
		shouldDelete, reason := shouldCleanup(inst, server, claim, hasClaim, now)
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip instance name=%s reason=%s\n", inst.Name, reason)
			continue
		}
		if !hasClaim && leaseID != "" {
			refreshed, claimExists, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
			if err != nil {
				return fmt.Errorf("recheck apple-vz claim %s before cleanup: %w", leaseID, err)
			}
			if claimExists {
				if refreshed.LeaseID != "" {
					live[refreshed.LeaseID] = struct{}{}
				}
				fmt.Fprintf(b.rt.Stderr, "skip instance name=%s reason=claim appeared during cleanup\n", inst.Name)
				continue
			}
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove instance name=%s lease=%s reason=%s\n", inst.Name, core.Blank(leaseID, "-"), reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove instance name=%s lease=%s reason=%s\n", inst.Name, core.Blank(leaseID, "-"), reason)
		if err := b.deleteInstance(ctx, cfg, inst.Name); err != nil {
			return err
		}
		if leaseID != "" {
			core.RemoveLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
		}
		removed++
	}
	claimsRemoved := 0
	for _, claim := range claims {
		if claim.LeaseID == "" {
			continue
		}
		if _, ok := live[claim.LeaseID]; ok {
			continue
		}
		if claimWithinStartupGrace(claim, now) {
			fmt.Fprintf(b.rt.Stderr, "skip claim lease=%s reason=startup grace period\n", claim.LeaseID)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove claim lease=%s reason=missing instance\n", claim.LeaseID)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "remove claim lease=%s reason=missing instance\n", claim.LeaseID)
		core.RemoveLeaseClaim(claim.LeaseID)
		core.RemoveStoredTestboxKey(claim.LeaseID)
		claimsRemoved++
	}
	if !req.DryRun {
		fmt.Fprintf(b.rt.Stdout, "%s cleanup removed=%d claims_removed=%d checked=%d\n", providerName, removed, claimsRemoved, len(instances))
	}
	return nil
}

func (b *backend) Touch(_ context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	original := server.Labels
	server.Labels = core.TouchDirectLeaseLabels(original, b.configForRun(), req.State, time.Now().UTC())
	for _, key := range []string{"image", "instance", "server_type", "ssh_user", "ssh_port", "work_root"} {
		if value := strings.TrimSpace(original[key]); value != "" {
			server.Labels[key] = value
		}
	}
	return server, nil
}

func (b *backend) prepareLease(ctx context.Context, cfg core.Config, inst applevzhelper.Instance, claim core.LeaseClaim, wait bool) (core.LeaseTarget, error) {
	server := b.serverFromInstance(inst, claim, cfg)
	user := strings.TrimSpace(server.Labels["ssh_user"])
	if user != "" {
		cfg.AppleVZ.User = user
		cfg.SSHUser = user
	}
	root := strings.TrimSpace(server.Labels["work_root"])
	if root != "" {
		cfg.AppleVZ.WorkRoot = root
		cfg.WorkRoot = root
	}
	leaseID := firstNonBlank(claim.LeaseID, inst.LeaseID)
	if !appleVZRunning(inst.Status) {
		server.Status = appleVZState(inst.Status)
		server.Labels["state"] = server.Status
		return core.LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	if inst.SSHHost == "" || inst.SSHPort <= 0 {
		return core.LeaseTarget{}, exit(5, "apple-vz instance %s has no local SSH endpoint", inst.Name)
	}
	if leaseID != "" {
		if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
			if _, statErr := os.Stat(keyPath); statErr == nil {
				cfg.SSHKey = keyPath
			}
		}
	}
	target := core.SSHTargetFromConfig(cfg, inst.SSHHost)
	target.Port = strconv.Itoa(inst.SSHPort)
	target.FallbackPorts = []string{}
	target.ReadyCheck = "/usr/local/bin/crabbox-ready"
	if wait {
		if err := b.waitForSSH(ctx, &target, b.rt.Stderr, "apple-vz ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) startInstance(ctx context.Context, cfg core.Config, name, leaseID, slug, publicKey string) (applevzhelper.Instance, error) {
	helperPath, err := b.prepareHelper(ctx, cfg)
	if err != nil {
		return applevzhelper.Instance{}, err
	}
	root, err := b.stateRoot()
	if err != nil {
		return applevzhelper.Instance{}, err
	}
	var resp applevzhelper.StartResponse
	if err := b.runHelperJSONInput(ctx, helperPath, []string{
		"start",
		"--state-root", root,
		"--name", name,
		"--lease-id", leaseID,
		"--slug", slug,
		"--image-request-stdin",
		"--ssh-user", cfg.AppleVZ.User,
		"--ssh-public-key", publicKey,
		"--work-root", cfg.AppleVZ.WorkRoot,
		"--cpus", strconv.Itoa(cfg.AppleVZ.CPUs),
		"--memory-mib", strconv.Itoa(cfg.AppleVZ.MemoryMiB),
		"--disk-gib", strconv.Itoa(cfg.AppleVZ.DiskGiB),
		"--ready-timeout", core.BootstrapWaitTimeout(cfg).String(),
	}, applevzhelper.ImageRequest{
		Image:  cfg.AppleVZ.Image,
		SHA256: cfg.AppleVZ.ImageSHA256,
	}, &resp); err != nil {
		return applevzhelper.Instance{}, err
	}
	return resp.Instance, nil
}

func (b *backend) rollbackInstance(cfg core.Config, name, leaseID string, cause error) (error, bool) {
	root, err := b.stateRoot()
	if err != nil {
		return appendErrorDetails(cause, fmt.Errorf("resolve apple-vz state for rollback: %w", err)), false
	}
	cause = appendErrorDetails(cause, instanceDiagnostics(root, name))
	if _, err := os.Stat(applevzhelper.InstanceDir(root, name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			core.RemoveLeaseClaim(leaseID)
			return cause, true
		}
		return appendErrorDetails(cause, fmt.Errorf("inspect apple-vz instance %s for rollback: %w", name, err)), false
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	if err := b.deleteInstance(cleanupCtx, cfg, name); err != nil {
		return appendErrorDetails(cause, fmt.Errorf("apple-vz cleanup failed for instance %s: %w", name, err)), false
	}
	core.RemoveLeaseClaim(leaseID)
	return cause, true
}

func appendErrorDetails(cause error, details ...error) error {
	combined := errors.Join(append([]error{cause}, details...)...)
	var exitErr core.ExitError
	if core.AsExitError(cause, &exitErr) {
		return core.Exit(exitErr.Code, "%s", combined.Error())
	}
	return combined
}

func instanceDiagnostics(stateRoot, name string) error {
	parts := make([]string, 0, 2)
	for _, log := range []struct {
		label string
		path  string
	}{
		{label: applevzhelper.HelperLogFileName, path: applevzhelper.HelperLogPath(stateRoot, name)},
		{label: applevzhelper.ConsoleLogFileName, path: applevzhelper.ConsoleLogPath(stateRoot, name)},
	} {
		tail, err := readFileTail(log.path, diagnosticTailBytes)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				parts = append(parts, fmt.Sprintf("%s unavailable: %v", log.label, err))
			}
			continue
		}
		if tail != "" {
			parts = append(parts, fmt.Sprintf("%s tail:\n%s", log.label, applevzhelper.SanitizeDiagnosticText(tail)))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return fmt.Errorf("apple-vz diagnostics for %s:\n%s", name, strings.Join(parts, "\n"))
}

func readFileTail(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	offset := info.Size() - limit
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (b *backend) deleteInstance(ctx context.Context, cfg core.Config, name string) error {
	helperPath, err := b.prepareExistingHelper(ctx, cfg)
	if err != nil {
		return err
	}
	root, err := b.stateRoot()
	if err != nil {
		return err
	}
	var resp applevzhelper.DeleteResponse
	if err := b.runHelperJSON(ctx, helperPath, []string{"delete", "--state-root", root, "--name", name}, &resp); err != nil {
		return err
	}
	if !resp.Deleted {
		if _, err := os.Stat(applevzhelper.InstanceDir(root, name)); err == nil {
			return fmt.Errorf("apple-vz helper did not delete instance %s", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("verify apple-vz instance %s deletion: %w", name, err)
		}
	}
	return nil
}

func (b *backend) listInstances(ctx context.Context, cfg core.Config) ([]applevzhelper.Instance, error) {
	helperPath, err := b.prepareExistingHelper(ctx, cfg)
	if err != nil {
		return nil, err
	}
	root, err := b.stateRoot()
	if err != nil {
		return nil, err
	}
	var resp applevzhelper.ListResponse
	if err := b.runHelperJSON(ctx, helperPath, []string{"list", "--state-root", root}, &resp); err != nil {
		return nil, err
	}
	return resp.Instances, nil
}

func (b *backend) resolveInstance(ctx context.Context, cfg core.Config, identifier string) (applevzhelper.Instance, core.LeaseClaim, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return applevzhelper.Instance{}, core.LeaseClaim{}, exit(2, "provider=%s requires a lease id, slug, or instance name", providerName)
	}
	instances, err := b.listInstances(ctx, cfg)
	if err != nil {
		return applevzhelper.Instance{}, core.LeaseClaim{}, err
	}
	claims, err := providerClaims()
	if err != nil {
		return applevzhelper.Instance{}, core.LeaseClaim{}, err
	}
	for _, inst := range instances {
		claim := claims[inst.Name]
		if inst.Name == identifier || inst.LeaseID == identifier || inst.Slug == identifier || claim.LeaseID == identifier || claim.Slug == identifier {
			return inst, claim, nil
		}
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName); err == nil && ok {
		return applevzhelper.Instance{}, claim, &missingInstanceError{
			err: exit(4, "apple-vz lease %q points to a missing instance; run `crabbox cleanup --provider apple-vz`", identifier),
		}
	}
	return applevzhelper.Instance{}, core.LeaseClaim{}, exit(4, "apple-vz lease not found: %s", identifier)
}

type missingInstanceError struct {
	err core.ExitError
}

func (e *missingInstanceError) Error() string {
	return e.err.Error()
}

func (e *missingInstanceError) Unwrap() error {
	return e.err
}

func (b *backend) serverFromInstance(inst applevzhelper.Instance, claim core.LeaseClaim, cfg core.Config) core.Server {
	labels := map[string]string{}
	for key, value := range claim.Labels {
		labels[key] = value
	}
	if labels["crabbox"] == "" {
		labels["crabbox"] = "true"
	}
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["instance"] == "" {
		labels["instance"] = inst.Name
	}
	if labels["lease"] == "" {
		labels["lease"] = firstNonBlank(claim.LeaseID, inst.LeaseID)
	}
	if labels["slug"] == "" {
		labels["slug"] = firstNonBlank(claim.Slug, inst.Slug)
	}
	imageIdentity := applevzhelper.ImageIdentity(cfg.AppleVZ.Image, cfg.AppleVZ.ImageSHA256)
	if labels["server_type"] == "" {
		labels["server_type"] = firstNonBlank(inst.Image, imageIdentity)
	}
	labels["server_type"] = applevzhelper.RedactImageRef(labels["server_type"])
	if labels["image"] == "" {
		labels["image"] = firstNonBlank(inst.Image, imageIdentity)
	}
	labels["image"] = applevzhelper.RedactImageRef(labels["image"])
	if labels["ssh_user"] == "" {
		labels["ssh_user"] = firstNonBlank(inst.SSHUser, cfg.AppleVZ.User)
	}
	if inst.SSHPort > 0 {
		labels["ssh_port"] = strconv.Itoa(inst.SSHPort)
	} else if labels["ssh_port"] == "" {
		labels["ssh_port"] = cfg.SSHPort
	}
	if labels["work_root"] == "" {
		labels["work_root"] = firstNonBlank(inst.WorkRoot, cfg.AppleVZ.WorkRoot)
	}
	status := appleVZState(inst.Status)
	if appleVZRunning(inst.Status) && labels["state"] == "ready" {
		status = "ready"
	}
	labels["state"] = status
	server := core.Server{
		CloudID:  inst.Name,
		Provider: providerName,
		Name:     inst.Name,
		Status:   status,
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = inst.SSHHost
	server.ServerType.Name = applevzhelper.RedactImageRef(firstNonBlank(labels["server_type"], imageIdentity))
	return server
}

func (b *backend) runHelperJSON(ctx context.Context, helperPath string, args []string, out any) error {
	return b.runHelperJSONInput(ctx, helperPath, args, nil, out)
}

func (b *backend) runHelperJSONInput(ctx context.Context, helperPath string, args []string, input, out any) error {
	var stdin io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return exit(2, "encode apple-vz helper input: %v", err)
		}
		stdin = strings.NewReader(string(data))
	}
	result, err := b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:              helperPath,
		Args:              args,
		Env:               appleVZHelperEnv(helperPath),
		Stdin:             stdin,
		CancelGracePeriod: helperCancelGracePeriod,
	})
	if err != nil {
		return exit(2, "apple-vz helper %s failed: %s", strings.Join(args, " "), localCommandDetail(result, err))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(result.Stdout), out); err != nil {
		return exit(2, "apple-vz helper %s returned invalid JSON: %v", strings.Join(args, " "), err)
	}
	return nil
}

func appleVZHelperEnv(helperPath string) []string {
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin",
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
	}
	for _, name := range []string{
		"HOME",
		"TMPDIR",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",
		"http_proxy",
		"https_proxy",
		"no_proxy",
		"SSL_CERT_FILE",
		"SSL_CERT_DIR",
	} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			env = append(env, name+"="+value)
		}
	}
	if isManagedHelperPath(helperPath) {
		env = append(env, applevzhelper.ManagedHelperUseLockEnv+"="+applevzhelper.ManagedHelperUseLockPath(helperPath))
	}
	return env
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func (b *backend) appleVZStateRoot() (string, error) {
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(stateDir, "apple-vz")
	if err := ensurePrivateDir(root); err != nil {
		return "", exit(2, "create apple-vz state directory: %v", err)
	}
	return root, nil
}

func (b *backend) ensureHelperBinary(ctx context.Context, cfg core.Config) (_ string, returnErr error) {
	root, err := b.stateRoot()
	if err != nil {
		return "", err
	}
	helperDir := applevzhelper.HelperDir(root)
	if err := ensurePrivateDir(helperDir); err != nil {
		return "", exit(2, "create apple-vz helper directory: %v", err)
	}
	installLock, err := lockManagedHelperDir(helperDir)
	if err != nil {
		return "", exit(2, "lock apple-vz helper directory: %v", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, unlockManagedHelperDir(installLock))
	}()
	sourcePath, err := resolveHelperSourcePath(cfg)
	if err != nil {
		return "", err
	}
	sourceDigest, err := fileSHA256(sourcePath)
	if err != nil {
		return "", exit(2, "hash apple-vz helper %s: %v", sourcePath, err)
	}
	entitlementsDigest := dataSHA256([]byte(applevzhelper.HelperEntitlements))
	managedPath := managedHelperInstallPath(helperDir, sourceDigest, entitlementsDigest)
	digestPath := managedHelperDigestPath(managedPath)
	if managedHelperCurrent(managedPath, digestPath, sourceDigest, entitlementsDigest) {
		if err := touchAndCleanupManagedHelpers(helperDir, managedPath); err != nil {
			return "", exit(2, "clean apple-vz helper directory: %v", err)
		}
		return managedPath, nil
	}
	stagedPath, stagedDigest, err := stageHelperBinary(sourcePath, helperDir)
	if err != nil {
		return "", err
	}
	defer os.Remove(stagedPath)
	if stagedDigest != sourceDigest {
		sourceDigest = stagedDigest
		managedPath = managedHelperInstallPath(helperDir, sourceDigest, entitlementsDigest)
		digestPath = managedHelperDigestPath(managedPath)
		if managedHelperCurrent(managedPath, digestPath, sourceDigest, entitlementsDigest) {
			if err := touchAndCleanupManagedHelpers(helperDir, managedPath); err != nil {
				return "", exit(2, "clean apple-vz helper directory: %v", err)
			}
			return managedPath, nil
		}
	}
	entitlementsPath := managedHelperEntitlementsPath(helperDir, entitlementsDigest)
	if err := writeAtomicFile(entitlementsPath, []byte(applevzhelper.HelperEntitlements), 0o644); err != nil {
		return "", exit(2, "write apple-vz entitlements: %v", err)
	}
	defer os.Remove(entitlementsPath)
	result, err := b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name: "codesign",
		Args: []string{"--force", "--sign", "-", "--entitlements", entitlementsPath, stagedPath},
	})
	if err != nil {
		return "", exit(2, "codesign apple-vz helper: %s", localCommandDetail(result, err))
	}
	if err := os.Rename(stagedPath, managedPath); err != nil {
		return "", exit(2, "install apple-vz helper %s: %v", managedPath, err)
	}
	managedDigest, err := fileSHA256(managedPath)
	if err != nil {
		return "", exit(2, "hash managed apple-vz helper: %v", err)
	}
	digestData, err := json.MarshalIndent(managedHelperDigests{
		SourceSHA256:       sourceDigest,
		ManagedSHA256:      managedDigest,
		EntitlementsSHA256: entitlementsDigest,
	}, "", "  ")
	if err != nil {
		return "", exit(2, "encode apple-vz helper digests: %v", err)
	}
	if err := writeAtomicFile(digestPath, append(digestData, '\n'), 0o644); err != nil {
		return "", exit(2, "write apple-vz helper digest: %v", err)
	}
	if err := touchAndCleanupManagedHelpers(helperDir, managedPath); err != nil {
		return "", exit(2, "clean apple-vz helper directory: %v", err)
	}
	return managedPath, nil
}

func (b *backend) ensureExistingHelperBinary(ctx context.Context, cfg core.Config) (string, error) {
	path, prepareErr := b.ensureHelperBinary(ctx, cfg)
	if prepareErr == nil {
		return path, nil
	}
	path, fallbackErr := b.latestValidManagedHelper()
	if fallbackErr != nil {
		return "", errors.Join(prepareErr, exit(2, "find existing apple-vz managed helper: %v", fallbackErr))
	}
	if path != "" {
		return path, nil
	}
	return "", prepareErr
}

func (b *backend) latestValidManagedHelper() (_ string, returnErr error) {
	root, err := b.stateRoot()
	if err != nil {
		return "", err
	}
	helperDir := applevzhelper.HelperDir(root)
	if err := ensurePrivateDir(helperDir); err != nil {
		return "", err
	}
	installLock, err := lockManagedHelperDir(helperDir)
	if err != nil {
		return "", err
	}
	defer func() {
		returnErr = errors.Join(returnErr, unlockManagedHelperDir(installLock))
	}()
	entries, err := os.ReadDir(helperDir)
	if err != nil {
		return "", err
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		path := filepath.Join(helperDir, entry.Name())
		if entry.IsDir() || !isManagedHelperPath(path) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		candidates = append(candidates, candidate{path: path, modTime: info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	for _, candidate := range candidates {
		if !managedHelperValid(candidate.path) {
			continue
		}
		if err := touchAndCleanupManagedHelpers(helperDir, candidate.path); err != nil {
			return "", err
		}
		return candidate.path, nil
	}
	return "", nil
}

func managedHelperValid(managedPath string) bool {
	data, err := os.ReadFile(managedHelperDigestPath(managedPath))
	if err != nil {
		return false
	}
	var digests managedHelperDigests
	if err := json.Unmarshal(data, &digests); err != nil ||
		digests.SourceSHA256 == "" ||
		digests.ManagedSHA256 == "" ||
		digests.EntitlementsSHA256 == "" {
		return false
	}
	if managedHelperInstallPath(filepath.Dir(managedPath), digests.SourceSHA256, digests.EntitlementsSHA256) != managedPath {
		return false
	}
	managedDigest, err := fileSHA256(managedPath)
	return err == nil && managedDigest == digests.ManagedSHA256
}

func managedHelperInstallPath(helperDir, sourceDigest, entitlementsDigest string) string {
	key := dataSHA256([]byte(sourceDigest + "\x00" + entitlementsDigest))
	return filepath.Join(helperDir, applevzhelper.ManagedHelperName+"-"+key)
}

func managedHelperDigestPath(managedPath string) string {
	return managedPath + managedHelperDigestSuffix
}

func managedHelperEntitlementsPath(helperDir, entitlementsDigest string) string {
	return filepath.Join(helperDir, "apple-vz-"+entitlementsDigest+".entitlements")
}

func lockManagedHelperDir(helperDir string) (*flock.Flock, error) {
	path := filepath.Join(helperDir, ".install.lock")
	lock := flock.New(path, flock.SetPermissions(0o600))
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	return lock, nil
}

func unlockManagedHelperDir(lock *flock.Flock) error {
	if lock == nil {
		return nil
	}
	return lock.Unlock()
}

func touchAndCleanupManagedHelpers(helperDir, currentPath string) error {
	now := time.Now()
	if err := os.Chtimes(currentPath, now, now); err != nil {
		return err
	}
	return cleanupObsoleteManagedHelpers(helperDir, currentPath, now)
}

func cleanupObsoleteManagedHelpers(helperDir, currentPath string, now time.Time) error {
	entries, err := os.ReadDir(helperDir)
	if err != nil {
		return err
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	var candidates []candidate
	prefix := applevzhelper.ManagedHelperName + "-"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		digest := strings.TrimPrefix(name, prefix)
		if len(digest) != sha256.Size*2 || !isLowerHex(digest) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{path: filepath.Join(helperDir, name), modTime: info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	keep := map[string]bool{currentPath: true}
	for _, candidate := range candidates {
		if len(keep) >= managedHelperKeepVersions {
			break
		}
		keep[candidate.path] = true
	}
	cutoff := now.Add(-managedHelperRecentGrace)
	for _, candidate := range candidates {
		if keep[candidate.path] || !candidate.modTime.Before(cutoff) {
			continue
		}
		useLockPath := applevzhelper.ManagedHelperUseLockPath(candidate.path)
		useLock := flock.New(useLockPath, flock.SetPermissions(0o600))
		locked, err := useLock.TryLock()
		if err != nil {
			return err
		}
		if !locked {
			continue
		}
		if err := os.Remove(candidate.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = useLock.Unlock()
			return err
		}
		if err := os.Remove(managedHelperDigestPath(candidate.path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = useLock.Unlock()
			return err
		}
		if err := useLock.Unlock(); err != nil {
			return err
		}
		if err := os.Remove(useLockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func isManagedHelperPath(path string) bool {
	name := filepath.Base(path)
	prefix := applevzhelper.ManagedHelperName + "-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	digest := strings.TrimPrefix(name, prefix)
	return len(digest) == sha256.Size*2 && isLowerHex(digest)
}

func resolveHelperSourcePath(cfg core.Config) (string, error) {
	if helper := strings.TrimSpace(cfg.AppleVZ.HelperPath); helper != "" {
		path := core.ExpandUserPath(helper)
		if !filepath.IsAbs(path) {
			abs, err := filepath.Abs(path)
			if err != nil {
				return "", err
			}
			path = abs
		}
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		return "", exit(2, "apple-vz helper not found at %s", path)
	}
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), applevzhelper.ManagedHelperName)
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
	}
	if path, err := exec.LookPath(applevzhelper.ManagedHelperName); err == nil {
		return path, nil
	}
	return "", exit(2, "apple-vz helper binary not found. Reinstall Crabbox on Apple Silicon, put `%s` on PATH, or explicitly pass --apple-vz-helper for a source build", applevzhelper.ManagedHelperName)
}

func managedHelperCurrent(managedPath, digestPath, sourceDigest, entitlementsDigest string) bool {
	if _, err := os.Stat(managedPath); err != nil {
		return false
	}
	data, err := os.ReadFile(digestPath)
	if err != nil {
		return false
	}
	var digests managedHelperDigests
	if err := json.Unmarshal(data, &digests); err != nil ||
		digests.SourceSHA256 != sourceDigest ||
		digests.EntitlementsSHA256 != entitlementsDigest {
		return false
	}
	managedDigest, err := fileSHA256(managedPath)
	return err == nil && managedDigest == digests.ManagedSHA256
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func dataSHA256(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash[:])
}

func stageHelperBinary(sourcePath, helperDir string) (string, string, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", "", exit(2, "read apple-vz helper %s: %v", sourcePath, err)
	}
	defer source.Close()
	staged, err := os.CreateTemp(helperDir, "."+applevzhelper.ManagedHelperName+"-*")
	if err != nil {
		return "", "", exit(2, "stage apple-vz helper: %v", err)
	}
	stagedPath := staged.Name()
	cleanup := true
	defer func() {
		_ = staged.Close()
		if cleanup {
			_ = os.Remove(stagedPath)
		}
	}()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(staged, hash), source); err != nil {
		return "", "", exit(2, "copy apple-vz helper: %v", err)
	}
	if err := staged.Chmod(0o755); err != nil {
		return "", "", exit(2, "chmod apple-vz helper: %v", err)
	}
	if err := staged.Sync(); err != nil {
		return "", "", exit(2, "sync apple-vz helper: %v", err)
	}
	if err := staged.Close(); err != nil {
		return "", "", exit(2, "close apple-vz helper: %v", err)
	}
	cleanup = false
	return stagedPath, fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func writeAtomicFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	defer os.Remove(tmpPath)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func providerClaims() (map[string]core.LeaseClaim, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider == providerName {
			name := strings.TrimSpace(claim.ProviderScope)
			if name == "" {
				name = strings.TrimSpace(claim.Labels["instance"])
			}
			if name != "" {
				out[name] = claim
			}
		}
	}
	return out, nil
}

func instanceScope(name string) string { return name }

func requireHost() error {
	if hostGOOS != "darwin" || hostGOARCH != "arm64" {
		return exit(2, "provider=%s requires macOS on Apple silicon; current host is %s/%s", providerName, hostGOOS, hostGOARCH)
	}
	version, err := hostMacOSVersion()
	if err != nil {
		return exit(2, "provider=%s could not determine the macOS version: %v", providerName, err)
	}
	major, err := macOSMajorVersion(version)
	if err != nil {
		return exit(2, "provider=%s could not parse macOS version %q: %v", providerName, version, err)
	}
	if major < 13 {
		return exit(2, "provider=%s requires macOS 13 or newer for Virtualization.framework EFI support; current version is %s", providerName, version)
	}
	return nil
}

func readHostMacOSVersion() (string, error) {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", fmt.Errorf("sw_vers returned an empty product version")
	}
	return version, nil
}

func macOSMajorVersion(version string) (int, error) {
	majorText, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major <= 0 {
		return 0, fmt.Errorf("invalid major version")
	}
	return major, nil
}

func shouldCleanup(inst applevzhelper.Instance, server core.Server, claim core.LeaseClaim, hasClaim bool, now time.Time) (bool, string) {
	labels := server.Labels
	if labels == nil {
		return false, "missing labels"
	}
	if strings.EqualFold(labels["keep"], "true") {
		return false, "keep=true"
	}
	if server.Status != "ready" && !appleVZRunning(server.Status) {
		return true, "instance state=" + core.Blank(server.Status, "unknown")
	}
	if hasClaim {
		lastUsed, err := time.Parse(time.RFC3339, claim.LastUsedAt)
		if err != nil || lastUsed.IsZero() {
			return false, "claim active"
		}
		idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
		if idle <= 0 {
			return false, "claim active"
		}
		if now.After(lastUsed.Add(idle).Add(12 * time.Hour)) {
			return true, "claim expired"
		}
		return false, "claim active"
	}
	lifecycleAt := inst.CreatedAt
	if inst.UpdatedAt.After(lifecycleAt) {
		lifecycleAt = inst.UpdatedAt
	}
	if !lifecycleAt.IsZero() && now.After(lifecycleAt.Add(unclaimedInstanceGrace)) {
		return true, "missing claim beyond grace period"
	}
	return false, "missing claim within grace period"
}

func claimWithinStartupGrace(claim core.LeaseClaim, now time.Time) bool {
	var latest time.Time
	for _, value := range []string{claim.ClaimedAt, claim.LastUsedAt} {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
		if err == nil && parsed.After(latest) {
			latest = parsed
		}
	}
	return !latest.IsZero() && !now.After(latest.Add(unclaimedInstanceGrace))
}

func appleVZRunning(state string) bool {
	switch appleVZState(state) {
	case applevzhelper.StatusStarting, applevzhelper.StatusRunning, "ready":
		return true
	default:
		return false
	}
}

func appleVZState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", applevzhelper.StatusStopped:
		return "stopped"
	case applevzhelper.StatusStarting:
		return applevzhelper.StatusStarting
	case applevzhelper.StatusRunning:
		return applevzhelper.StatusRunning
	case applevzhelper.StatusStopping:
		return applevzhelper.StatusStopping
	case applevzhelper.StatusError:
		return "failed"
	case "ready":
		return "ready"
	default:
		return strings.ToLower(strings.TrimSpace(state))
	}
}

func localCommandDetail(result core.LocalCommandResult, err error) string {
	parts := []string{}
	if err != nil {
		parts = append(parts, err.Error())
	}
	if stdout := strings.TrimSpace(result.Stdout); stdout != "" {
		parts = append(parts, "stdout="+stdout)
	}
	if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
		parts = append(parts, "stderr="+stderr)
	}
	if len(parts) == 0 {
		return "no output"
	}
	return strings.Join(parts, " ")
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}
