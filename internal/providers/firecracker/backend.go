package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type Runtime = core.Runtime
type ProviderSpec = core.ProviderSpec
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type DoctorCheck = core.DoctorCheck
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type CleanupRequest = core.CleanupRequest
type LeaseTarget = core.LeaseTarget
type Server = core.Server

const (
	providerName           = "firecracker"
	firecrackerNetworkCNI  = "cni"
	firecrackerSSHPort     = "22"
	firecrackerServerClass = "microvm"
)

var (
	firecrackerHostGOOS = runtime.GOOS
	firecrackerLookPath = exec.LookPath
	firecrackerStat     = os.Stat
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func applyDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.Provider = providerName
	base := core.BaseConfig()
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.TargetOS == core.TargetLinux {
		cfg.WindowsMode = ""
	}
	if user := strings.TrimSpace(cfg.Firecracker.User); user != "" &&
		(cfg.SSHUser == "" || cfg.SSHUser == base.SSHUser || user != strings.TrimSpace(base.Firecracker.User)) {
		cfg.SSHUser = user
	}
	if workRoot := strings.TrimSpace(cfg.Firecracker.WorkRoot); workRoot != "" &&
		(core.IsDefaultWorkRoot(cfg.WorkRoot) || workRoot != strings.TrimSpace(base.Firecracker.WorkRoot)) {
		cfg.WorkRoot = workRoot
	}
	currentSSHPort := strings.TrimSpace(cfg.SSHPort)
	if currentSSHPort == "" || currentSSHPort == strings.TrimSpace(base.SSHPort) {
		cfg.SSHPort = firecrackerSSHPort
	} else {
		cfg.SSHPort = currentSSHPort
	}
	cfg.SSHFallbackPorts = nil
	if !cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) == "" {
		cfg.ServerType = firecrackerServerTypeForConfig(*cfg)
	}
}

func firecrackerServerTypeForConfig(_ Config) string {
	return firecrackerServerClass
}

func normalizeFirecrackerNetwork(value string) string {
	mode := strings.ToLower(strings.TrimSpace(value))
	if mode == "" {
		return firecrackerNetworkCNI
	}
	return mode
}

func validateConfig(cfg Config) error {
	applyDefaults(&cfg)
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.Exit(2, "provider=firecracker supports target=linux only")
	}
	if cfg.Tailscale.Enabled || cfg.Network == core.NetworkTailscale {
		return core.Exit(2, "provider=firecracker does not support tailscale-managed networking")
	}
	if strings.TrimSpace(cfg.Firecracker.User) == "" {
		return core.Exit(2, "provider=firecracker requires firecracker.user")
	}
	workRoot := strings.TrimSpace(cfg.Firecracker.WorkRoot)
	if workRoot == "" || !strings.HasPrefix(workRoot, "/") {
		return core.Exit(2, "provider=firecracker requires firecracker.workRoot to be an absolute POSIX path")
	}
	if cfg.Firecracker.CPUs <= 0 {
		return core.Exit(2, "provider=firecracker requires firecracker.cpus > 0")
	}
	if cfg.Firecracker.MemoryMiB <= 0 {
		return core.Exit(2, "provider=firecracker requires firecracker.memoryMiB > 0")
	}
	if cfg.Firecracker.DiskMiB <= 0 {
		return core.Exit(2, "provider=firecracker requires firecracker.diskMiB > 0")
	}
	if mode := normalizeFirecrackerNetwork(cfg.Firecracker.Network); mode != firecrackerNetworkCNI {
		return core.Exit(2, "provider=firecracker supports firecracker.network=%s only", firecrackerNetworkCNI)
	}
	return nil
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, unsupportedLifecycle("acquire")
}

func (b *backend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return LeaseTarget{}, unsupportedLifecycle("resolve")
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, unsupportedLifecycle("list")
}

func (b *backend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return unsupportedLifecycle("release")
}

func (b *backend) Cleanup(context.Context, CleanupRequest) error {
	return unsupportedLifecycle("cleanup")
}

