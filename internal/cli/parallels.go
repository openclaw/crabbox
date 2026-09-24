package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const parallelsProvider = "parallels"
const parallelsDHCPLeasesPath = "/Library/Preferences/Parallels/parallels_dhcp_leases"
const parallelsPasswordEnvName = "CRABBOX_PARALLELS_PASSWORD"
const parallelsCapacityLockRetryDelay = 100 * time.Millisecond

const (
	parallelsIPBootGrace        = 2 * time.Minute
	parallelsIPProbeInterval    = 15 * time.Second
	parallelsIPProbeTimeout     = 10 * time.Second
	parallelsIPUnavailableLimit = 3
)

var errParallelsDHCPLeaseAmbiguous = errors.New("ambiguous Parallels DHCP lease")

var errParallelsDHCPLeaseMissing = errors.New("no Parallels DHCP lease for VM MACs")

var errParallelsGuestToolsUnavailable = errors.New("Parallels guest tools unavailable")

// Acquisition owns clone mode and rollback; resolving an existing VM does not.
type ParallelsIPWaitPurpose uint8

const (
	ParallelsIPWaitExisting ParallelsIPWaitPurpose = iota
	ParallelsIPWaitAcquisition
)

type ParallelsClient struct {
	Cfg    Config
	Runner CommandRunner
}

type ParallelsVM struct {
	ID           string
	Name         string
	State        string
	OS           string
	Home         string
	IP           string
	IPSource     string
	MACs         []string
	Template     bool
	SnapshotName string
}

type parallelsDHCPLease struct {
	IP        string
	MAC       string
	ExpiresAt time.Time
}

type ParallelsSnapshot struct {
	ID      string
	Name    string
	Date    string
	State   string
	Current bool
	Parent  string
}

func NewParallelsClient(cfg Config, runner CommandRunner) *ParallelsClient {
	if runner == nil {
		runner = execCommandRunner{}
	}
	return &ParallelsClient{
		Cfg:    cfg,
		Runner: commandRunnerWithChildCredentialBoundary(runner, []string{parallelsPasswordEnvName}),
	}
}

func (c *ParallelsClient) Version(ctx context.Context) (string, error) {
	result, err := c.prlctl(ctx, nil, "--version")
	return strings.TrimSpace(result.Stdout + result.Stderr), err
}

func ParallelsCandidateConfigs(cfg Config) []Config {
	if len(cfg.Parallels.Hosts) == 0 {
		return []Config{cfg}
	}
	out := make([]Config, 0, len(cfg.Parallels.Hosts))
	for _, host := range cfg.Parallels.Hosts {
		if !parallelsHostMatchesTarget(host, cfg.TargetOS) {
			continue
		}
		next := cfg
		next.Parallels.Host = host.Host
		next.Parallels.HostUser = host.User
		next.Parallels.HostKey = host.Key
		next.credentialProvenance.parallelsHost = host.hostSource
		next.credentialProvenance.parallelsHostKey = host.keySource
		if host.VMRoot != "" {
			next.Parallels.VMRoot = host.VMRoot
		}
		next.Parallels.SelectedHost = firstNonBlank(host.Name, host.Host, "local")
		out = append(out, next)
	}
	if len(out) == 0 {
		return []Config{cfg}
	}
	return out
}

// SelectParallelsFleetConfig picks a fleet host whose maxVMs still has room.
// The result is only advisory: the count it is based on is stale the moment it
// returns. Callers that go on to create a VM must use ReserveParallelsFleetCapacity
// so the count and the clone happen under one reservation.
func SelectParallelsFleetConfig(ctx context.Context, cfg Config, runner CommandRunner, source string) (Config, error) {
	selected, release, err := selectParallelsFleetConfig(ctx, cfg, runner, source, false)
	if err != nil {
		return Config{}, err
	}
	release()
	return selected, nil
}

// ReserveParallelsFleetCapacity picks a fleet host with room under maxVMs and holds
// that host's reservation lock until the returned release func runs. Capacity is
// enforced by counting the host's live crabbox- VMs and then cloning into it; with
// no reservation spanning both steps, concurrent forks all observe the same
// pre-clone count, all pass the gate, and all clone, so `crabbox shard --count 8`
// can put 8 VMs on a host configured maxVMs: 2. Callers must hold the reservation
// until their clone has completed and is visible to the next ListVMs.
//
// Reservations coordinate callers sharing a state directory and the same configured
// host/account. Display names and keys do not affect lock identity; SSH aliases are
// not resolved. Independent state directories or machines still race.
func ReserveParallelsFleetCapacity(ctx context.Context, cfg Config, runner CommandRunner, source string) (Config, func(), error) {
	return selectParallelsFleetConfig(ctx, cfg, runner, source, true)
}

// ReserveParallelsHostCapacity holds the capacity reservation for one already
// chosen host, without re-running fleet selection. A fixed lease is pinned to
// the host recorded in its durable intent and must never be moved to another
// one, so it reserves that host alone rather than shopping the fleet.
func ReserveParallelsHostCapacity(ctx context.Context, cfg Config, runner CommandRunner, source string) (func(), error) {
	_, release, err := selectParallelsFleetConfig(ctx, parallelsPinnedFleetConfig(cfg), runner, source, true)
	return release, err
}

// parallelsPinnedFleetConfig narrows a candidate's fleet to the entry it was
// derived from. The entry has to survive: maxVMs is read from it by name.
func parallelsPinnedFleetConfig(cfg Config) Config {
	for _, host := range cfg.Parallels.Hosts {
		if firstNonBlank(host.Name, host.Host, "local") == cfg.Parallels.SelectedHost {
			pinned := cfg
			pinned.Parallels.Hosts = []ParallelsHostConfig{host}
			return pinned
		}
	}
	return cfg
}

func selectParallelsFleetConfig(ctx context.Context, cfg Config, runner CommandRunner, source string, reserve bool) (Config, func(), error) {
	var lastErr error
	for _, candidate := range ParallelsCandidateConfigs(cfg) {
		release := func() {}
		if reserve {
			var err error
			release, err = lockParallelsFleetCapacity(ctx, candidate)
			if err != nil {
				lastErr = err
				continue
			}
		}
		client := NewParallelsClient(candidate, runner)
		vms, err := client.ListVMs(ctx)
		if err != nil {
			release()
			lastErr = err
			continue
		}
		if source != "" && !parallelsVMListContains(vms, source) {
			release()
			lastErr = Exit(4, "Parallels source VM %q not found on host %s", source, parallelsHostRefForConfig(candidate))
			continue
		}
		if !parallelsHostWithinCapacity(candidate, vms) {
			release()
			lastErr = Exit(5, "Parallels host %s is at maxVMs capacity", parallelsHostRefForConfig(candidate))
			continue
		}
		return candidate, release, nil
	}
	if lastErr != nil {
		return Config{}, nil, lastErr
	}
	return cfg, func() {}, nil
}

// lockParallelsFleetCapacity serializes capacity reservations for one fleet host.
// A host with no maxVMs has no capacity to protect, so it is left unserialized and
// forks against it stay fully parallel.
func lockParallelsFleetCapacity(ctx context.Context, cfg Config) (func(), error) {
	if parallelsHostMaxVMs(cfg) <= 0 {
		return func() {}, nil
	}
	path, err := parallelsCapacityLockPath(parallelsCapacityIdentity(cfg))
	if err != nil {
		return nil, err
	}
	// Never unlink this lock: a waiter must keep using the same inode after unlock.
	lock := flock.New(path, flock.SetPermissions(0o600))
	if _, err := lock.TryLockContext(ctx, parallelsCapacityLockRetryDelay); err != nil {
		return nil, fmt.Errorf("wait for Parallels host %s capacity reservation: %w", parallelsHostRefForConfig(cfg), err)
	}
	return func() { _ = lock.Close() }, nil
}

func parallelsCapacityIdentity(cfg Config) string {
	host := strings.TrimSpace(cfg.Parallels.Host)
	if host == "" {
		return "local"
	}
	return "remote\x00" + host + "\x00" + strings.TrimSpace(cfg.Parallels.HostUser)
}

func parallelsCapacityLockPath(identity string) (string, error) {
	dir, err := CrabboxStateDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "parallels", "capacity-locks")
	if err := makePrivateDurableDirectories(dir); err != nil {
		return "", Exit(2, "create Parallels capacity lock directory: %v", err)
	}
	// Digest the execution identity so operator-supplied values stay out of paths.
	digest := sha256.Sum256([]byte(identity))
	return filepath.Join(dir, hex.EncodeToString(digest[:])+".lock"), nil
}

