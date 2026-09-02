package cli

import (
	"context"
	"encoding/base64"
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
)

const parallelsProvider = "parallels"
const parallelsDHCPLeasesPath = "/Library/Preferences/Parallels/parallels_dhcp_leases"

var errParallelsDHCPLeaseAmbiguous = errors.New("ambiguous Parallels DHCP lease")

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
	return &ParallelsClient{Cfg: cfg, Runner: runner}
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

func SelectParallelsFleetConfig(ctx context.Context, cfg Config, runner CommandRunner, source string) (Config, error) {
	var lastErr error
	for _, candidate := range ParallelsCandidateConfigs(cfg) {
		client := NewParallelsClient(candidate, runner)
		vms, err := client.ListVMs(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		if source != "" && !parallelsVMListContains(vms, source) {
			lastErr = exit(4, "Parallels source VM %q not found on host %s", source, parallelsHostRefForConfig(candidate))
			continue
		}
		if !parallelsHostWithinCapacity(candidate, vms) {
			lastErr = exit(5, "Parallels host %s is at maxVMs capacity", parallelsHostRefForConfig(candidate))
			continue
		}
		return candidate, nil
	}
	if lastErr != nil {
		return Config{}, lastErr
	}
	return cfg, nil
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
	return Config{}, ParallelsVM{}, exit(4, "parallels VM not found: %s", id)
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
	return slug != "" && normalizeLeaseSlug(slug) == normalizeLeaseSlug(id)
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
		return ParallelsVM{}, exit(4, "parallels VM not found: %s", id)
	}
	return vms[0], nil
}

func (c *ParallelsClient) Clone(ctx context.Context, source, snapshotID, leaseID, slug string, keep bool) (Server, error) {
	if strings.TrimSpace(source) == "" {
		return Server{}, exit(2, "parallels.source or parallels.sourceId is required")
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
			return Server{}, exit(2, "Parallels linked clones require --parallels-source-snapshot or --parallels-source-snapshot-id; otherwise prlctl creates a source-side linked-clone snapshot")
		}
		if snapshotID != "" {
			snapshot, ok, err := c.snapshotByID(ctx, source, snapshotID)
			if err != nil {
				return Server{}, err
			}
			if ok {
				if err := validateParallelsSnapshotCloneMode(snapshot, c.Cfg.Parallels.CloneMode); err != nil {
					return Server{}, err
				}
			}
		}
		args = append(args, "--linked")
	case "full":
		if snapshotID != "" {
			return Server{}, exit(2, "Parallels snapshot forks require cloneMode=linked; prlctl selects snapshots only for linked clones")
		}
	case "unlink":
		if snapshotID != "" {
			return Server{}, exit(2, "Parallels snapshot forks require cloneMode=linked; prlctl selects snapshots only for linked clones")
		}
		args = append(args, "--unlink")
	default:
		return Server{}, exit(2, "parallels.cloneMode must be linked, full, or unlink")
	}
	if snapshotID != "" {
		args = append(args, "-i", snapshotID)
	}
	result, err := c.prlctl(ctx, nil, args...)
	if err != nil {
		return Server{}, commandOutputError("parallels clone", result, err)
	}
	labels := directLeaseLabels(c.Cfg, leaseID, slug, parallelsProvider, "", keep, time.Now().UTC())
	labels["source"] = source
	labels["host"] = parallelsHostRefForConfig(c.Cfg)
	if snapshotID != "" {
		labels["source_snapshot"] = snapshotID
	}
	vm, err := c.GetVM(ctx, name)
	if err != nil {
		return Server{}, err
	}
	server := parallelsVMToServer(c.Cfg, vm, labels)
	_ = writeParallelsLeaseLabels(leaseID, server.Labels)
	return server, nil
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
		return exit(2, "refusing to delete non-Crabbox Parallels VM %q", vm.Name)
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
		return exit(2, "parallels.bootstrapKey is supported only for macOS guests")
	}
	if !filepath.IsAbs(key) || strings.ContainsAny(key, "\r\n\x00") {
		return exit(2, "parallels.bootstrapKey must be an absolute path on the Parallels host")
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
		return exit(2, "parallels guest SSH user is required")
	}
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return exit(2, "parallels guest SSH public key is empty")
	}
	if cfg.TargetOS == targetWindows {
		return c.runWindowsPowerShellFile(ctx, vmID, "install-ssh", windowsBootstrapPowerShell(cfg, publicKey))
	}
	args := []string{"/bin/sh", "-lc", parallelsPOSIXInstallSSHKeyScript(user, publicKey)}
	result, err := c.prlctl(ctx, nil, append([]string{"exec", vmID}, args...)...)
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
		return exit(2, "Parallels SSH bootstrap fallback is supported only for macOS guests")
	}
	bootstrapKey := strings.TrimSpace(cfg.Parallels.BootstrapKey)
	if !filepath.IsAbs(bootstrapKey) || strings.ContainsAny(bootstrapKey, "\r\n\x00") {
		return exit(2, "parallels.bootstrapKey must be an absolute path on the Parallels host")
	}
	user := strings.TrimSpace(cfg.SSHUser)
	if user == "" {
		return exit(2, "parallels guest SSH user is required")
	}
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil || parsedIP.To4() == nil {
		return exit(5, "Parallels DHCP fallback returned invalid IPv4 address %q", ip)
	}
	port := strings.TrimSpace(cfg.SSHPort)
	if port == "" {
		port = "22"
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return exit(2, "invalid Parallels guest SSH port %q", port)
	}
	script := parallelsPOSIXInstallSSHKeyScript(user, publicKey) + "\n" + parallelsPOSIXEnsureReadyScript(user, cfg.WorkRoot, cfg.Desktop, cfg.Parallels.Password != "")
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
	args := strings.Fields(powershellCommand(script))
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
		return exit(2, "parallels guest SSH user is required")
	}
	workRoot := strings.TrimSpace(cfg.WorkRoot)
	if workRoot == "" {
		workRoot = baseConfig().WorkRoot
	}
	desktop := cfg.Desktop
	result, err := c.prlctl(ctx, nil, "exec", vmID, "/bin/sh", "-lc", parallelsPOSIXEnsureReadyScript(user, workRoot, desktop, cfg.TargetOS == targetMacOS && cfg.Parallels.Password != ""))
	if err != nil {
		return commandOutputError("parallels guest prep", result, err)
	}
	return nil
}