func unsupportedLifecycle(operation string) error {
	return core.Exit(2, "provider=firecracker %s is not implemented yet; PLAN-01 only ships the provider contract and read-only doctor checks", operation)
}

func (b *backend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Provider == "" {
		server.Provider = providerName
	}
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		state = "touched"
	}
	server.Status = state
	server.Labels["state"] = state
	return server, nil
}

func (b *backend) Doctor(_ context.Context, _ DoctorRequest) (DoctorResult, error) {
	cfg := b.cfg
	applyDefaults(&cfg)
	checks := []DoctorCheck{
		doctorHostCheck(),
		doctorKVMCheck(),
		doctorExecutableCheck("binary", "firecracker.binary", cfg.Firecracker.Binary),
		doctorJailerCheck(cfg.Firecracker.Jailer),
		doctorFileCheck("kernel", "firecracker.kernel", cfg.Firecracker.Kernel),
		doctorFileCheck("rootfs", "firecracker.rootfs", cfg.Firecracker.RootFS),
		doctorNetworkCheck(cfg),
	}
	return DoctorResult{
		Provider: providerName,
		Status:   aggregateDoctorStatus(checks),
		Message:  summarizeDoctorChecks(checks),
		Checks:   checks,
	}, nil
}

func doctorHostCheck() DoctorCheck {
	details := map[string]string{
		"os":       firecrackerHostGOOS,
		"mutation": "false",
	}
	if firecrackerHostGOOS != "linux" {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   "host",
			Message: fmt.Sprintf("host=%s requires a Linux KVM host", firecrackerHostGOOS),
			Details: details,
		}
	}
	return DoctorCheck{
		Status:  "ok",
		Check:   "host",
		Message: "host=linux mutation=false",
		Details: details,
	}
}

func doctorKVMCheck() DoctorCheck {
	details := map[string]string{
		"path":     "/dev/kvm",
		"mutation": "false",
	}
	if firecrackerHostGOOS != "linux" {
		details["reason"] = "unsupported_host"
		return DoctorCheck{
			Status:  "skip",
			Check:   "kvm",
			Message: "/dev/kvm check skipped on non-Linux host",
			Details: details,
		}
	}
	info, err := firecrackerStat("/dev/kvm")
	if err != nil {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   "kvm",
			Message: fmt.Sprintf("/dev/kvm unavailable: %v", err),
			Details: details,
		}
	}
	if info.IsDir() {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   "kvm",
			Message: "/dev/kvm must be a device file, not a directory",
			Details: details,
		}
	}
	return DoctorCheck{
		Status:  "ok",
		Check:   "kvm",
		Message: "kvm=/dev/kvm mutation=false",
		Details: details,
	}
}

func doctorExecutableCheck(check, field, configured string) DoctorCheck {
	value := strings.TrimSpace(configured)
	details := map[string]string{
		"configured": value,
		"field":      field,
		"mutation":   "false",
	}
	if value == "" {
		details["class"] = "configuration_incomplete"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s is required", field),
			Details: details,
		}
	}
	resolved, err := firecrackerLookPath(value)
	if err != nil {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s unavailable: %v", field, err),
			Details: details,
		}
	}
	details["path"] = resolved
	return DoctorCheck{
		Status:  "ok",
		Check:   check,
		Message: fmt.Sprintf("%s=%s mutation=false", check, resolved),
		Details: details,
	}
}

func doctorJailerCheck(configured string) DoctorCheck {
	value := strings.TrimSpace(configured)
	details := map[string]string{
		"configured": value,
		"mutation":   "false",
	}
	if value == "" {
		return DoctorCheck{
			Status:  "skip",
			Check:   "jailer",
			Message: "jailer=disabled",
			Details: details,
		}
	}
	check := doctorExecutableCheck("jailer", "firecracker.jailer", value)
	if check.Status == "ok" {
		check.Message = fmt.Sprintf("jailer=%s mutation=false", check.Details["path"])
	}
	return check
}