func ResolveParallelsVM(ctx context.Context, cfg Config, runner CommandRunner, id string) (Config, ParallelsVM, error) {
	var lastErr error
	for _, candidate := range ParallelsCandidateConfigs(cfg) {
		client := NewParallelsClient(candidate, runner)
		vm, err := client.GetVM(ctx, id)
		if err == nil {
			return candidate, vm, nil
		}
		lastErr = err
		vms, err := client.ListVMs(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		for _, vm := range vms {
			if parallelsVMMatchesHandle(vm, id) {
				return candidate, vm, nil
			}
		}
	}
	if lastErr != nil {
		return Config{}, ParallelsVM{}, lastErr
	}
	return Config{}, ParallelsVM{}, Exit(4, "parallels VM not found: %s", id)
}

func parallelsVMMatchesHandle(vm ParallelsVM, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if vm.ID == id || strings.Trim(vm.ID, "{}") == strings.Trim(id, "{}") || vm.Name == id {
		return true
	}
	leaseID, slug := parallelsLeaseFromVMName(vm.Name)
	if leaseID == id || strings.ReplaceAll(leaseID, "_", "-") == strings.ReplaceAll(id, "_", "-") {
		return true
	}
	return slug != "" && NormalizeLeaseSlug(slug) == NormalizeLeaseSlug(id)
}

// ParallelsServerIdentity is the Parallels service's own account of the machine
// it runs on. Neither field is derived from configuration, so it attests which
// machine a connection actually reached rather than which one it was labelled
// with.
type ParallelsServerIdentity struct {
	ServerID   string `json:"serverId"`
	HardwareID string `json:"hardwareId"`
	AccountID  string `json:"accountId"`
	// VMHome is the service's default VM directory. It is the base a fixed
	// lease clones into when no vmRoot is configured.
	VMHome string `json:"-"`
}

// ServerIdentity reads the connected Parallels service identity. A fixed lease
// binds to this, not to a fleet entry's display name: repointing an entry's
// host or account at a different machine changes the reported identity.
func (c *ParallelsClient) ServerIdentity(ctx context.Context) (ParallelsServerIdentity, error) {
	result, err := c.prlsrvctl(ctx, "info", "--json")
	if err != nil {
		return ParallelsServerIdentity{}, commandOutputError("parallels server info", result, err)
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &item); err != nil {
		return ParallelsServerIdentity{}, Exit(4, "parse Parallels server info: %v", err)
	}
	identity := ParallelsServerIdentity{
		ServerID:   firstJSONField(item, "ID", "Id", "id"),
		HardwareID: firstJSONField(item, "Hardware Id", "HardwareId", "hardware_id"),
		VMHome:     firstJSONField(item, "VM home", "VMHome", "vm_home"),
	}
	if identity.ServerID == "" {
		return ParallelsServerIdentity{}, Exit(4, "Parallels host reported no server ID; refusing to bind a fixed lease to an unattested host")
	}
	// Another account's empty inventory cannot prove this account's VM is gone.
	account, err := c.hostCommand(ctx, nil, "id", "-u")
	if err != nil {
		return ParallelsServerIdentity{}, commandOutputError("parallels host account identity", account, err)
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(account.Stdout), 10, 32)
	if err != nil {
		return ParallelsServerIdentity{}, Exit(4, "Parallels host reported no valid account ID; refusing to bind a fixed lease to an unattested account")
	}
	identity.AccountID = strconv.FormatUint(uid, 10)
	return identity, nil
}

func (c *ParallelsClient) ListCrabboxServers(ctx context.Context) ([]Server, error) {
	vms, err := c.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(vms))
	for _, vm := range vms {
		if !strings.HasPrefix(vm.Name, "crabbox-") {
			continue
		}
		servers = append(servers, parallelsVMToServer(c.Cfg, vm, parallelsLabelsFromName(vm.Name)))
	}
	return servers, nil
}

func (c *ParallelsClient) ListVMs(ctx context.Context) ([]ParallelsVM, error) {
	result, err := c.prlctl(ctx, nil, "list", "-a", "-f", "-j")
	if err != nil {
		return nil, commandOutputError("parallels list", result, err)
	}
	return parseParallelsVMs(result.Stdout)
}

// ListVMsDetailed returns the complete inventory with per-VM detail. The plain
// listing reports only uuid, name, status and address; the bundle path a fixed
// lease attests its VM by comes from the info form.
func (c *ParallelsClient) ListVMsDetailed(ctx context.Context) ([]ParallelsVM, error) {
	result, err := c.prlctl(ctx, nil, "list", "-a", "-i", "-f", "-j")
	if err != nil {
		return nil, commandOutputError("parallels list detail", result, err)
	}
	return parseParallelsVMs(result.Stdout)
}

func (c *ParallelsClient) GetVM(ctx context.Context, id string) (ParallelsVM, error) {
	result, err := c.prlctl(ctx, nil, "list", "-i", "-f", "-j", id)
	if err != nil {
		return ParallelsVM{}, commandOutputError("parallels get vm", result, err)
	}
	vms, err := parseParallelsVMs(result.Stdout)
	if err != nil {
		return ParallelsVM{}, err
	}
	if len(vms) == 0 {
		return ParallelsVM{}, Exit(4, "parallels VM not found: %s", id)
	}
	return vms[0], nil
}

// SubmitClone validates the request, calls beforeSubmit immediately before
// `prlctl clone`, and leaves resource discovery to the caller. A mutable-name
// lookup cannot attest which incarnation the clone produced.
func (c *ParallelsClient) SubmitClone(ctx context.Context, source, snapshotID, leaseID, slug string, keep bool, beforeSubmit func() error) error {
	_, err := c.submitClone(ctx, source, snapshotID, leaseID, slug, keep, beforeSubmit)
	return err
}

func (c *ParallelsClient) Clone(ctx context.Context, source, snapshotID, leaseID, slug string, keep bool) (Server, error) {
	labels, err := c.submitClone(ctx, source, snapshotID, leaseID, slug, keep, nil)
	if err != nil {
		return Server{}, err
	}
	vm, err := c.GetVM(ctx, parallelsLeaseVMName(leaseID, slug))
	if err != nil {
		return Server{}, err
	}
	server := parallelsVMToServer(c.Cfg, vm, labels)
	_ = writeParallelsLeaseLabels(leaseID, server.Labels)
	return server, nil
}

func (c *ParallelsClient) submitClone(ctx context.Context, source, snapshotID, leaseID, slug string, keep bool, beforeSubmit func() error) (map[string]string, error) {
	if strings.TrimSpace(source) == "" {
		return nil, Exit(2, "parallels.source or parallels.sourceId is required")
	}
	name := parallelsLeaseVMName(leaseID, slug)
	args := []string{"clone", source, "--name", name}
	if dst := strings.TrimSpace(c.Cfg.Parallels.VMRoot); dst != "" {
		// --dst is the existing parent directory; Parallels names and creates the VM bundle.
		args = append(args, "--dst", dst)
	}
	switch strings.ToLower(strings.TrimSpace(c.Cfg.Parallels.CloneMode)) {
	case "", "linked":
		if strings.TrimSpace(snapshotID) == "" {
			return nil, Exit(2, "Parallels linked clones require --parallels-source-snapshot or --parallels-source-snapshot-id; otherwise prlctl creates a source-side linked-clone snapshot")
		}
		if snapshotID != "" {
			snapshot, ok, err := c.snapshotByID(ctx, source, snapshotID)
			if err != nil {
				return nil, err
			}
			if ok {
				if err := validateParallelsSnapshotCloneMode(snapshot, c.Cfg.Parallels.CloneMode); err != nil {
					return nil, err
				}
			}
		}
		args = append(args, "--linked")
	case "full":
		if snapshotID != "" {
			return nil, Exit(2, "Parallels snapshot forks require cloneMode=linked; prlctl selects snapshots only for linked clones")
		}
	case "unlink":
		if snapshotID != "" {
			return nil, Exit(2, "Parallels snapshot forks require cloneMode=linked; prlctl selects snapshots only for linked clones")
		}
		args = append(args, "--unlink")
	default:
		return nil, Exit(2, "parallels.cloneMode must be linked, full, or unlink")
	}
	if snapshotID != "" {
		args = append(args, "-i", snapshotID)
	}
	if beforeSubmit != nil {
		if err := beforeSubmit(); err != nil {
			return nil, err
		}
	}
	result, err := c.prlctl(ctx, nil, args...)
	if err != nil {
		return nil, commandOutputError("parallels clone", result, err)
	}
	labels := DirectLeaseLabels(c.Cfg, leaseID, slug, parallelsProvider, "", keep, time.Now().UTC())
	labels["source"] = source
	labels["host"] = parallelsHostRefForConfig(c.Cfg)
	if snapshotID != "" {
		labels["source_snapshot"] = snapshotID
	}
	return labels, nil
}

// EnsureHostDir creates a directory on the Parallels host. A fixed lease clones
// into a per-attempt directory, and prlctl requires --dst to exist already.
func (c *ParallelsClient) EnsureHostDir(ctx context.Context, dir string) error {
	if err := validParallelsHostDir(dir); err != nil {
		return err
	}
	result, err := c.hostCommand(ctx, nil, "mkdir", "-p", dir)
	if err != nil {
		return commandOutputError("parallels create host directory", result, err)
	}
	return nil
}

// RemoveHostDirIfEmpty clears a spent per-attempt directory. It never recurses:
// only an empty directory is removed, so a surviving VM is never disturbed.
func (c *ParallelsClient) RemoveHostDirIfEmpty(ctx context.Context, dir string) {
	if validParallelsHostDir(dir) != nil {
		return
	}
	_, _ = c.hostCommand(ctx, nil, "rmdir", dir)
}

func validParallelsHostDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if !strings.HasPrefix(dir, "/") || strings.ContainsAny(dir, "\r\n\x00") {
		return Exit(2, "Parallels host directory must be an absolute path")
	}
	return nil
}

func (c *ParallelsClient) Start(ctx context.Context, id string) error {
	result, err := c.prlctl(ctx, nil, "start", id)
	if err != nil && !strings.Contains(strings.ToLower(result.Stderr+result.Stdout), "already started") {
		return commandOutputError("parallels start", result, err)
	}
	return nil
}

func (c *ParallelsClient) Stop(ctx context.Context, id string) error {
	result, err := c.prlctl(ctx, nil, "stop", id, "--kill")
	if err != nil {
		return commandOutputError("parallels stop", result, err)
	}
	return nil
}

func (c *ParallelsClient) Delete(ctx context.Context, id string) error {
	vm, err := c.GetVM(ctx, id)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(vm.Name, "crabbox-") {
		return Exit(2, "refusing to delete non-Crabbox Parallels VM %q", vm.Name)
	}
	if strings.EqualFold(vm.State, "running") {
		_ = c.Stop(ctx, id)
	}
	result, err := c.prlctl(ctx, nil, "delete", vm.ID)
	if err != nil {
		return commandOutputError("parallels delete", result, err)
	}
	if leaseID, _ := parallelsLeaseFromVMName(vm.Name); leaseID != "" {
		removeParallelsLeaseLabels(leaseID)
	}
	return nil
}

func (c *ParallelsClient) ValidateMacOSBootstrapKey(ctx context.Context) error {
	key := strings.TrimSpace(c.Cfg.Parallels.BootstrapKey)
	if key == "" {
		return nil
	}
	if c.Cfg.TargetOS != targetMacOS {
		return Exit(2, "parallels.bootstrapKey is supported only for macOS guests")
	}
	if !filepath.IsAbs(key) || strings.ContainsAny(key, "\r\n\x00") {
		return Exit(2, "parallels.bootstrapKey must be an absolute path on the Parallels host")
	}
	result, err := c.hostCommand(ctx, nil, "/bin/test", "-f", key, "-a", "-r", key)
	if err != nil {
		return commandOutputError("validate Parallels macOS bootstrap key on host", result, err)
	}
	return nil
}

func (c *ParallelsClient) SetLeaseLabels(leaseID string, labels map[string]string) {
	_ = writeParallelsLeaseLabels(leaseID, labels)
}

func (c *ParallelsClient) InstallSSHKey(ctx context.Context, vmID string, cfg Config, publicKey string) error {
	user := strings.TrimSpace(cfg.SSHUser)
	if user == "" {
		return Exit(2, "parallels guest SSH user is required")
	}
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return Exit(2, "parallels guest SSH public key is empty")
	}
	if cfg.TargetOS == targetWindows {
		return c.runWindowsPowerShellFile(ctx, vmID, "install-ssh", WindowsBootstrapPowerShell(cfg, publicKey))
	}
	script := parallelsPOSIXInstallSSHKeyScript(user, publicKey)
	result, err := c.prlctlWithStdin(ctx, strings.NewReader(script), nil, "exec", vmID, "/bin/sh", "-s")
	if err != nil {
		return commandOutputError("parallels install ssh key", result, err)
	}
	return nil
}

// BootstrapMacOSOverSSH installs the per-lease key and performs the normal
// guest preparation through a pre-provisioned host-side identity. The key path
// belongs to the Parallels host (local or remote), never to the cloned guest.
func (c *ParallelsClient) BootstrapMacOSOverSSH(ctx context.Context, ip string, cfg Config, publicKey string) error {
	if cfg.TargetOS != targetMacOS {
		return Exit(2, "Parallels SSH bootstrap fallback is supported only for macOS guests")
	}
	bootstrapKey := strings.TrimSpace(cfg.Parallels.BootstrapKey)
	if !filepath.IsAbs(bootstrapKey) || strings.ContainsAny(bootstrapKey, "\r\n\x00") {
		return Exit(2, "parallels.bootstrapKey must be an absolute path on the Parallels host")
	}
	user := strings.TrimSpace(cfg.SSHUser)
	if user == "" {
		return Exit(2, "parallels guest SSH user is required")
	}
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil || parsedIP.To4() == nil {
		return Exit(5, "Parallels DHCP fallback returned invalid IPv4 address %q", ip)
	}
	port := strings.TrimSpace(cfg.SSHPort)
	if port == "" {
		port = "22"
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return Exit(2, "invalid Parallels guest SSH port %q", port)
	}
	script := parallelsPOSIXInstallSSHKeyScript(user, publicKey) + "\n" + parallelsPOSIXEnsureReadyScript(user, cfg.WorkRoot, cfg.Desktop, cfg.Parallels.Password != "", sshPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts))
	args := []string{
		"/usr/bin/ssh",
		"-i", bootstrapKey,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=1",
		"-o", "StrictHostKeyChecking=accept-new",
		"-p", port,
		user + "@" + parsedIP.String(),
		"sudo", "-n", "/bin/sh", "-s",
	}
	result, err := c.hostCommand(ctx, strings.NewReader(script), args...)
	if err != nil {
		return commandOutputError("parallels macOS SSH bootstrap", result, err)
	}
	return nil
}