func (c *ParallelsClient) WaitForIP(ctx context.Context, id string, timeout time.Duration) (ParallelsVM, error) {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	var last ParallelsVM
	var lastDHCPError error
	useDHCPFallback := c.Cfg.TargetOS == targetMacOS && strings.TrimSpace(c.Cfg.Parallels.BootstrapKey) != ""
	for {
		vm, err := c.GetVM(ctx, id)
		if err == nil {
			last = vm
			if vm.IP != "" {
				vm.IPSource = "tools"
				return vm, nil
			}
			if useDHCPFallback {
				data, readErr := c.readDHCPLeases(ctx)
				if readErr != nil {
					lastDHCPError = readErr
				} else if ip, resolveErr := resolveParallelsDHCPLeaseIP(data, vm.MACs, time.Now()); resolveErr != nil {
					lastDHCPError = resolveErr
					if errors.Is(resolveErr, errParallelsDHCPLeaseAmbiguous) {
						return ParallelsVM{}, resolveErr
					}
				} else if probeErr := c.probeHostTCP(ctx, ip, c.Cfg.SSHPort); probeErr != nil {
					lastDHCPError = probeErr
				} else {
					vm.IP = ip
					vm.IPSource = "dhcp-mac"
					return vm, nil
				}
			}
		}
		if time.Now().After(deadline) {
			if lastDHCPError != nil {
				return ParallelsVM{}, exit(5, "timed out waiting for Parallels VM %s IP; last_state=%s; DHCP fallback: %v", id, blank(last.State, "-"), lastDHCPError)
			}
			return ParallelsVM{}, exit(5, "timed out waiting for Parallels VM %s IP; last_state=%s", id, blank(last.State, "-"))
		}
		select {
		case <-ctx.Done():
			return ParallelsVM{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *ParallelsClient) WaitForGuestExec(ctx context.Context, id string, cfg Config, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		var args []string
		if cfg.TargetOS == targetWindows {
			args = strings.Fields(powershellCommand(`"ok" | Out-Null`))
		} else {
			args = []string{"/bin/sh", "-lc", "true"}
		}
		result, err := c.prlctl(ctx, nil, append([]string{"exec", id}, args...)...)
		if err == nil {
			return nil
		}
		lastErr = commandOutputError("parallels guest exec", result, err)
		if c.Cfg.TargetOS == targetMacOS && strings.TrimSpace(c.Cfg.Parallels.BootstrapKey) != "" && ParallelsGuestToolsUnavailable(lastErr) {
			return lastErr
		}
		if time.Now().After(deadline) {
			return exit(5, "timed out waiting for Parallels guest exec in %s: %v", id, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func ParallelsGuestToolsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "prl_err_vm_exec_guest_tool_not_available") ||
		strings.Contains(message, "guest tools are not available") ||
		strings.Contains(message, "guest tools not available")
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
	return ParallelsSnapshot{}, exit(4, "Parallels snapshot %q not found for VM %s", value, vmID)
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
	return ParallelsSnapshot{}, exit(5, "Parallels snapshot %q was created but not found in snapshot-list", name)
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
		return exit(5, "Parallels DHCP fallback returned invalid IPv4 address %q", ip)
	}
	port = strings.TrimSpace(port)
	if port == "" {
		port = "22"
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return exit(2, "invalid Parallels guest SSH port %q", port)
	}
	result, err := c.hostCommand(ctx, nil, "/usr/bin/nc", "-z", "-w", "2", parsedIP.String(), port)
	if err != nil {
		return commandOutputError(fmt.Sprintf("verify Parallels DHCP guest SSH reachability at %s:%s", parsedIP.String(), port), result, err)
	}
	return nil
}

func parallelsPOSIXInstallSSHKeyScript(user, publicKey string) string {
	return fmt.Sprintf(`set -eu
user=%s
key=%s
home=$(getent passwd "$user" 2>/dev/null | cut -d: -f6 || true)
if [ -z "$home" ]; then
  home=$(dscl . -read "/Users/$user" NFSHomeDirectory 2>/dev/null | awk '{print $2}' || true)
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
		return "[ -f /var/db/crabbox/vnc.console ] && nc -z 127.0.0.1 5900"
	}
	return "[ -s /var/db/crabbox/vnc.password ] && [ -f /var/db/crabbox/vnc.console ] && nc -z 127.0.0.1 5900"
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
      *) vnc_password="$(/usr/bin/openssl rand -hex 4)" ;;
    esac
    case "$vnc_password" in
      *[!A-Za-z0-9]*) echo "invalid generated VNC password" >&2; exit 1 ;;
    esac
    printf '%s\n' "$vnc_password" >/var/db/crabbox/vnc.password
    chmod 0600 /var/db/crabbox/vnc.password
    mkdir -p /etc/sudoers.d
    printf '%s ALL=(root) NOPASSWD: /bin/cat /var/db/crabbox/vnc.password\n' "$user" >/etc/sudoers.d/crabbox-vnc-password
    chmod 0440 /etc/sudoers.d/crabbox-vnc-password
    /usr/sbin/visudo -cf /etc/sudoers.d/crabbox-vnc-password >/dev/null
`
		clientOptions = `    "$kickstart" -configure -clientopts -setdirlogins -dirlogins no -setvnclegacy -vnclegacy yes -setvncpw -vncpw "$vnc_password" >/dev/null 2>&1
`
	}
	return `    mkdir -p /var/db/crabbox
` + credentialSetup + `    kickstart=/System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart
    [ -x "$kickstart" ]
    /usr/bin/defaults write /Library/Preferences/com.apple.RemoteManagement VNCAlwaysStartOnConsole -bool true
    "$kickstart" -activate -configure -allowAccessFor -specifiedUsers >/dev/null 2>&1
    "$kickstart" -configure -access -on -users "$user" -privs -all >/dev/null 2>&1
` + clientOptions + `    "$kickstart" -restart -agent >/dev/null 2>&1
    /bin/launchctl enable system/com.apple.screensharing >/dev/null 2>&1 || true
    /bin/launchctl kickstart -k system/com.apple.screensharing >/dev/null 2>&1 || true
    vnc_ready=false
    for _ in $(jot 60 1); do
      if nc -z 127.0.0.1 5900; then
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

func parallelsPOSIXEnsureReadyScript(user, workRoot string, desktop, macOSAccountCredentials bool) string {
	return fmt.Sprintf(`set -eu
user=%s
work_root=%s
desktop=%t
if [ -x /usr/local/bin/crabbox-ready ] && /usr/local/bin/crabbox-ready >/tmp/crabbox-ready.log 2>&1; then
  if [ "$desktop" != true ]; then
    exit 0
  fi
  if command -v sw_vers >/dev/null 2>&1; then
    if %s; then
      exit 0
    fi
  elif command -v websockify >/dev/null 2>&1 && command -v x11vnc >/dev/null 2>&1 && { [ -f /usr/share/novnc/vnc.html ] || [ -f /usr/share/novnc/core/vnc.html ] || [ -f /usr/share/novnc/html/vnc.html ]; } && systemctl is-active --quiet crabbox-x11vnc.service; then
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
  apt-get update
  apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq
  if [ "$desktop" = true ]; then
    apt-get install -y --no-install-recommends xvfb xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings x11vnc xauth dbus-x11 x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel fonts-dejavu-core fonts-liberation iproute2 openssl util-linux novnc websockify
    if [ ! -s /var/lib/crabbox/vnc.password ]; then
      umask 077
      openssl rand -hex 16 >/var/lib/crabbox/vnc.password
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
    systemctl daemon-reload
    systemctl enable --now crabbox-xvfb.service crabbox-desktop.service crabbox-x11vnc.service
  fi
  systemctl enable ssh >/dev/null 2>&1 || true
  systemctl restart ssh >/dev/null 2>&1 || systemctl restart ssh.socket >/dev/null 2>&1 || true
fi
if command -v sw_vers >/dev/null 2>&1; then
	mkdir -p /usr/local/bin
	remote_login_log=/tmp/crabbox-remote-login.log
  /bin/launchctl load -w /System/Library/LaunchDaemons/ssh.plist >"$remote_login_log" 2>&1 ||
    /bin/launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist >>"$remote_login_log" 2>&1 || true
  /bin/launchctl enable system/com.openssh.sshd >>"$remote_login_log" 2>&1 || true
  /bin/launchctl kickstart -k system/com.openssh.sshd >>"$remote_login_log" 2>&1 || true
  if [ "$desktop" = true ]; then
%s
  fi
  cat >/usr/local/bin/crabbox-ready <<'READY'
#!/bin/sh
set -eu
rsync --version >/dev/null
curl --version >/dev/null
test -w %s
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
/usr/local/bin/crabbox-ready
`, shellWords([]string{user})[0], shellWords([]string{workRoot})[0], desktop, parallelsMacOSDesktopReadyTest(macOSAccountCredentials), parallelsMacOSDesktopSetupScript(macOSAccountCredentials), shellWords([]string{workRoot})[0], shellWords([]string{workRoot})[0])
}

func (c *ParallelsClient) prlctl(ctx context.Context, extraEnv []string, args ...string) (LocalCommandResult, error) {
	if c.Cfg.Parallels.Host != "" {
		remote := "PATH=/usr/local/bin:/opt/homebrew/bin:$PATH " + strings.Join(shellWords(append([]string{"prlctl"}, args...)), " ")
		sshArgs := []string{}
		if c.Cfg.Parallels.HostKey != "" {
			sshArgs = append(sshArgs, "-i", c.Cfg.Parallels.HostKey, "-o", "IdentitiesOnly=yes")
		}
		host := c.Cfg.Parallels.Host
		if c.Cfg.Parallels.HostUser != "" {
			host = c.Cfg.Parallels.HostUser + "@" + host
		}
		sshArgs = append(sshArgs, host, remote)
		return c.Runner.Run(ctx, LocalCommandRequest{Name: directSSHExecutable(), Args: sshArgs, Env: extraEnv})
	}
	return c.Runner.Run(ctx, LocalCommandRequest{Name: "prlctl", Args: args, Env: extraEnv})
}

func (c *ParallelsClient) hostCommand(ctx context.Context, stdin io.Reader, args ...string) (LocalCommandResult, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return LocalCommandResult{}, exit(2, "Parallels host command is empty")
	}
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
		return c.Runner.Run(ctx, LocalCommandRequest{Name: directSSHExecutable(), Args: sshArgs, Stdin: stdin})
	}
	return c.Runner.Run(ctx, LocalCommandRequest{Name: args[0], Args: args[1:], Stdin: stdin})
}

func validateParallelsSnapshotCloneMode(snapshot ParallelsSnapshot, cloneMode string) error {
	switch strings.ToLower(strings.TrimSpace(cloneMode)) {
	case "", "linked":
		if !strings.EqualFold(snapshot.State, "poweroff") {
			return exit(2, "Parallels linked clones require a power-off snapshot; snapshot %q state=%s", snapshot.Name, blank(snapshot.State, "unknown"))
		}
		return nil
	case "full", "unlink":
		return exit(2, "Parallels snapshot forks require cloneMode=linked; prlctl selects snapshots only for linked clones")
	default:
		return exit(2, "parallels.cloneMode must be linked, full, or unlink")
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
	return "", fmt.Errorf("no Parallels DHCP lease for VM MACs")
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

func parallelsHostWithinCapacity(cfg Config, vms []ParallelsVM) bool {
	limit := 0
	for _, host := range cfg.Parallels.Hosts {
		if cfg.Parallels.SelectedHost == firstNonBlank(host.Name, host.Host, "local") {
			limit = host.MaxVMs
			break
		}
	}
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

func parallelsLeaseVMName(leaseID, slug string) string {
	base := strings.ReplaceAll(leaseID, "_", "-")
	if normalized := normalizeLeaseSlug(slug); normalized != "" {
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
		return "", normalizeLeaseSlug(rest)
	}
	leaseID := "cbx_" + parts[1]
	slug := ""
	if len(parts) == 3 {
		slug = normalizeLeaseSlug(parts[2])
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
	dir, err := crabboxStateDir()
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