func doctorFileCheck(check, field, configured string) DoctorCheck {
	value := strings.TrimSpace(configured)
	details := map[string]string{
		"path":     value,
		"field":    field,
		"mutation": "false",
	}
	if value == "" {
		details["class"] = "configuration_incomplete"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s is required", field),
			Details: details,
		}
	}
	info, err := firecrackerStat(value)
	if err != nil {
		details["class"] = "environment_blocked"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s unavailable: %v", field, err),
			Details: details,
		}
	}
	if info.IsDir() {
		details["class"] = "configuration_incomplete"
		return DoctorCheck{
			Status:  "failed",
			Check:   check,
			Message: fmt.Sprintf("%s must point to a file, got directory %s", field, value),
			Details: details,
		}
	}
	return DoctorCheck{
		Status:  "ok",
		Check:   check,
		Message: fmt.Sprintf("%s=%s mutation=false", check, value),
		Details: details,
	}
}

func doctorNetworkCheck(cfg Config) DoctorCheck {
	mode := normalizeFirecrackerNetwork(cfg.Firecracker.Network)
	details := map[string]string{
		"mode":       mode,
		"cniNetwork": strings.TrimSpace(cfg.Firecracker.CNINetwork),
		"cniConfDir": strings.TrimSpace(cfg.Firecracker.CNIConfDir),
		"cniBinDir":  strings.TrimSpace(cfg.Firecracker.CNIBinDir),
		"mutation":   "false",
	}
	if mode != firecrackerNetworkCNI {
		details["class"] = "configuration_incomplete"
		return DoctorCheck{
			Status:  "failed",
			Check:   "network",
			Message: fmt.Sprintf("firecracker.network=%s is unsupported; only %s is supported", blankIfEmpty(mode), firecrackerNetworkCNI),
			Details: details,
		}
	}
	problems := make([]string, 0, 3)
	class := "configuration_incomplete"
	if details["cniNetwork"] == "" {
		problems = append(problems, "firecracker.cniNetwork is required")
	}
	if err := doctorRequireDir(details["cniConfDir"]); err != nil {
		class = "environment_blocked"
		problems = append(problems, fmt.Sprintf("firecracker.cniConfDir %v", err))
	}
	if err := doctorRequireDir(details["cniBinDir"]); err != nil {
		class = "environment_blocked"
		problems = append(problems, fmt.Sprintf("firecracker.cniBinDir %v", err))
	}
	if len(problems) > 0 {
		details["class"] = class
		return DoctorCheck{
			Status:  "failed",
			Check:   "network",
			Message: fmt.Sprintf("network=%s %s", mode, strings.Join(problems, "; ")),
			Details: details,
		}
	}
	return DoctorCheck{
		Status:  "ok",
		Check:   "network",
		Message: fmt.Sprintf("network=%s cni_network=%s mutation=false", mode, details["cniNetwork"]),
		Details: details,
	}
}

func doctorRequireDir(path string) error {
	value := strings.TrimSpace(path)
	if value == "" {
		return fmt.Errorf("is required")
	}
	info, err := firecrackerStat(value)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("must be a directory")
	}
	return nil
}

func aggregateDoctorStatus(checks []DoctorCheck) string {
	for _, check := range checks {
		if strings.EqualFold(strings.TrimSpace(check.Status), "failed") || strings.EqualFold(strings.TrimSpace(check.Status), "missing") {
			return "failed"
		}
	}
	for _, check := range checks {
		if strings.EqualFold(strings.TrimSpace(check.Status), "warning") {
			return "warning"
		}
	}
	return "ok"
}

func summarizeDoctorChecks(checks []DoctorCheck) string {
	fields := make([]string, 0, len(checks)+1)
	for _, check := range checks {
		fields = append(fields, fmt.Sprintf("%s=%s", check.Check, strings.TrimSpace(check.Status)))
	}
	fields = append(fields, "mutation=false")
	return strings.Join(fields, " ")
}

func blankIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "<empty>"
	}
	return value
}