func (c *ParallelsClient) runWindowsPowerShellFile(ctx context.Context, vmID, name, script string) error {
	base := `C:\ProgramData\crabbox`
	ps1 := base + `\` + name + `.ps1`
	b64 := ps1 + `.b64`
	if err := c.runWindowsPowerShell(ctx, vmID, fmt.Sprintf(`New-Item -ItemType Directory -Force -Path %s | Out-Null
foreach ($path in @(%s, %s)) {
  if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
}`, psQuote(base), psQuote(ps1), psQuote(b64))); err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	const chunkSize = 400
	for start := 0; start < len(encoded); start += chunkSize {
		end := start + chunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		chunk := encoded[start:end]
		if err := c.runWindowsPowerShell(ctx, vmID, fmt.Sprintf(`Add-Content -Encoding ASCII -LiteralPath %s -Value %s`, psQuote(b64), psQuote(chunk))); err != nil {
			return err
		}
	}
	run := fmt.Sprintf(`$raw = (Get-Content -Raw -LiteralPath %s) -replace '\s',''
$bytes = [Convert]::FromBase64String($raw)
$text = [Text.Encoding]::UTF8.GetString($bytes)
[IO.File]::WriteAllText(%s, $text, [Text.UTF8Encoding]::new($false))
& powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File %s`, psQuote(b64), psQuote(ps1), psQuote(ps1))
	return c.runWindowsPowerShell(ctx, vmID, run)
}

func (c *ParallelsClient) runWindowsPowerShell(ctx context.Context, vmID, script string) error {
	args := strings.Fields(PowershellCommand(script))
	result, err := c.prlctl(ctx, nil, append([]string{"exec", vmID}, args...)...)
	if err != nil {
		return commandOutputError("parallels windows powershell", result, err)
	}
	return nil
}

func (c *ParallelsClient) EnsureGuestReady(ctx context.Context, vmID string, cfg Config) error {
	if cfg.TargetOS == targetWindows {
		return nil
	}
	user := strings.TrimSpace(cfg.SSHUser)
	if user == "" {
		return Exit(2, "parallels guest SSH user is required")
	}
	workRoot := strings.TrimSpace(cfg.WorkRoot)
	if workRoot == "" {
		workRoot = baseConfig().WorkRoot
	}
	desktop := cfg.Desktop
	script := parallelsPOSIXEnsureReadyScript(user, workRoot, desktop, cfg.TargetOS == targetMacOS && cfg.Parallels.Password != "", sshPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts))
	result, err := c.prlctlWithStdin(ctx, strings.NewReader(script), nil, "exec", vmID, "/bin/sh", "-s")
	if err != nil {
		return commandOutputError("parallels guest prep", result, err)
	}
	return nil
}

func (c *ParallelsClient) WaitForIP(ctx context.Context, id string, timeout time.Duration, purpose ParallelsIPWaitPurpose) (ParallelsVM, error) {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := waitCtx.Deadline()
	nextProbe := time.Now().Add(parallelsIPBootGrace)
	unavailableProbes := 0
	dhcpAddressObserved := false
	var last ParallelsVM
	var vmObserved bool
	var lastDHCPError error
	useDHCPFallback := c.Cfg.TargetOS == targetMacOS && strings.TrimSpace(c.Cfg.Parallels.BootstrapKey) != ""
	waitError := func(reason string) error {
		if parentDeadline, ok := ctx.Deadline(); ok && !time.Now().Before(parentDeadline) {
			<-ctx.Done()
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		hint := parallelsIPTimeoutHint(c.Cfg, last, vmObserved, useDHCPFallback, lastDHCPError, purpose)
		if lastDHCPError != nil {
			return Exit(5, "%s Parallels VM %s IP; last_state=%s; DHCP fallback: %v; %s", reason, id, blank(last.State, "-"), lastDHCPError, hint)
		}
		return Exit(5, "%s Parallels VM %s IP; last_state=%s; %s", reason, id, blank(last.State, "-"), hint)
	}
	waitExpired := func() bool {
		// The poll can wake before the deadline's cancellation callback runs.
		// Join that notification before admitting work, preserving the parent cause.
		if !time.Now().Before(deadline) {
			<-waitCtx.Done()
		}
		return waitCtx.Err() != nil
	}
	for {
		if ctx.Err() != nil {
			return ParallelsVM{}, context.Cause(ctx)
		}
		if waitExpired() {
			return ParallelsVM{}, waitError("timed out waiting for")
		}
		vm, err := c.GetVM(waitCtx, id)
		vmObserved = err == nil
		if ctx.Err() != nil {
			return ParallelsVM{}, context.Cause(ctx)
		}
		if err == nil {
			last = vm
			if vm.IP != "" {
				if waitExpired() {
					return ParallelsVM{}, waitError("timed out waiting for")
				}
				vm.IPSource = "tools"
				return vm, nil
			}
			if useDHCPFallback {
				if waitExpired() {
					return ParallelsVM{}, waitError("timed out waiting for")
				}
				data, readErr := c.readDHCPLeases(waitCtx)
				if ctx.Err() != nil {
					return ParallelsVM{}, context.Cause(ctx)
				}
				if readErr != nil {
					lastDHCPError = readErr
				} else if ip, resolveErr := resolveParallelsDHCPLeaseIP(data, vm.MACs, time.Now()); resolveErr != nil {
					lastDHCPError = resolveErr
					if errors.Is(resolveErr, errParallelsDHCPLeaseAmbiguous) {
						return ParallelsVM{}, resolveErr
					}
				} else {
					// A valid address makes SSH, rather than Tools, the remaining
					// readiness dependency. Keep its full startup budget.
					dhcpAddressObserved = true
					if waitExpired() {
						return ParallelsVM{}, waitError("timed out waiting for")
					}
					probeErr := c.probeHostTCP(waitCtx, ip, c.Cfg.SSHPort)
					if ctx.Err() != nil {
						return ParallelsVM{}, context.Cause(ctx)
					}
					if probeErr != nil {
						lastDHCPError = probeErr
					} else {
						if waitExpired() {
							return ParallelsVM{}, waitError("timed out waiting for")
						}
						vm.IP = ip
						vm.IPSource = "dhcp-mac"
						return vm, nil
					}
				}
			}
		}
		if waitExpired() {
			return ParallelsVM{}, waitError("timed out waiting for")
		}
		if err != nil || !strings.EqualFold(strings.TrimSpace(vm.State), "running") || dhcpAddressObserved {
			unavailableProbes = 0
		} else if !time.Now().Before(nextProbe) {
			probeCtx, cancelProbe := context.WithTimeout(waitCtx, parallelsIPProbeTimeout)
			probeErr := c.probeGuestExec(probeCtx, id, c.Cfg)
			probeContextErr := probeCtx.Err()
			cancelProbe()
			nextProbe = time.Now().Add(parallelsIPProbeInterval)
			if ctx.Err() != nil {
				return ParallelsVM{}, context.Cause(ctx)
			}
			if waitExpired() {
				return ParallelsVM{}, waitError("timed out waiting for")
			}
			// Early boot metadata, timeouts and unrelated execution errors do
			// not prove that Tools remain unavailable.
			if probeContextErr == nil && ParallelsGuestToolsUnavailable(probeErr) {
				unavailableProbes++
			} else {
				unavailableProbes = 0
			}
			if unavailableProbes >= parallelsIPUnavailableLimit {
				return ParallelsVM{}, fmt.Errorf("%w: %w", errParallelsGuestToolsUnavailable,
					waitError(fmt.Sprintf("failed after %d consecutive Tools-unavailable probes waiting for", unavailableProbes)))
			}
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ParallelsVM{}, context.Cause(ctx)
			}
			return ParallelsVM{}, waitError("timed out waiting for")
		case <-time.After(5 * time.Second):
		}
	}
}

// parallelsIPTimeoutHint explains an IP discovery timeout in terms of the
// clone mode, the discovery routes that ran, and the next check to make.
func parallelsIPTimeoutHint(cfg Config, last ParallelsVM, vmObserved, dhcpFallback bool, dhcpErr error, purpose ParallelsIPWaitPurpose) string {
	mode := "unknown"
	if purpose == ParallelsIPWaitAcquisition {
		mode = strings.ToLower(strings.TrimSpace(cfg.Parallels.CloneMode))
		if mode == "" {
			mode = "linked"
		}
	}
	macs := "-"
	if len(last.MACs) > 0 {
		macs = strings.Join(last.MACs, ",")
	}
	toolsIP := "unknown"
	if vmObserved {
		toolsIP = "none"
	}
	parts := []string{"clone_mode=" + mode + " macs=" + macs + " tools_ip=" + toolsIP}
	parts = append(parts, "--parallels-startup-timeout bounds the overall IP wait; increasing it does not disable Tools fail-fast")
	if !dhcpFallback && cfg.TargetOS == targetMacOS {
		parts = append(parts, "for macOS guests without working Parallels Tools, set parallels.bootstrapKey to enable DHCP/SSH discovery")
	}
	running := strings.EqualFold(strings.TrimSpace(last.State), "running")
	if running && errors.Is(dhcpErr, errParallelsDHCPLeaseMissing) {
		parts = append(parts, "no matching DHCP lease was found in the Parallels host lease file for the VM's MACs; check guest boot and network configuration")
		if purpose == ParallelsIPWaitAcquisition {
			parts = append(parts, "acquisition cleans up failed clones: retry and capture the new VM on the Parallels host while IP discovery is still waiting (`prlctl capture <new-vm-id> --file <png>`)")
		} else {
			parts = append(parts, "inspect the existing VM on the Parallels host with `prlctl capture <existing-vm-id> --file <png>`")
		}
	}
	if running && purpose == ParallelsIPWaitAcquisition && mode == "linked" {
		parts = append(parts, "the template snapshot may not boot as a linked clone; retry with parallels.cloneMode=full (--parallels-clone-mode full) and clear parallels.sourceSnapshot and parallels.sourceSnapshotId with --parallels-source-snapshot= --parallels-source-snapshot-id= (full clones use the source VM's current state and cannot select a source snapshot)")
	}
	return "hint: " + strings.Join(parts, "; ")
}

func (c *ParallelsClient) WaitForGuestExec(ctx context.Context, id string, cfg Config, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		err := c.probeGuestExec(ctx, id, cfg)
		if err == nil {
			return nil
		}
		lastErr = err
		if c.Cfg.TargetOS == targetMacOS && strings.TrimSpace(c.Cfg.Parallels.BootstrapKey) != "" && ParallelsGuestToolsUnavailable(lastErr) {
			return lastErr
		}
		if time.Now().After(deadline) {
			return Exit(5, "timed out waiting for Parallels guest exec in %s: %v", id, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *ParallelsClient) probeGuestExec(ctx context.Context, id string, cfg Config) error {
	var args []string
	if cfg.TargetOS == targetWindows {
		args = strings.Fields(PowershellCommand(`"ok" | Out-Null`))
	} else {
		args = []string{"/bin/sh", "-lc", "true"}
	}
	result, err := c.prlctl(ctx, nil, append([]string{"exec", id}, args...)...)
	return commandOutputError("parallels guest exec", result, err)
}

func ParallelsGuestToolsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return errors.Is(err, errParallelsGuestToolsUnavailable) ||
		strings.Contains(message, "prl_err_vm_exec_guest_tool_not_available") ||
		strings.Contains(message, "guest tools are not available") ||
		strings.Contains(message, "guest tools not available") ||
		(strings.Contains(message, "unable to open new session in this virtual machine.") &&
			strings.Contains(message, "runs the latest version of parallels tools, and is not isolated from the host os"))
}

func (c *ParallelsClient) WindowsGuestText(ctx context.Context, id, path string) (string, error) {
	result, err := c.prlctl(ctx, nil, "exec", id, "cmd.exe", "/C", "type", path)
	if err != nil {
		return "", commandOutputError("parallels read windows guest file", result, err)
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (c *ParallelsClient) POSIXGuestText(ctx context.Context, id, path string) (string, error) {
	result, err := c.prlctl(ctx, nil, "exec", id, "/bin/cat", path)
	if err != nil {
		return "", commandOutputError("parallels read posix guest file", result, err)
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (c *ParallelsClient) Snapshots(ctx context.Context, id string) ([]ParallelsSnapshot, error) {
	result, err := c.prlctl(ctx, nil, "snapshot-list", id, "-j")
	if err != nil {
		return nil, commandOutputError("parallels snapshot-list", result, err)
	}
	return parseParallelsSnapshots(result.Stdout)
}

func (c *ParallelsClient) SnapshotID(ctx context.Context, vmID, nameOrID string) (string, error) {
	snapshot, err := c.Snapshot(ctx, vmID, nameOrID)
	if err != nil {
		return "", err
	}
	return snapshot.ID, nil
}

func (c *ParallelsClient) Snapshot(ctx context.Context, vmID, nameOrID string) (ParallelsSnapshot, error) {
	value := strings.TrimSpace(nameOrID)
	if value == "" {
		return ParallelsSnapshot{}, nil
	}
	snapshots, err := c.Snapshots(ctx, vmID)
	if err != nil {
		return ParallelsSnapshot{}, err
	}
	trimmed := strings.Trim(value, "{}")
	for _, snapshot := range snapshots {
		if strings.Trim(snapshot.ID, "{}") == trimmed || snapshot.Name == value {
			return snapshot, nil
		}
	}
	return ParallelsSnapshot{}, Exit(4, "Parallels snapshot %q not found for VM %s", value, vmID)
}

func (c *ParallelsClient) snapshotByID(ctx context.Context, vmID, id string) (ParallelsSnapshot, bool, error) {
	snapshots, err := c.Snapshots(ctx, vmID)
	if err != nil {
		return ParallelsSnapshot{}, false, err
	}
	trimmed := strings.Trim(id, "{}")
	for _, snapshot := range snapshots {
		if strings.Trim(snapshot.ID, "{}") == trimmed {
			return snapshot, true, nil
		}
	}
	return ParallelsSnapshot{}, false, nil
}

func (c *ParallelsClient) CreateSnapshot(ctx context.Context, vmID, name, description string) (ParallelsSnapshot, error) {
	args := []string{"snapshot", vmID, "--name", name}
	if description != "" {
		args = append(args, "--description", description)
	}
	result, err := c.prlctl(ctx, nil, args...)
	if err != nil {
		return ParallelsSnapshot{}, commandOutputError("parallels snapshot", result, err)
	}
	snapshots, err := c.Snapshots(ctx, vmID)
	if err != nil {
		return ParallelsSnapshot{}, err
	}
	for _, snapshot := range snapshots {
		if snapshot.Name == name {
			return snapshot, nil
		}
	}
	return ParallelsSnapshot{}, Exit(5, "Parallels snapshot %q was created but not found in snapshot-list", name)
}

func (c *ParallelsClient) SwitchSnapshot(ctx context.Context, vmID, snapshotID string, skipResume bool) error {
	args := []string{"snapshot-switch", vmID, "-i", snapshotID}
	if skipResume {
		args = append(args, "--skip-resume")
	}
	result, err := c.prlctl(ctx, nil, args...)
	if err != nil {
		return commandOutputError("parallels snapshot-switch", result, err)
	}
	return nil
}

func (c *ParallelsClient) DeleteSnapshot(ctx context.Context, vmID, snapshotID string, children bool) error {
	args := []string{"snapshot-delete", vmID, "-i", snapshotID}
	if children {
		args = append(args, "-c")
	}
	result, err := c.prlctl(ctx, nil, args...)
	if err != nil {
		return commandOutputError("parallels snapshot-delete", result, err)
	}
	return nil
}

func (c *ParallelsClient) readDHCPLeases(ctx context.Context) (string, error) {
	result, err := c.hostCommand(ctx, nil, "/bin/cat", parallelsDHCPLeasesPath)
	if err != nil {
		return "", commandOutputError("read Parallels DHCP leases", result, err)
	}
	return result.Stdout, nil
}

func (c *ParallelsClient) probeHostTCP(ctx context.Context, ip, port string) error {
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil || parsedIP.To4() == nil {
		return Exit(5, "Parallels DHCP fallback returned invalid IPv4 address %q", ip)
	}
	port = strings.TrimSpace(port)
	if port == "" {
		port = "22"
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return Exit(2, "invalid Parallels guest SSH port %q", port)
	}
	result, err := c.hostCommand(ctx, nil, "/usr/bin/nc", "-z", "-w", "2", parsedIP.String(), port)
	if err != nil {
		return commandOutputError(fmt.Sprintf("verify Parallels DHCP guest SSH reachability at %s:%s", parsedIP.String(), port), result, err)
	}
	return nil
}

// Both preparation scripts are handed to the guest shell on stdin, never as an
// argv element: `prlctl exec` flattens argv and re-parses it guest-side, which
// silently drops the leading `set -eu`. Every child either script runs must
// therefore take its stdin from /dev/null, or it eats the rest of the script
// while the shell still exits 0.
// https://github.com/openclaw/crabbox/issues/2396
func parallelsPOSIXInstallSSHKeyScript(user, publicKey string) string {
	return fmt.Sprintf(`set -eu
user=%s
key=%s
home=$(getent passwd "$user" </dev/null 2>/dev/null | cut -d: -f6 || true)
if [ -z "$home" ]; then
  home=$(dscl . -read "/Users/$user" NFSHomeDirectory </dev/null 2>/dev/null | awk '{print $2}' || true)
fi
if [ -z "$home" ]; then
  echo "user home not found: $user" >&2
  exit 1
fi
group=$(id -gn "$user" 2>/dev/null || printf '%%s' "$user")
install -d -m 700 -o "$user" -g "$group" "$home/.ssh"
touch "$home/.ssh/authorized_keys"
grep -qxF "$key" "$home/.ssh/authorized_keys" || printf '%%s\n' "$key" >> "$home/.ssh/authorized_keys"
chown "$user:$group" "$home/.ssh/authorized_keys"
chmod 600 "$home/.ssh/authorized_keys"
mkdir -p /var/lib/crabbox 2>/dev/null || true
printf '%%s\n' "$user" >/var/lib/crabbox/ssh.username 2>/dev/null || true
`, shellWords([]string{user})[0], shellWords([]string{publicKey})[0])
}

func parallelsMacOSDesktopReadyTest(accountCredentials bool) string {
	if accountCredentials {
		return "[ -f /var/db/crabbox/vnc.console ] && nc -z 127.0.0.1 5900 </dev/null"
	}
	return "[ -s /var/db/crabbox/vnc.password ] && [ -f /var/db/crabbox/vnc.console ] && nc -z 127.0.0.1 5900 </dev/null"
}

func parallelsMacOSDesktopSetupScript(accountCredentials bool) string {
	credentialSetup := ""
	clientOptions := ""
	if !accountCredentials {
		credentialSetup = `    vnc_password=""
    if [ -s /var/db/crabbox/vnc.password ]; then
      vnc_password="$(tr -d '\r\n' </var/db/crabbox/vnc.password)"
    fi
    case "$vnc_password" in
      ????????) ;;
      *) vnc_password="$(/usr/bin/openssl rand -hex 4 </dev/null)" ;;
    esac
    case "$vnc_password" in
      *[!A-Za-z0-9]*) echo "invalid generated VNC password" >&2; exit 1 ;;
    esac
    printf '%s\n' "$vnc_password" >/var/db/crabbox/vnc.password
    chmod 0600 /var/db/crabbox/vnc.password
    mkdir -p /etc/sudoers.d
    printf '%s ALL=(root) NOPASSWD: /bin/cat /var/db/crabbox/vnc.password\n' "$user" >/etc/sudoers.d/crabbox-vnc-password
    chmod 0440 /etc/sudoers.d/crabbox-vnc-password
    /usr/sbin/visudo -cf /etc/sudoers.d/crabbox-vnc-password </dev/null >/dev/null
`
		clientOptions = `    "$kickstart" -configure -clientopts -setdirlogins -dirlogins no -setvnclegacy -vnclegacy yes -setvncpw -vncpw "$vnc_password" </dev/null >/dev/null 2>&1
`
	}
	return `    mkdir -p /var/db/crabbox
` + credentialSetup + `    kickstart=/System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart
    [ -x "$kickstart" ]
    /usr/bin/defaults write /Library/Preferences/com.apple.RemoteManagement VNCAlwaysStartOnConsole -bool true </dev/null
    "$kickstart" -activate -configure -allowAccessFor -specifiedUsers </dev/null >/dev/null 2>&1
    "$kickstart" -configure -access -on -users "$user" -privs -all </dev/null >/dev/null 2>&1
` + clientOptions + `    "$kickstart" -restart -agent </dev/null >/dev/null 2>&1
    /bin/launchctl enable system/com.apple.screensharing </dev/null >/dev/null 2>&1 || true
    /bin/launchctl kickstart -k system/com.apple.screensharing </dev/null >/dev/null 2>&1 || true
    vnc_ready=false
    for _ in $(jot 60 1); do
      if nc -z 127.0.0.1 5900 </dev/null; then
        vnc_ready=true
        break
      fi
      sleep 1
    done
    if [ "$vnc_ready" != true ]; then
      echo "macOS Screen Sharing did not start (no VNC listener on 127.0.0.1:5900)" >&2
      exit 1
    fi
    touch /var/db/crabbox/vnc.console
`
}

// The macOS Node handling is its own unit so a test can exercise it directly.
// Both preparation paths drive this script through /bin/sh while the pinned
// installer is bash with pipefail, and neither its shell options nor its
// exit 0 may escape into preparation.
func parallelsMacOSNodeBaselineStanza() string {
	return fmt.Sprintf(`# macOS readiness requires Node, so settle it before the gate below. Running
# ahead of the gate keeps a guest whose crabbox-ready predates the Node checks
# from exiting early and skipping this forever.
if command -v sw_vers >/dev/null 2>&1; then
  if ! PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin sh -c 'node --version >/dev/null 2>&1 && npm --version >/dev/null 2>&1'; then
    # A runtime the guest user already manages (Homebrew, nvm, asdf) satisfies
    # readiness, so preserve it rather than downloading over it: an existing
    # template must not start needing nodejs.org to stay ready.
    #
    # Resolve it as that user through bash -lc, matching the probe exactly
    # rather than the user's default login shell, which reads different rc
    # files. Never source their login files as root, and keep stdin off these
    # children -- this whole script arrives on the guest shell's stdin, so
    # anything reading stdin silently eats the rest of it.
    #
    # Ask node for its own execPath rather than taking what command -v returns.
    # An asdf-style shim re-execs through its manager, which is absent from the
    # PATH crabbox-ready uses, so linking the shim would satisfy the probe's
    # login shell and then fail the helper; execPath is the binary that shim
    # ultimately runs, with npm and npx beside it.
    crabbox_node_preserved=false
    crabbox_node_bin=$(su - "$user" -c 'bash -lc "node -p process.execPath"' </dev/null 2>/dev/null || true)
    if [ ! -x "$crabbox_node_bin" ]; then
      crabbox_node_bin=$(su - "$user" -c 'bash -lc "command -v node"' </dev/null 2>/dev/null || true)
    fi
    crabbox_npm_bin=
    if [ -x "$crabbox_node_bin" ]; then
      crabbox_npm_bin=${crabbox_node_bin%%/*}/npm
    fi
    if [ ! -x "$crabbox_npm_bin" ]; then
      crabbox_npm_bin=$(su - "$user" -c 'bash -lc "command -v npm"' </dev/null 2>/dev/null || true)
    fi
    # Guard each destination on its own. The commands can sit in different
    # prefixes -- node already at /usr/local/bin with npm only in the user's
    # login PATH is a healthy template -- and a combined guard would reject
    # preservation whenever either one already occupies its destination,
    # downloading over a runtime that already satisfies readiness. If neither
    # needs linking, the install at /usr/local/bin is the one that just failed
    # the probe above, so fall through and let the installer replace it.
    if [ -x "$crabbox_node_bin" ] && [ -x "$crabbox_npm_bin" ] &&
      { [ "$crabbox_node_bin" != /usr/local/bin/node ] || [ "$crabbox_npm_bin" != /usr/local/bin/npm ]; }; then
      install -d -m 0755 /usr/local/bin
      if [ "$crabbox_node_bin" != /usr/local/bin/node ]; then
        ln -sfn "$crabbox_node_bin" /usr/local/bin/node
      fi
      if [ "$crabbox_npm_bin" != /usr/local/bin/npm ]; then
        ln -sfn "$crabbox_npm_bin" /usr/local/bin/npm
      fi
      crabbox_npx_bin=${crabbox_node_bin%%/*}/npx
      if [ ! -x "$crabbox_npx_bin" ]; then
        crabbox_npx_bin=$(su - "$user" -c 'bash -lc "command -v npx"' </dev/null 2>/dev/null || true)
      fi
      if [ -x "$crabbox_npx_bin" ] && [ "$crabbox_npx_bin" != /usr/local/bin/npx ]; then
        ln -sfn "$crabbox_npx_bin" /usr/local/bin/npx
      fi
      # Only claim preservation if the result actually works in the environment
      # crabbox-ready runs in. Anything that still needs the guest user's login
      # context falls through to the pinned installer instead of leaving a
      # helper that fails while the probe passes.
      if PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin sh -c 'node --version >/dev/null 2>&1 && npm --version >/dev/null 2>&1'; then
        crabbox_node_preserved=true
      fi
    fi
    if [ "$crabbox_node_preserved" != true ]; then
      # No usable runtime anywhere: fall back to the pinned shared installer.
      crabbox_node_installer="$(mktemp /tmp/crabbox-node-install.XXXXXX)"
      cat >"$crabbox_node_installer" <<'CRABBOXNODEINSTALL'
%s
CRABBOXNODEINSTALL
      /bin/bash "$crabbox_node_installer" </dev/null || { rm -f "$crabbox_node_installer"; exit 1; }
      rm -f "$crabbox_node_installer"
    fi
  fi
fi`, sharedMacOSNodeInstall())
}

// parallelsShellPortList renders SSH port candidates as quoted shell words for
// a `for ... in` list. Non-numeric entries are dropped rather than quoted into
// the guest script, and an empty result falls back to the default SSH port.
func parallelsShellPortList(ports []string) string {
	valid := make([]string, 0, len(ports))
	for _, port := range ports {
		port = strings.TrimSpace(port)
		if port == "" {
			continue
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			continue
		}
		valid = append(valid, port)
	}
	if len(valid) == 0 {
		valid = []string{"22"}
	}
	return strings.Join(shellWords(uniqueSSHPorts(valid)), " ")
}

func parallelsPOSIXEnsureReadyScript(user, workRoot string, desktop, macOSAccountCredentials bool, sshPorts []string) string {
	return fmt.Sprintf(`set -eu
user=%s
work_root=%s
desktop=%t
# Only macOS guests gate on this. The Linux branch manages ssh through systemd
# and is left exactly as it was.
crabbox_ssh_listening() {
  if ! command -v sw_vers >/dev/null 2>&1; then
    return 0
  fi
  for port in %s; do
    if nc -z 127.0.0.1 "$port" </dev/null >/dev/null 2>&1; then
      return 0
    fi
  done
  return 1
}
%s
if [ -x /usr/local/bin/crabbox-ready ] && /usr/local/bin/crabbox-ready </dev/null >/tmp/crabbox-ready.log 2>&1 && crabbox_ssh_listening; then
  if [ "$desktop" != true ]; then
    exit 0
  fi
  if command -v sw_vers >/dev/null 2>&1; then
    if %s; then
      exit 0
    fi
  elif command -v websockify >/dev/null 2>&1 && command -v x11vnc >/dev/null 2>&1 && { [ -f /usr/share/novnc/vnc.html ] || [ -f /usr/share/novnc/core/vnc.html ] || [ -f /usr/share/novnc/html/vnc.html ]; } && systemctl is-active --quiet crabbox-x11vnc.service </dev/null; then
    exit 0
  fi
fi
group=$(id -gn "$user" 2>/dev/null || printf '%%s' "$user")
mkdir -p "$work_root" /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/lib/crabbox
printf '%%s\n' "$user" >/var/lib/crabbox/ssh.username 2>/dev/null || true
chown -R "$user:$group" "$work_root" /var/cache/crabbox 2>/dev/null || true
chmod 755 "$work_root" 2>/dev/null || true
case "$(dirname "$work_root")" in
  /work|/workspaces|/var/lib/crabbox|/opt/crabbox) chmod 755 "$(dirname "$work_root")" 2>/dev/null || true ;;
esac
if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  mkdir -p /etc/apt/apt.conf.d
  cat >/etc/apt/apt.conf.d/80-crabbox-retries <<'APT'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APT
  apt-get update </dev/null
  apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq </dev/null
  if [ "$desktop" = true ]; then
    apt-get install -y --no-install-recommends xvfb xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings x11vnc xauth dbus-x11 x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel fonts-dejavu-core fonts-liberation iproute2 openssl util-linux novnc websockify </dev/null
    if [ ! -s /var/lib/crabbox/vnc.password ]; then
      umask 077
      openssl rand -hex 16 </dev/null >/var/lib/crabbox/vnc.password
    fi
    { head -c 8 /var/lib/crabbox/vnc.password; printf '\n'; head -c 8 /var/lib/crabbox/vnc.password; printf '\n\n'; } | x11vnc -storepasswd /var/lib/crabbox/vnc.pass >/dev/null 2>&1
    chown "$user:$group" /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    chmod 0600 /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
    printf 'CRABBOX_DESKTOP_ENV=xfce\nDISPLAY=:99\n' >/var/lib/crabbox/desktop.env
    cat >/etc/systemd/system/crabbox-xvfb.service <<'UNIT'
[Unit]
Description=Crabbox virtual X display
After=network.target
[Service]
ExecStart=/usr/bin/Xvfb :99 -screen 0 1600x1000x24 -nolisten tcp
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
    cat >/etc/systemd/system/crabbox-desktop.service <<UNIT
[Unit]
Description=Crabbox XFCE desktop
After=crabbox-xvfb.service
Requires=crabbox-xvfb.service
[Service]
User=$user
Environment=DISPLAY=:99
ExecStart=/usr/bin/startxfce4
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT
    cat >/etc/systemd/system/crabbox-x11vnc.service <<UNIT
[Unit]
Description=Crabbox VNC server
After=crabbox-desktop.service
Requires=crabbox-xvfb.service
[Service]
User=$user
ExecStart=/usr/bin/x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -rfbauth /var/lib/crabbox/vnc.pass -wait 16 -defer 8 -nowait_bog
Restart=always
RestartSec=1
[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload </dev/null
    systemctl enable --now crabbox-xvfb.service crabbox-desktop.service crabbox-x11vnc.service </dev/null
  fi
  systemctl enable ssh </dev/null >/dev/null 2>&1 || true
  systemctl restart ssh </dev/null >/dev/null 2>&1 || systemctl restart ssh.socket </dev/null >/dev/null 2>&1 || true
fi
if command -v sw_vers >/dev/null 2>&1; then
	mkdir -p /usr/local/bin
	remote_login_log=/tmp/crabbox-remote-login.log
  /bin/launchctl load -w /System/Library/LaunchDaemons/ssh.plist </dev/null >"$remote_login_log" 2>&1 ||
    /bin/launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist </dev/null >>"$remote_login_log" 2>&1 || true
  /bin/launchctl enable system/com.openssh.sshd </dev/null >>"$remote_login_log" 2>&1 || true
  /bin/launchctl kickstart -k system/com.openssh.sshd </dev/null >>"$remote_login_log" 2>&1 || true
  if [ "$desktop" = true ]; then
%s
  fi
  cat >/usr/local/bin/crabbox-ready <<'READY'
#!/bin/sh
set -eu
export PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
rsync --version >/dev/null
curl --version >/dev/null
node --version >/dev/null
npm --version >/dev/null
test -w %s
ssh_ready=0
for port in %s; do
  if nc -z 127.0.0.1 "$port" >/dev/null 2>&1; then
    ssh_ready=1
    break
  fi
done
test "$ssh_ready" -eq 1
READY
else
  cat >/usr/local/bin/crabbox-ready <<'READY'
#!/usr/bin/env bash
set -euo pipefail
git --version >/dev/null
rsync --version >/dev/null
curl --version >/dev/null
jq --version >/dev/null
test -w %s
READY
fi
chmod 0755 /usr/local/bin/crabbox-ready
touch /var/lib/crabbox/bootstrapped 2>/dev/null || true
/usr/local/bin/crabbox-ready </dev/null
`, shellWords([]string{user})[0], shellWords([]string{workRoot})[0], desktop, parallelsShellPortList(sshPorts), parallelsMacOSNodeBaselineStanza(), parallelsMacOSDesktopReadyTest(macOSAccountCredentials), parallelsMacOSDesktopSetupScript(macOSAccountCredentials), shellWords([]string{workRoot})[0], parallelsShellPortList(sshPorts), shellWords([]string{workRoot})[0])
}

func parallelsChildCommandEnv(extraEnv []string) []string {
	env := extraEnv
	if env == nil {
		env = os.Environ()
	}
	return childEnvironmentWithout(env, parallelsPasswordEnvName)
}

func (c *ParallelsClient) prlctl(ctx context.Context, extraEnv []string, args ...string) (LocalCommandResult, error) {
	return c.prlctlWithStdin(ctx, nil, extraEnv, args...)
}

// prlctlWithStdin is prlctl with a reader attached to the guest command's
// standard input. `prlctl exec` does not preserve argv boundaries -- it joins
// its arguments into one string and re-parses that string with a shell inside
// the guest -- so anything that has to survive verbatim, a shell script above
// all, travels on stdin instead. ssh forwards stdin to the remote prlctl, so
// both routes deliver the same bytes.
// https://github.com/openclaw/crabbox/issues/2396
func (c *ParallelsClient) prlctlWithStdin(ctx context.Context, stdin io.Reader, extraEnv []string, args ...string) (LocalCommandResult, error) {
	return c.parallelsBinary(ctx, stdin, extraEnv, "prlctl", args...)
}

// prlsrvctl reaches the Parallels service rather than a VM. It carries the
// host's own attested identity, which is what a fixed lease binds to.
func (c *ParallelsClient) prlsrvctl(ctx context.Context, args ...string) (LocalCommandResult, error) {
	return c.parallelsBinary(ctx, nil, nil, "prlsrvctl", args...)
}

func (c *ParallelsClient) parallelsBinary(ctx context.Context, stdin io.Reader, extraEnv []string, binary string, args ...string) (LocalCommandResult, error) {
	env := parallelsChildCommandEnv(extraEnv)
	if c.Cfg.Parallels.Host != "" {
		remote := "PATH=/usr/local/bin:/opt/homebrew/bin:$PATH " + strings.Join(shellWords(append([]string{binary}, args...)), " ")
		sshArgs := []string{}
		if c.Cfg.Parallels.HostKey != "" {
			sshArgs = append(sshArgs, "-i", c.Cfg.Parallels.HostKey, "-o", "IdentitiesOnly=yes")
		}
		host := c.Cfg.Parallels.Host
		if c.Cfg.Parallels.HostUser != "" {
			host = c.Cfg.Parallels.HostUser + "@" + host
		}
		sshArgs = append(sshArgs, host, remote)
		return c.Runner.Run(ctx, LocalCommandRequest{Name: directSSHExecutable(), Args: sshArgs, Stdin: stdin, Env: env})
	}
	return c.Runner.Run(ctx, LocalCommandRequest{Name: binary, Args: args, Stdin: stdin, Env: env})
}

func (c *ParallelsClient) hostCommand(ctx context.Context, stdin io.Reader, args ...string) (LocalCommandResult, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return LocalCommandResult{}, Exit(2, "Parallels host command is empty")
	}
	env := parallelsChildCommandEnv(nil)
	if c.Cfg.Parallels.Host != "" {
		sshArgs := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
		if c.Cfg.Parallels.HostKey != "" {
			sshArgs = append(sshArgs, "-i", c.Cfg.Parallels.HostKey, "-o", "IdentitiesOnly=yes")
		}
		host := c.Cfg.Parallels.Host
		if c.Cfg.Parallels.HostUser != "" {
			host = c.Cfg.Parallels.HostUser + "@" + host
		}
		sshArgs = append(sshArgs, host, strings.Join(shellWords(args), " "))
		return c.Runner.Run(ctx, LocalCommandRequest{Name: directSSHExecutable(), Args: sshArgs, Stdin: stdin, Env: env})
	}
	return c.Runner.Run(ctx, LocalCommandRequest{Name: args[0], Args: args[1:], Stdin: stdin, Env: env})
}

func validateParallelsSnapshotCloneMode(snapshot ParallelsSnapshot, cloneMode string) error {
	switch strings.ToLower(strings.TrimSpace(cloneMode)) {
	case "", "linked":
		if !strings.EqualFold(snapshot.State, "poweroff") {
			return Exit(2, "Parallels linked clones require a power-off snapshot; snapshot %q state=%s", snapshot.Name, blank(snapshot.State, "unknown"))
		}
		return nil
	case "full", "unlink":
		return Exit(2, "Parallels snapshot forks require cloneMode=linked; prlctl selects snapshots only for linked clones")
	default:
		return Exit(2, "parallels.cloneMode must be linked, full, or unlink")
	}
}

func parseParallelsVMs(data string) ([]ParallelsVM, error) {
	var raw []map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, fmt.Errorf("parse parallels VM JSON: %w", err)
	}
	out := make([]ParallelsVM, 0, len(raw))
	for _, item := range raw {
		vm := ParallelsVM{
			ID:       firstJSONField(item, "ID", "uuid"),
			Name:     firstJSONField(item, "Name", "name"),
			State:    firstJSONField(item, "State", "status"),
			OS:       firstJSONField(item, "OS", "os"),
			Home:     firstJSONField(item, "Home", "home"),
			IP:       cleanParallelsIP(firstJSONField(item, "ip_configured")),
			Template: strings.EqualFold(firstJSONField(item, "Template"), "yes"),
		}
		if vm.IP == "" {
			if network, ok := item["Network"].(map[string]any); ok {
				vm.IP = firstParallelsNetworkIP(network)
			}
		}
		vm.MACs = parallelsNetworkMACs(item)
		out = append(out, vm)
	}
	return out, nil
}

func parallelsNetworkMACs(item map[string]any) []string {
	hardware, ok := item["Hardware"].(map[string]any)
	if !ok {
		return nil
	}
	seen := map[string]struct{}{}
	for name, raw := range hardware {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "net") {
			continue
		}
		adapter, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if enabled, ok := adapter["enabled"].(bool); ok && !enabled {
			continue
		}
		mac, ok := normalizeParallelsMAC(firstJSONField(adapter, "mac", "MAC"))
		if ok {
			seen[mac] = struct{}{}
		}
	}
	macs := make([]string, 0, len(seen))
	for mac := range seen {
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	return macs
}

func normalizeParallelsMAC(value string) (string, bool) {
	value = strings.TrimSpace(value)
	var normalized strings.Builder
	normalized.Grow(12)
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			normalized.WriteRune(r)
		case r >= 'a' && r <= 'f':
			normalized.WriteRune(r)
		case r >= 'A' && r <= 'F':
			normalized.WriteRune(r + ('a' - 'A'))
		case r == ':' || r == '-' || r == '.':
		default:
			return "", false
		}
	}
	if normalized.Len() != 12 {
		return "", false
	}
	return normalized.String(), true
}

func parseParallelsDHCPLeases(data string) ([]parallelsDHCPLease, error) {
	var leases []parallelsDHCPLease
	for index, rawLine := range strings.Split(data, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("parse Parallels DHCP leases line %d: missing '='", index+1)
		}
		ip := net.ParseIP(strings.TrimSpace(parts[0]))
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("parse Parallels DHCP leases line %d: invalid IPv4 address", index+1)
		}
		value := strings.TrimSpace(parts[1])
		if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
			return nil, fmt.Errorf("parse Parallels DHCP leases line %d: invalid quoted record", index+1)
		}
		fields := strings.Split(value[1:len(value)-1], ",")
		if len(fields) < 3 {
			return nil, fmt.Errorf("parse Parallels DHCP leases line %d: incomplete record", index+1)
		}
		expires, err := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64)
		if err != nil || expires <= 0 {
			return nil, fmt.Errorf("parse Parallels DHCP leases line %d: invalid expiry", index+1)
		}
		mac, ok := normalizeParallelsMAC(fields[2])
		if !ok {
			return nil, fmt.Errorf("parse Parallels DHCP leases line %d: invalid MAC", index+1)
		}
		leases = append(leases, parallelsDHCPLease{IP: ip.String(), MAC: mac, ExpiresAt: time.Unix(expires, 0)})
	}
	return leases, nil
}

func resolveParallelsDHCPLeaseIP(data string, macs []string, now time.Time) (string, error) {
	wanted := map[string]struct{}{}
	for _, value := range macs {
		if mac, ok := normalizeParallelsMAC(value); ok {
			wanted[mac] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return "", fmt.Errorf("Parallels VM has no usable NIC MAC for DHCP fallback")
	}
	leases, err := parseParallelsDHCPLeases(data)
	if err != nil {
		return "", err
	}
	fresh := map[string]struct{}{}
	stale := false
	for _, lease := range leases {
		if _, ok := wanted[lease.MAC]; !ok {
			continue
		}
		if !lease.ExpiresAt.After(now) {
			stale = true
			continue
		}
		fresh[lease.IP] = struct{}{}
	}
	if len(fresh) > 1 {
		ips := make([]string, 0, len(fresh))
		for ip := range fresh {
			ips = append(ips, ip)
		}
		sort.Strings(ips)
		return "", fmt.Errorf("%w for VM MACs: %s", errParallelsDHCPLeaseAmbiguous, strings.Join(ips, ", "))
	}
	for ip := range fresh {
		return ip, nil
	}
	if stale {
		return "", fmt.Errorf("no fresh Parallels DHCP lease for VM MACs")
	}
	return "", errParallelsDHCPLeaseMissing
}

func parseParallelsSnapshots(data string) ([]ParallelsSnapshot, error) {
	var raw map[string]struct {
		Name    string `json:"name"`
		Date    string `json:"date"`
		State   string `json:"state"`
		Current bool   `json:"current"`
		Parent  string `json:"parent"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, fmt.Errorf("parse parallels snapshot JSON: %w", err)
	}
	out := make([]ParallelsSnapshot, 0, len(raw))
	for id, item := range raw {
		out = append(out, ParallelsSnapshot{ID: id, Name: item.Name, Date: item.Date, State: item.State, Current: item.Current, Parent: item.Parent})
	}
	return out, nil
}

func firstJSONField(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := item[key]; ok {
			if s, ok := value.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func firstParallelsNetworkIP(network map[string]any) string {
	values, ok := network["ipAddresses"].([]any)
	if !ok {
		return ""
	}
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok || item["type"] != "ipv4" {
			continue
		}
		if ip, ok := item["ip"].(string); ok {
			return cleanParallelsIP(ip)
		}
	}
	return ""
}

func cleanParallelsIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "-" {
		return ""
	}
	if strings.Contains(value, " ") {
		return strings.Fields(value)[0]
	}
	return value
}

func parallelsHostMatchesTarget(host ParallelsHostConfig, target string) bool {
	if len(host.Targets) == 0 || strings.TrimSpace(target) == "" {
		return true
	}
	target = normalizeTargetOS(target)
	for _, candidate := range host.Targets {
		if normalizeTargetOS(candidate) == target {
			return true
		}
	}
	return false
}

func parallelsVMListContains(vms []ParallelsVM, id string) bool {
	id = strings.TrimSpace(id)
	for _, vm := range vms {
		if vm.ID == id || vm.Name == id || strings.Trim(vm.ID, "{}") == strings.Trim(id, "{}") {
			return true
		}
	}
	return false
}

func parallelsHostMaxVMs(cfg Config) int {
	for _, host := range cfg.Parallels.Hosts {
		if cfg.Parallels.SelectedHost == firstNonBlank(host.Name, host.Host, "local") {
			return host.MaxVMs
		}
	}
	// No fleet entry selected, so this is the direct host. A matched fleet entry
	// returns above even at zero, so this is not a default for fleet entries.
	return cfg.Parallels.MaxVMs
}

func parallelsHostWithinCapacity(cfg Config, vms []ParallelsVM) bool {
	limit := parallelsHostMaxVMs(cfg)
	if limit <= 0 {
		return true
	}
	count := 0
	for _, vm := range vms {
		if strings.HasPrefix(vm.Name, "crabbox-") {
			count++
		}
	}
	return count < limit
}

func parallelsVMToServer(cfg Config, vm ParallelsVM, labels map[string]string) Server {
	if labels == nil {
		labels = map[string]string{}
	}
	labels["provider"] = parallelsProvider
	if labels["lease"] == "" {
		labels["lease"] = vm.ID
	}
	server := Server{
		CloudID:  vm.ID,
		Provider: parallelsProvider,
		Name:     vm.Name,
		Status:   strings.ToLower(blank(vm.State, "unknown")),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = vm.IP
	server.ServerType.Name = parallelsServerTypeForConfig(cfg)
	return server
}

func parallelsLabelsFromName(name string) map[string]string {
	labels := map[string]string{"provider": parallelsProvider}
	leaseID, slug := parallelsLeaseFromVMName(name)
	if leaseID != "" {
		labels["lease"] = leaseID
		if stored, err := readParallelsLeaseLabels(leaseID); err == nil {
			for key, value := range stored {
				labels[key] = value
			}
			labels["lease"] = leaseID
		}
	}
	if slug != "" {
		labels["slug"] = slug
	}
	return labels
}

func ParallelsLabelsFromName(name string) map[string]string {
	return parallelsLabelsFromName(name)
}

// ParallelsLeaseVMName renders the host-unique VM name that carries a lease
// identity. The name is the Parallels idempotency key: prlctl refuses a second
// VM with the same name on a host, so a provider adapter can bind a fixed lease
// ID to a create attempt without pre-allocating any other resource identifier.
func ParallelsLeaseVMName(leaseID, slug string) string {
	return parallelsLeaseVMName(leaseID, slug)
}

// ParallelsHostRefForConfig reports the resolved Parallels host identity used as
// the provider scope of a lease: the selected fleet host, an explicit remote
// host, or the local Mac.
func ParallelsHostRefForConfig(cfg Config) string {
	return parallelsHostRefForConfig(cfg)
}

func parallelsLeaseVMName(leaseID, slug string) string {
	base := strings.ReplaceAll(leaseID, "_", "-")
	if normalized := NormalizeLeaseSlug(slug); normalized != "" {
		return "crabbox-" + base + "-" + normalized
	}
	return "crabbox-" + base
}

func parallelsLeaseFromVMName(name string) (string, string) {
	rest := strings.TrimPrefix(name, "crabbox-")
	if rest == name {
		return "", ""
	}
	parts := strings.SplitN(rest, "-", 3)
	if len(parts) < 2 || parts[0] != "cbx" {
		return "", NormalizeLeaseSlug(rest)
	}
	leaseID := "cbx_" + parts[1]
	slug := ""
	if len(parts) == 3 {
		slug = NormalizeLeaseSlug(parts[2])
	}
	return leaseID, slug
}

func writeParallelsLeaseLabels(leaseID string, labels map[string]string) error {
	if leaseID == "" || labels == nil {
		return nil
	}
	path, err := parallelsLeaseLabelsPath(leaseID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(labels, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func readParallelsLeaseLabels(leaseID string) (map[string]string, error) {
	path, err := parallelsLeaseLabelsPath(leaseID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var labels map[string]string
	if err := json.Unmarshal(data, &labels); err != nil {
		return nil, err
	}
	return labels, nil
}

func removeParallelsLeaseLabels(leaseID string) {
	path, err := parallelsLeaseLabelsPath(leaseID)
	if err == nil {
		_ = os.Remove(path)
	}
}

func parallelsLeaseLabelsPath(leaseID string) (string, error) {
	dir, err := CrabboxStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "parallels", "leases", leaseID+".json"), nil
}

func commandOutputError(action string, result LocalCommandResult, err error) error {
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}
