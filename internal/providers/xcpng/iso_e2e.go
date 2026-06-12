package xcpng

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type ISOE2EOptions struct {
	Config      Config
	Mode        string
	OS          string
	ISO         string
	AnswerISO   string
	NamePrefix  string
	Timeout     time.Duration
	EvidenceDir string
	MutateGate  bool
}

type ISOE2ESummary struct {
	Classification string            `json:"classification"`
	Mutation       bool              `json:"mutation"`
	OS             string            `json:"os"`
	ISO            string            `json:"iso"`
	AnswerISO      string            `json:"answer_iso,omitempty"`
	Phase          string            `json:"phase"`
	VMUUID         string            `json:"vm_uuid,omitempty"`
	VMRef          string            `json:"vm_ref,omitempty"`
	Cleanup        string            `json:"cleanup"`
	Reason         string            `json:"reason,omitempty"`
	Evidence       map[string]string `json:"evidence"`
	Details        map[string]string `json:"details,omitempty"`
}

type isoE2ERuntime struct {
	client            lifecycleClient
	placement         xcpNgPlacement
	leaseID           string
	labels            map[string]string
	vm                xcpNgFreshVMResult
	importedInstaller xcpNgConfigDrive
	importedAnswer    xcpNgConfigDrive
	installerDrive    xcpNgConfigDrive
	answerDrive       xcpNgConfigDrive
	installDisk       xcpNgConfigDrive
	remasteredISO     string
	generatedSeed     string
	generatedAnswer   string
	linuxSeedPayload  xcpNgCloudInitPayload
	keyPath           string
	ownsKey           bool
	publicKey         string
	windowsUser       string
	sshTarget         core.SSHTarget
	cleanupLocal      []string
	keepLocal         map[string]struct{}
	windowsFallback   bool
	windowsVTPM       bool
}

const (
	isoE2EDefaultTimeout        = 90 * time.Minute
	isoE2EInstallerTimeout      = 55 * time.Minute
	isoE2EWindowsInstallTimeout = 70 * time.Minute
	isoE2EGuestMetricsTimeout   = 20 * time.Minute
	isoE2ECleanupTimeout        = 11 * time.Minute
	isoE2ELinuxInstallDiskBytes = 24 * 1024 * 1024 * 1024
	isoE2EWindowsDiskBytes      = 64 * 1024 * 1024 * 1024
)

var (
	isoE2ECurrentTime     = func() time.Time { return time.Now().UTC() }
	isoE2EWaitForSSHReady = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
		return core.WaitForSSHReady(ctx, target, os.Stderr, phase, timeout)
	}
	isoE2ERunSSHQuiet = func(ctx context.Context, target core.SSHTarget, remote string) error {
		return core.RunSSHQuiet(ctx, target, remote)
	}
	isoE2EEnsureTestboxKey = func(cfg Config, leaseID string) (string, string, error) {
		return core.EnsureTestboxKeyForConfig(cfg, leaseID)
	}
	isoE2EStoredTestboxKeyExists  = storedISOE2ETestboxKeyExists
	isoE2ENewLeaseID              = core.NewLeaseID
	isoE2EProviderKeyForLease     = func(leaseID string) string { return core.ProviderKeyForLease(leaseID) }
	isoE2ERemasterUbuntuISO       = remasterUbuntuAutoinstallISO
	isoE2EWriteLinuxSeedISO       = writeLinuxSeedISO
	isoE2EWriteWindowsAnswerISO   = writeWindowsAnswerISO
	isoE2EGenerateWindowsPassword = generateWindowsAutoLogonPassword
	isoE2ERemoveLocalArtifact     = os.Remove
	isoE2ERemoveStoredTestboxKey  = removeISOE2EStoredTestboxKey
	isoE2EUbuntuLinuxLinePattern  = regexp.MustCompile(`(?m)^(\s*linux\s+/casper/vmlinuz\s+)(.*?)(\s+---\s*)$`)
	isoE2EWindowsARMLinePattern   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(arm64|aarch64|arm)(?:[^a-z0-9]|$)`)
	isoE2EWindows11LinePattern    = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:win(?:dows)?[\s._-]*11|w11)(?:[^a-z0-9]|$)`)
)

func isoE2ELeaseID() string {
	if override := strings.TrimSpace(os.Getenv("CRABBOX_XCP_NG_ISO_E2E_LEASE_ID")); override != "" {
		return override
	}
	return "cbx_isoe2e_" + strings.TrimPrefix(isoE2ENewLeaseID(), "cbx_")
}

func isoE2EAllowImportedInstaller() bool {
	return os.Getenv("CRABBOX_XCP_NG_ISO_E2E_ALLOW_IMPORTED_INSTALLER") == "1"
}

func isoE2EForceProvidedWindowsAnswerSSH() bool {
	return os.Getenv("CRABBOX_XCP_NG_ISO_E2E_FORCE_WINDOWS_ANSWER_SSH") == "1"
}

func isoE2EKeepGeneratedWindowsAnswer() bool {
	return os.Getenv("CRABBOX_XCP_NG_ISO_E2E_KEEP_WINDOWS_ANSWER") == "1"
}

func RunISOE2E(ctx context.Context, opts ISOE2EOptions) (summary ISOE2ESummary, err error) {
	summary = ISOE2ESummary{
		Mutation:  opts.Mode == "mutate",
		OS:        opts.OS,
		ISO:       opts.ISO,
		AnswerISO: opts.AnswerISO,
		Phase:     "init",
		Cleanup:   "not_started",
		Evidence:  map[string]string{},
		Details:   map[string]string{},
	}
	if err = validateISOE2EOptions(&opts); err != nil {
		summary.Classification = "test_failed"
		summary.Reason = err.Error()
		return summary, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = isoE2EDefaultTimeout
	}
	if opts.EvidenceDir == "" {
		opts.EvidenceDir = ".crabbox/xcpng-iso-e2e"
	}
	if err = os.MkdirAll(opts.EvidenceDir, 0o700); err != nil {
		summary.Classification = "test_failed"
		summary.Reason = fmt.Sprintf("create evidence dir: %v", err)
		return summary, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	client, err := newLifecycleClient(ctx, opts.Config)
	if err != nil {
		summary.Classification = "environment_blocked"
		summary.Reason = err.Error()
		summary.Phase = "connect"
		return summary, err
	}
	defer func() {
		closeCtx, closeCancel := xcpNgRollbackContext(ctx)
		defer closeCancel()
		if closeErr := client.Close(closeCtx); closeErr != nil {
			hadPrimaryError := err != nil
			closeErr = fmt.Errorf("close xcp-ng session: %w", closeErr)
			err = errors.Join(err, closeErr)
			summary.Cleanup = "failed"
			if summary.Reason == "" {
				summary.Reason = closeErr.Error()
			} else {
				summary.Reason = fmt.Sprintf("%s; %v", summary.Reason, closeErr)
			}
			if !hadPrimaryError {
				summary.Classification = "resource_cleanup_failed"
				summary.Phase = "close"
			}
		}
	}()

	placement, err := resolveISOE2EPlacement(ctx, client, opts.Config)
	if err != nil {
		summary.Classification = "environment_blocked"
		summary.Reason = err.Error()
		summary.Phase = "placement"
		return summary, err
	}
	if opts.Mode == "read-only" {
		if err := resolveISOE2EReadOnly(ctx, client, placement, opts, &summary); err != nil {
			return summary, err
		}
		summary.Classification = "read_only_passed"
		summary.Phase = "read_only_validation"
		summary.Cleanup = "not_needed"
		return summary, nil
	}
	if !opts.MutateGate {
		summary.Classification = "environment_blocked"
		summary.Reason = "mutation_gate_missing"
		summary.Phase = "gate"
		return summary, core.Exit(3, "CRABBOX_XCP_NG_ISO_E2E_MUTATE=1 is required for --mutate")
	}
	if placement.networkRef == "" {
		summary.Classification = "environment_blocked"
		summary.Reason = "xcp-ng ISO mutation requires xcpNg.network or xcpNg.networkUuid so the fresh VM has a VIF"
		summary.Phase = "placement"
		return summary, core.Exit(3, "%s", summary.Reason)
	}
	if opts.OS == "windows" {
		return runISOE2EWindows(ctx, client, placement, opts, summary)
	}
	return runISOE2ELinux(ctx, client, placement, opts, summary)
}

func resolveISOE2EReadOnly(ctx context.Context, client lifecycleClient, placement xcpNgPlacement, opts ISOE2EOptions, summary *ISOE2ESummary) error {
	_ = placement
	installer, err := client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), opts.ISO)
	if err != nil {
		summary.Classification = "environment_blocked"
		summary.Reason = err.Error()
		summary.Phase = "installer_iso"
		return err
	}
	if opts.OS == "windows" {
		if err := ensureWindowsInstallerSupported(opts.ISO, installer.NameLabel, summary); err != nil {
			return err
		}
	}
	summary.Details["installer_source"] = installer.Source
	if installer.UUID != "" {
		summary.Details["installer_uuid"] = installer.UUID
	}
	if opts.AnswerISO == "" {
		return nil
	}
	answerISO, err := client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), opts.AnswerISO)
	if err != nil {
		summary.Classification = "environment_blocked"
		summary.Reason = err.Error()
		summary.Phase = "answer_iso"
		return err
	}
	summary.Details["answer_iso_source"] = answerISO.Source
	if answerISO.UUID != "" {
		summary.Details["answer_iso_uuid"] = answerISO.UUID
	}
	return nil
}

func runISOE2EWindows(ctx context.Context, client lifecycleClient, placement xcpNgPlacement, opts ISOE2EOptions, summary ISOE2ESummary) (result ISOE2ESummary, err error) {
	result = summary
	now := isoE2ECurrentTime()
	leaseID := isoE2ELeaseID()
	labelCfg := opts.Config
	labelCfg.TargetOS = core.TargetWindows
	labelCfg.WindowsMode = core.WindowsModeNormal
	workRoot := strings.TrimSpace(firstNonBlank(labelCfg.XCPNg.WorkRoot, labelCfg.WorkRoot))
	if workRoot == "" || strings.HasPrefix(workRoot, "/") {
		workRoot = `C:\crabbox`
	}
	labelCfg.WorkRoot = workRoot
	labels := core.DirectLeaseLabels(labelCfg, leaseID, strings.TrimPrefix(opts.NamePrefix, "crabbox-"), "xcp-ng", "", true, now)
	labels["work_root"] = workRoot
	installerIdentity, err := client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), opts.ISO)
	if err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "installer_iso"
		result.Reason = err.Error()
		return result, err
	}
	if err = ensureWindowsInstallerSupported(opts.ISO, installerIdentity.NameLabel, &result); err != nil {
		return result, err
	}
	runtime := &isoE2ERuntime{
		client:       client,
		placement:    placement,
		leaseID:      leaseID,
		labels:       labels,
		cleanupLocal: []string{},
		keepLocal:    map[string]struct{}{},
		windowsVTPM:  windowsInstallerRequiresVTPM(opts.ISO, installerIdentity.NameLabel),
	}
	runtime.labels["workflow"] = "iso-e2e"
	runtime.labels["os"] = opts.OS
	runtime.labels["name_prefix"] = opts.NamePrefix
	runtime.labels["provider_key"] = isoE2EProviderKeyForLease(runtime.leaseID)
	defer finalizeLocalArtifacts(runtime, &result, &err)
	defer runtime.cleanup(context.Background(), &result, &err)
	if err = runtime.prepareWindowsAnswerMedia(ctx, opts, &result); err != nil {
		return result, err
	}
	if err = runtime.createBaseVM(ctx, opts, &result); err != nil {
		return result, err
	}
	if err = runtime.attachInstallDisk(ctx); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "windows_install_disk"
		result.Reason = err.Error()
		return result, err
	}
	installer, resolveErr := runtime.resolveInstallerMedia(ctx, &result)
	if resolveErr != nil {
		return result, resolveErr
	}
	if err = ensureWindowsInstallerSupported(opts.ISO, installer.NameLabel, &result); err != nil {
		return result, err
	}
	if err = runtime.attachInstallerISO(ctx, installer, "3"); err != nil {
		if result.Classification == "" {
			result.Classification = "environment_blocked"
			result.Reason = err.Error()
		}
		return result, err
	}
	if err = runtime.attachAnswerMedia(ctx, opts, &result); err != nil {
		return result, err
	}
	result.Phase = "windows_iso_attached"
	if err = runtime.startVM(ctx); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "windows_setup_started"
		result.Reason = err.Error()
		return result, err
	}
	result.Phase = "windows_setup_started"
	result.Details["installer_boot"] = "started"
	if err = runtime.client.SetVMBootOrder(ctx, xapiRef(runtime.vm.VM.Ref), "c"); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "windows_install_complete"
		result.Reason = fmt.Sprintf("set installed-boot order: %v", err)
		return result, err
	}
	installerDeadline := isoE2EWindowsInstallTimeout
	if opts.Timeout > isoE2EGuestMetricsTimeout {
		remaining := opts.Timeout - isoE2EGuestMetricsTimeout
		if remaining > 0 && remaining < installerDeadline {
			installerDeadline = remaining
		}
	}
	installerCtx, installerCancel := context.WithTimeout(ctx, installerDeadline)
	defer installerCancel()
	firstBootIP, ipErr := runtime.waitForGuestIPv4(installerCtx, "Windows", !runtime.windowsFallback)
	if ipErr != nil {
		result.Classification = "environment_blocked"
		result.Phase = "windows_install_complete"
		result.Reason = ipErr.Error()
		return result, ipErr
	}
	if runtime.windowsFallback {
		result.Phase = "windows_install_complete"
		result.Details["first_boot_ip"] = firstBootIP
		result.Classification = "source_uncovered"
		result.Phase = "windows_first_boot"
		result.Details["readiness_probe"] = "guest-metrics"
		result.Reason = "Windows guest reached first boot and reported guest metrics, but the supplied answer media does not guarantee Crabbox SSH bootstrap; remote command proof remains uncovered"
		return result, core.Exit(4, "%s", result.Reason)
	}
	runtime.sshTarget = core.SSHTargetFromConfig(opts.Config, firstBootIP)
	runtime.sshTarget.TargetOS = "windows"
	runtime.sshTarget.WindowsMode = "normal"
	runtime.sshTarget.Key = runtime.keyPath
	runtime.sshTarget.User = firstNonBlank(runtime.windowsUser, runtime.sshTarget.User, opts.Config.XCPNg.User, opts.Config.SSHUser)
	if runtime.sshTarget.Port == "" {
		runtime.sshTarget.Port = firstNonBlank(opts.Config.SSHPort, "22")
	}
	if err = isoE2EWaitForSSHReady(ctx, &runtime.sshTarget, "windows_first_boot", opts.Timeout); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "windows_first_boot"
		result.Reason = err.Error()
		return result, err
	}
	result.Phase = "windows_install_complete"
	result.Details["first_boot_ip"] = firstBootIP
	result.Phase = "windows_first_boot"
	if err = isoE2ERunSSHQuiet(ctx, runtime.sshTarget, `Write-Output windows-iso-e2e-ok`); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "windows_command_ok"
		result.Reason = err.Error()
		return result, err
	}
	result.Classification = "windows_install_passed"
	result.Phase = "windows_command_ok"
	result.Reason = ""
	return result, nil
}

func runISOE2ELinux(ctx context.Context, client lifecycleClient, placement xcpNgPlacement, opts ISOE2EOptions, summary ISOE2ESummary) (result ISOE2ESummary, err error) {
	result = summary
	now := isoE2ECurrentTime()
	leaseID := isoE2ELeaseID()
	guestCfg := opts.Config
	guestCfg.TargetOS = core.TargetLinux
	guestCfg.WindowsMode = ""
	workRoot := strings.TrimSpace(firstNonBlank(guestCfg.XCPNg.WorkRoot, guestCfg.WorkRoot))
	if workRoot == "" || !strings.HasPrefix(workRoot, "/") {
		workRoot = "/work/crabbox"
	}
	guestCfg.WorkRoot = workRoot
	guestCfg.XCPNg.WorkRoot = workRoot
	opts.Config = guestCfg
	labels := core.DirectLeaseLabels(guestCfg, leaseID, strings.TrimPrefix(opts.NamePrefix, "crabbox-"), "xcp-ng", "", true, now)
	labels["work_root"] = workRoot
	runtime := &isoE2ERuntime{
		client:       client,
		placement:    placement,
		leaseID:      leaseID,
		labels:       labels,
		cleanupLocal: []string{},
		keepLocal:    map[string]struct{}{},
	}
	runtime.labels["workflow"] = "iso-e2e"
	runtime.labels["os"] = opts.OS
	runtime.labels["name_prefix"] = opts.NamePrefix
	runtime.labels["provider_key"] = isoE2EProviderKeyForLease(runtime.leaseID)
	defer finalizeLocalArtifacts(runtime, &result, &err)
	defer runtime.cleanup(context.Background(), &result, &err)
	keyExisted := isoE2EStoredTestboxKeyExists(runtime.leaseID)
	keyPath, publicKey, err := isoE2EEnsureTestboxKey(opts.Config, runtime.leaseID)
	if err != nil {
		result.Classification = "test_failed"
		result.Phase = "linux_seed_generation"
		result.Reason = err.Error()
		return result, err
	}
	runtime.keyPath = keyPath
	runtime.ownsKey = !keyExisted
	runtime.publicKey = publicKey
	result.Details["ssh_key"] = filepath.Base(keyPath)
	if err = runtime.prepareInstallerMedia(ctx, opts, &result); err != nil {
		return result, err
	}
	if err = runtime.createBaseVM(ctx, opts, &result); err != nil {
		return result, err
	}
	if err = runtime.attachInstallDisk(ctx); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "linux_install_disk"
		result.Reason = err.Error()
		return result, err
	}
	installer, resolveErr := runtime.resolveInstallerMedia(ctx, &result)
	if resolveErr != nil {
		return result, resolveErr
	}
	if err = runtime.attachInstallerISO(ctx, installer, "3"); err != nil {
		if result.Classification == "" {
			result.Classification = "environment_blocked"
			result.Reason = err.Error()
		}
		return result, err
	}
	if err = runtime.attachAnswerMedia(ctx, opts, &result); err != nil {
		return result, err
	}
	result.Phase = "linux_iso_attached"
	if err = runtime.startVM(ctx); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "boot"
		result.Reason = err.Error()
		return result, err
	}
	result.Phase = "linux_installer_booted"
	result.Details["installer_boot"] = "started"
	if err = runtime.client.SetVMBootOrder(ctx, xapiRef(runtime.vm.VM.Ref), "c"); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "linux_install_complete"
		result.Reason = fmt.Sprintf("set installed-boot order: %v", err)
		return result, err
	}
	installerDeadline := isoE2EInstallerTimeout
	if opts.Timeout > isoE2EGuestMetricsTimeout {
		remaining := opts.Timeout - isoE2EGuestMetricsTimeout
		if remaining > 0 && remaining < installerDeadline {
			installerDeadline = remaining
		}
	}
	installerCtx, installerCancel := context.WithTimeout(ctx, installerDeadline)
	defer installerCancel()
	firstBootIP, ipErr := runtime.waitForGuestIPv4(installerCtx, "Linux", false)
	if ipErr != nil {
		result.Classification = "environment_blocked"
		result.Phase = "linux_install_complete"
		result.Reason = ipErr.Error()
		return result, ipErr
	}
	result.Phase = "linux_install_complete"
	result.Details["first_boot_ip"] = firstBootIP
	runtime.sshTarget = core.SSHTargetFromConfig(opts.Config, firstBootIP)
	runtime.sshTarget.Key = runtime.keyPath
	if runtime.sshTarget.User == "" {
		runtime.sshTarget.User = firstNonBlank(opts.Config.XCPNg.User, opts.Config.SSHUser)
	}
	if runtime.sshTarget.Port == "" {
		runtime.sshTarget.Port = firstNonBlank(opts.Config.SSHPort, "22")
	}
	if err = isoE2EWaitForSSHReady(ctx, &runtime.sshTarget, "linux_first_boot", minDuration(isoE2EGuestMetricsTimeout, opts.Timeout/3)); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "linux_first_boot"
		result.Reason = err.Error()
		return result, err
	}
	result.Phase = "linux_first_boot"
	sshProof := "printf linux-iso-e2e-ok"
	if err = isoE2ERunSSHQuiet(ctx, runtime.sshTarget, sshProof); err != nil {
		result.Classification = "environment_blocked"
		result.Phase = "linux_ssh_ok"
		result.Reason = err.Error()
		return result, err
	}
	result.Phase = "linux_ssh_ok"
	result.Classification = "linux_install_passed"
	result.Reason = ""
	return result, nil
}

func (r *isoE2ERuntime) prepareInstallerMedia(ctx context.Context, opts ISOE2EOptions, summary *ISOE2ESummary) error {
	installerPath := strings.TrimSpace(opts.ISO)
	if installerPath == "" {
		summary.Classification = "test_failed"
		summary.Phase = "installer_iso"
		summary.Reason = "installer ISO is required"
		return exit(2, "%s", summary.Reason)
	}
	importedInstallerRef := strings.HasPrefix(installerPath, "OpaqueRef:") || looksLikeUUID(installerPath)
	if !fileExists(installerPath) && !(isoE2EAllowImportedInstaller() && importedInstallerRef) {
		summary.Classification = "environment_blocked"
		summary.Phase = "installer_iso"
		summary.Reason = "linux mutating path requires a local Ubuntu Server ISO so the harness can remaster the autoinstall boot arg"
		return exit(3, "%s", summary.Reason)
	}
	if fileExists(installerPath) {
		remasteredISO, err := isoE2ERemasterUbuntuISO(ctx, installerPath, opts.EvidenceDir)
		if err != nil {
			summary.Classification = "environment_blocked"
			summary.Phase = "linux_installer_booted"
			summary.Reason = fmt.Sprintf("linux_autoinstall_boot_arg_unavailable: %v", err)
			return err
		}
		r.remasteredISO = remasteredISO
		r.keepLocal[remasteredISO] = struct{}{}
		summary.Evidence["installer_iso_remastered"] = remasteredISO
		opts.ISO = remasteredISO
		summary.ISO = remasteredISO
	} else if importedInstallerRef {
		summary.Details["installer_source"] = "sr-vdi"
		summary.ISO = installerPath
	}
	payload, err := newLinuxAutoinstallPayload(opts.Config, r.leaseID, strings.TrimPrefix(r.leaseID, "cbx_"), r.publicKey)
	if err != nil {
		summary.Classification = "test_failed"
		summary.Phase = "linux_seed_generation"
		summary.Reason = err.Error()
		return err
	}
	r.linuxSeedPayload = xcpNgCloudInitPayload{UserData: payload.UserData, MetaData: payload.MetaData}
	seedISO, err := isoE2EWriteLinuxSeedISO(ctx, opts.EvidenceDir, payload)
	if err != nil {
		summary.Classification = "test_failed"
		summary.Phase = "linux_seed_generation"
		summary.Reason = err.Error()
		return err
	}
	r.generatedSeed = seedISO
	r.keepLocal[seedISO] = struct{}{}
	if opts.AnswerISO == "" {
		opts.AnswerISO = seedISO
		summary.AnswerISO = seedISO
	}
	summary.Phase = "linux_seed_generation"
	summary.Evidence["linux_seed_iso"] = seedISO
	return nil
}

func (r *isoE2ERuntime) prepareWindowsAnswerMedia(ctx context.Context, opts ISOE2EOptions, summary *ISOE2ESummary) error {
	if strings.TrimSpace(summary.AnswerISO) != "" {
		if !isoE2EForceProvidedWindowsAnswerSSH() {
			r.windowsFallback = true
			return nil
		}
		keyExisted := isoE2EStoredTestboxKeyExists(r.leaseID)
		keyPath, publicKey, err := isoE2EEnsureTestboxKey(opts.Config, r.leaseID)
		if err != nil {
			summary.Classification = "test_failed"
			summary.Phase = "windows_answer_generation"
			summary.Reason = err.Error()
			return err
		}
		r.keyPath = keyPath
		r.ownsKey = !keyExisted
		r.publicKey = publicKey
		rawUser := strings.TrimSpace(core.Blank(opts.Config.XCPNg.User, opts.Config.SSHUser))
		user := windowsAccountName(rawUser)
		if user == "" {
			summary.Classification = "test_failed"
			summary.Phase = "windows_answer_generation"
			summary.Reason = "xcp-ng windows autounattend user is required"
			return exit(2, "%s", summary.Reason)
		}
		r.windowsUser = user
		summary.Details["answer_iso_source"] = "provided-media"
		summary.Details["windows_bootstrap"] = "openssh-key"
		summary.Details["ssh_key"] = filepath.Base(keyPath)
		summary.Phase = "windows_answer_generation"
		return nil
	}
	keyExisted := isoE2EStoredTestboxKeyExists(r.leaseID)
	keyPath, publicKey, err := isoE2EEnsureTestboxKey(opts.Config, r.leaseID)
	if err != nil {
		summary.Classification = "test_failed"
		summary.Phase = "windows_answer_generation"
		summary.Reason = err.Error()
		return err
	}
	r.keyPath = keyPath
	r.ownsKey = !keyExisted
	r.publicKey = publicKey
	initialPassword, err := isoE2EGenerateWindowsPassword()
	if err != nil {
		summary.Classification = "test_failed"
		summary.Phase = "windows_answer_generation"
		summary.Reason = fmt.Sprintf("generate Windows autounattend password: %v", err)
		return err
	}
	payload, err := newWindowsAutounattendPayload(opts.Config, r.leaseID, strings.TrimPrefix(r.leaseID, "cbx_"), publicKey, initialPassword)
	if err != nil {
		summary.Classification = "test_failed"
		summary.Phase = "windows_answer_generation"
		summary.Reason = err.Error()
		return err
	}
	answerISO, err := isoE2EWriteWindowsAnswerISO(ctx, opts.EvidenceDir, payload)
	if err != nil {
		summary.Classification = "test_failed"
		summary.Phase = "windows_answer_generation"
		summary.Reason = err.Error()
		return err
	}
	r.windowsUser = payload.Username
	r.generatedAnswer = answerISO
	r.cleanupLocal = append(r.cleanupLocal, answerISO)
	answerDir := filepath.Dir(answerISO)
	if strings.HasPrefix(filepath.Base(answerDir), "windows-answer-media-") {
		r.cleanupLocal = append(r.cleanupLocal, answerDir)
	}
	if isoE2EKeepGeneratedWindowsAnswer() {
		for _, path := range r.cleanupLocal {
			r.keepLocal[path] = struct{}{}
		}
		summary.Evidence["windows_answer_iso"] = answerISO
	}
	summary.AnswerISO = answerISO
	summary.Details["answer_iso_source"] = "generated-local-file"
	summary.Details["windows_bootstrap"] = "openssh-key"
	summary.Details["ssh_key"] = filepath.Base(keyPath)
	summary.Phase = "windows_answer_generation"
	return nil
}

func (r *isoE2ERuntime) createBaseVM(ctx context.Context, opts ISOE2EOptions, summary *ISOE2ESummary) error {
	vmName := isoE2EVMName(opts)
	created, err := r.client.CreateFreshVM(ctx, xcpNgFreshVMRequest{
		Name:        vmName,
		Description: "Crabbox XCP-ng ISO E2E harness",
		HostRef:     r.placement.hostRef,
		Network: func() *xcpNgVIFSpec {
			if r.placement.networkRef == "" {
				return nil
			}
			return &xcpNgVIFSpec{Device: "0", NetworkRef: r.placement.networkRef, MTU: 1500, Labels: r.labels}
		}(),
		Labels:     r.labels,
		SecureBoot: strings.EqualFold(opts.OS, "windows"),
		VTPM:       strings.EqualFold(opts.OS, "windows") && r.windowsVTPM,
		HVMBoot: func() map[string]string {
			boot := map[string]string{"order": "dc"}
			if strings.EqualFold(opts.OS, "windows") {
				boot["firmware"] = "uefi"
			}
			return boot
		}(),
	})
	if created.VM.Ref != "" {
		r.vm = created
		summary.VMUUID = created.VM.UUID
		summary.VMRef = created.VM.Ref
		summary.Cleanup = "pending"
	}
	if err != nil {
		summary.Classification = "environment_blocked"
		summary.Phase = "create_vm"
		summary.Reason = err.Error()
		return err
	}
	return nil
}

func (r *isoE2ERuntime) resolveInstallerMedia(ctx context.Context, summary *ISOE2ESummary) (xcpNgISOMediaRef, error) {
	if r.installerDrive.VDIRef != "" {
		return xcpNgISOMediaRef{VDIRef: r.installerDrive.VDIRef, NameLabel: r.installerDrive.Name, Source: "imported-local-file"}, nil
	}
	installer, err := r.client.ResolveISOMedia(ctx, xcpNgConfig{}, summary.ISO)
	if err == nil {
		summary.Details["installer_source"] = installer.Source
		if installer.UUID != "" {
			summary.Details["installer_uuid"] = installer.UUID
		}
		if installer.Source != "local-file" {
			return installer, nil
		}
	}
	if summary.ISO == "" || !fileExists(summary.ISO) {
		if err == nil {
			err = exit(4, "xcp-ng installer ISO not found: %s", summary.ISO)
		}
		summary.Classification = "environment_blocked"
		summary.Phase = "installer_iso"
		summary.Reason = err.Error()
		return xcpNgISOMediaRef{}, err
	}
	imported, importErr := r.client.ImportISO(ctx, xcpNgImportISORequest{
		SRRef:       r.placement.srRef,
		Path:        summary.ISO,
		Name:        filepath.Base(summary.ISO),
		Description: fmt.Sprintf("Crabbox imported %s installer ISO", isoE2EOSLabel(summary.OS)),
		Labels:      r.labels,
		DestroyVDI:  true,
	})
	if imported.VDIRef != "" {
		r.importedInstaller = imported
	}
	if importErr != nil {
		summary.Classification = "environment_blocked"
		summary.Phase = "installer_iso"
		summary.Reason = importErr.Error()
		return xcpNgISOMediaRef{}, importErr
	}
	summary.Evidence["installer_iso_imported"] = imported.Name
	return xcpNgISOMediaRef{VDIRef: imported.VDIRef, NameLabel: imported.Name, Source: "imported-local-file"}, nil
}

func (r *isoE2ERuntime) attachInstallDisk(ctx context.Context) error {
	disk, err := r.client.AttachDisk(ctx, xcpNgDiskAttachRequest{
		VMRef:       xapiRef(r.vm.VM.Ref),
		SRRef:       r.placement.srRef,
		Name:        r.vm.VM.Name + "-install-disk",
		Description: fmt.Sprintf("Crabbox %s ISO install disk", isoE2EOSLabel(r.labels["os"])),
		SizeBytes:   isoE2EInstallDiskSize(r.labels["os"]),
		UserDevice:  "0",
		Labels:      r.labels,
		Unpluggable: true,
		DestroyVDI:  true,
	})
	if disk.VDIRef != "" || disk.VBDRef != "" {
		r.installDisk = disk
	}
	if err != nil {
		return err
	}
	return nil
}

func (r *isoE2ERuntime) attachInstallerISO(ctx context.Context, installer xcpNgISOMediaRef, userDevice string) error {
	drive, err := r.client.AttachISO(ctx, xcpNgISOAttachRequest{VMRef: xapiRef(r.vm.VM.Ref), ISO: installer, UserDevice: userDevice, Bootable: true, Labels: r.labels, Unpluggable: true})
	if err != nil {
		if r.importedInstaller.VDIRef != "" && r.importedInstaller.DestroyVDI {
			cleanupCtx, cancel := xcpNgRollbackContext(ctx)
			deleteErr := r.client.DeleteConfigDrive(cleanupCtx, r.importedInstaller)
			cancel()
			if deleteErr == nil {
				r.importedInstaller = xcpNgConfigDrive{}
			} else {
				err = fmt.Errorf("%w; cleanup imported installer ISO: %v", err, deleteErr)
			}
		}
		return err
	}
	if r.importedInstaller.DestroyVDI {
		drive.DestroyVDI = true
	}
	r.installerDrive = drive
	return nil
}

func (r *isoE2ERuntime) attachAnswerMedia(ctx context.Context, opts ISOE2EOptions, summary *ISOE2ESummary) error {
	answerValue := strings.TrimSpace(summary.AnswerISO)
	if answerValue == "" {
		return nil
	}
	if strings.EqualFold(summary.OS, "linux") && answerValue == r.generatedSeed && r.linuxSeedPayload.UserData != "" {
		drive, err := r.client.AttachConfigDrive(ctx, xcpNgConfigDriveRequest{
			VMRef:   xapiRef(r.vm.VM.Ref),
			SRRef:   r.placement.srRef,
			LeaseID: r.leaseID,
			Slug:    "linux-autoinstall",
			Payload: r.linuxSeedPayload,
			Labels:  r.labels,
		})
		if drive.VDIRef != "" || drive.VBDRef != "" {
			r.answerDrive = drive
		}
		if err != nil {
			summary.Classification = "environment_blocked"
			summary.Phase = "answer_iso"
			summary.Reason = err.Error()
			return err
		}
		summary.Details["answer_iso_source"] = "generated-config-drive"
		return nil
	}
	answerISO, err := r.client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), answerValue)
	if err == nil && answerISO.Source != "local-file" {
		summary.Details["answer_iso_source"] = answerISO.Source
		if answerISO.UUID != "" {
			summary.Details["answer_iso_uuid"] = answerISO.UUID
		}
		return r.attachAnswerISO(ctx, answerISO, "4")
	}
	if !fileExists(answerValue) {
		if err == nil {
			err = exit(4, "xcp-ng answer ISO not found: %s", answerValue)
		}
		summary.Classification = "environment_blocked"
		summary.Phase = "answer_iso"
		summary.Reason = err.Error()
		return err
	}
	imported, importErr := r.client.ImportISO(ctx, xcpNgImportISORequest{
		SRRef:       r.placement.srRef,
		Path:        answerValue,
		Name:        filepath.Base(answerValue),
		Description: fmt.Sprintf("Crabbox %s answer ISO", isoE2EOSLabel(summary.OS)),
		Labels:      r.labels,
		DestroyVDI:  true,
	})
	if imported.VDIRef != "" {
		r.importedAnswer = imported
	}
	if importErr != nil {
		summary.Classification = "environment_blocked"
		summary.Phase = "answer_iso"
		summary.Reason = importErr.Error()
		return importErr
	}
	summary.Details["answer_iso_source"] = "generated-local-file"
	return r.attachAnswerISO(ctx, xcpNgISOMediaRef{VDIRef: imported.VDIRef, NameLabel: imported.Name, Source: "generated-local-file"}, "4")
}

func (r *isoE2ERuntime) attachAnswerISO(ctx context.Context, answerISO xcpNgISOMediaRef, userDevice string) error {
	drive, err := r.client.AttachISO(ctx, xcpNgISOAttachRequest{VMRef: xapiRef(r.vm.VM.Ref), ISO: answerISO, UserDevice: userDevice, Bootable: false, Labels: r.labels, Unpluggable: true})
	if err != nil {
		if r.importedAnswer.VDIRef != "" && r.importedAnswer.DestroyVDI {
			cleanupCtx, cancel := xcpNgRollbackContext(ctx)
			deleteErr := r.client.DeleteConfigDrive(cleanupCtx, r.importedAnswer)
			cancel()
			if deleteErr == nil {
				r.importedAnswer = xcpNgConfigDrive{}
			} else {
				err = fmt.Errorf("%w; cleanup imported answer ISO: %v", err, deleteErr)
			}
		}
		return err
	}
	if r.importedAnswer.DestroyVDI {
		drive.DestroyVDI = true
	}
	r.answerDrive = drive
	return nil
}

func (r *isoE2ERuntime) startVM(ctx context.Context) error {
	return r.client.StartVM(ctx, xapiRef(r.vm.VM.Ref))
}

func (r *isoE2ERuntime) waitForGuestIPv4(ctx context.Context, installLabel string, allowDiscovery bool) (string, error) {
	waitLabel := strings.ToLower(strings.TrimSpace(installLabel))
	if waitLabel == "" {
		waitLabel = "guest"
	}
	deadline := time.Now().Add(minDuration(isoE2EGuestMetricsTimeout, time.Until(time.Now().Add(isoE2EGuestMetricsTimeout))))
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	var lastErr error
	restartCount := 0
	const maxRestarts = 3
	const restartCooldown = 20 * time.Second
	nextPowerCheck := time.Time{}
	nextDiscover := time.Time{}
	for {
		ip, err := r.client.GuestIPv4(ctx, xapiRef(r.vm.VM.Ref))
		if err == nil && ip != "" {
			return ip, nil
		}
		lastErr = err
		if allowDiscovery {
			if discoverer, ok := r.client.(guestIPv4Discoverer); ok && time.Now().After(nextDiscover) {
				nextDiscover = time.Now().Add(guestIPDiscoverInterval)
				discovered, discoverErr := discoverer.DiscoverGuestIPv4(ctx, xapiRef(r.vm.VM.Ref))
				if discoverErr == nil && discovered != "" {
					return discovered, nil
				}
				var configErr guestProbeConfigError
				if errors.As(discoverErr, &configErr) {
					return "", discoverErr
				}
				if lastErr == nil && discoverErr != nil {
					lastErr = discoverErr
				}
			}
		}

		// Halt-restart: if the VM is halted (e.g. autoinstall rebooted and shut down),
		// automatically start it again, but only up to a bounded number of attempts.
		if time.Now().After(nextPowerCheck) && restartCount < maxRestarts {
			srv, srvErr := r.client.GetServer(ctx, r.vm.VM.UUID)
			if srvErr == nil {
				status := strings.ToLower(strings.TrimSpace(srv.Status))
				switch status {
				case "running":
					// VM is running but guest metrics haven't reported an IP yet.
					// Nothing to do; keep waiting.
				case "halted":
					restartCount++
					if startErr := r.client.StartVM(ctx, xapiRef(r.vm.VM.Ref)); startErr != nil {
						lastErr = fmt.Errorf(
							"guest IPv4 unavailable and VM halted (restart %d/%d failed: %w)",
							restartCount, maxRestarts, startErr,
						)
					} else {
						// Give Xenops/XAPI time to boot the guest before we poll again.
						nextPowerCheck = time.Now().Add(restartCooldown)
					}
				default:
					// Unexpected state (paused, suspended, etc.) — surface it.
					lastErr = fmt.Errorf("guest IPv4 unavailable and VM in unexpected state %q", srv.Status)
				}
			}
			// If we couldn't reach GetServer, lastErr stays the GuestIPv4 error.
		}

		if time.Now().After(deadline) {
			if restartCount > 0 {
				return "", exit(5, "timed out waiting for XCP-ng guest IPv4 after %s install (restarted %d times): %v", waitLabel, restartCount, lastErr)
			}
			if lastErr != nil {
				return "", exit(5, "timed out waiting for XCP-ng guest IPv4 after %s install: %v", waitLabel, lastErr)
			}
			return "", exit(5, "timed out waiting for XCP-ng guest IPv4 after %s install", waitLabel)
		}
		select {
		case <-ctx.Done():
			if lastErr != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				return "", exit(5, "timed out waiting for XCP-ng guest IPv4 after %s install: %v", waitLabel, lastErr)
			}
			return "", context.Cause(ctx)
		case <-time.After(5 * time.Second):
		}
	}
}

func (r *isoE2ERuntime) cleanup(ctx context.Context, summary *ISOE2ESummary, runErr *error) {
	if r.vm.VM.Ref == "" {
		summary.Cleanup = "not_needed"
		return
	}
	if os.Getenv("CRABBOX_XCP_NG_ISO_E2E_NO_CLEANUP") == "1" {
		summary.Cleanup = "skipped"
		return
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, isoE2ECleanupTimeout)
	defer cancel()
	cleanupErr := r.client.DeleteFreshServer(cleanupCtx, r.vm.VM.Ref, r.vm.VTPMRef)
	seen := map[string]struct{}{}
	for _, drive := range []xcpNgConfigDrive{r.installDisk, r.answerDrive, r.installerDrive, r.importedAnswer, r.importedInstaller} {
		if !drive.DestroyVDI || drive.VDIRef == "" {
			continue
		}
		key := drive.VDIRef + "|" + drive.VBDRef
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if err := r.client.DeleteConfigDrive(cleanupCtx, drive); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
	}
	if cleanupErr != nil {
		summary.Cleanup = "resource_cleanup_failed"
		summary.Details["cleanup_error"] = cleanupErr.Error()
		if summary.Classification == "linux_install_passed" || summary.Classification == "started_unverified" || summary.Classification == "windows_install_passed" || summary.Classification == "source_uncovered" {
			summary.Classification = "resource_cleanup_failed"
		}
		if *runErr == nil {
			*runErr = cleanupErr
		}
		return
	}
	summary.Cleanup = "cleaned"
}

func isoE2EInstallDiskSize(osName string) int64 {
	if strings.EqualFold(osName, "windows") {
		return isoE2EWindowsDiskBytes
	}
	return isoE2ELinuxInstallDiskBytes
}

func finalizeLocalArtifacts(runtime *isoE2ERuntime, summary *ISOE2ESummary, runErr *error) {
	cleanupErr := errors.Join(runtime.cleanupLocalArtifacts(), runtime.cleanupStoredTestboxKey(summary.Cleanup))
	if cleanupErr == nil {
		return
	}
	summary.Cleanup = "failed"
	if summary.Classification == "" ||
		strings.HasSuffix(summary.Classification, "_passed") ||
		summary.Classification == "source_uncovered" {
		summary.Classification = "resource_cleanup_failed"
	}
	if summary.Reason == "" {
		summary.Reason = cleanupErr.Error()
	} else {
		summary.Reason = fmt.Sprintf("%s; %v", summary.Reason, cleanupErr)
	}
	if *runErr == nil {
		*runErr = cleanupErr
	}
}

func (r *isoE2ERuntime) cleanupStoredTestboxKey(cleanupStatus string) error {
	if r.keyPath == "" || !r.ownsKey {
		return nil
	}
	if cleanupStatus == "skipped" || cleanupStatus == "resource_cleanup_failed" {
		return nil
	}
	if r.vm.VM.Ref != "" && cleanupStatus != "cleaned" {
		return nil
	}
	if err := isoE2ERemoveStoredTestboxKey(r.leaseID); err != nil {
		return fmt.Errorf("remove ISO E2E SSH key: %w", err)
	}
	return nil
}

func storedISOE2ETestboxKeyExists(leaseID string) bool {
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return false
	}
	_, err = os.Stat(keyPath)
	return err == nil
}

func removeISOE2EStoredTestboxKey(leaseID string) error {
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Dir(keyPath))
}

func (r *isoE2ERuntime) cleanupLocalArtifacts() error {
	var cleanupErr error
	for _, path := range r.cleanupLocal {
		if _, keep := r.keepLocal[path]; keep {
			continue
		}
		if err := isoE2ERemoveLocalArtifact(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove local ISO artifact %s: %w", filepath.Base(path), err))
		}
	}
	return cleanupErr
}

func resolveISOE2EPlacement(ctx context.Context, client lifecycleClient, cfg Config) (xcpNgPlacement, error) {
	xcfg := xcpNgProviderConfig(cfg)
	if err := validateXCPNgConfig(xcfg); err != nil {
		return xcpNgPlacement{}, err
	}
	if strings.TrimSpace(xcfg.SR) == "" && strings.TrimSpace(xcfg.SRUUID) == "" {
		return xcpNgPlacement{}, exit(3, "xcp-ng configuration is incomplete: missing xcpNg.sr/xcpNg.srUuid or CRABBOX_XCP_NG_SR/CRABBOX_XCP_NG_SR_UUID")
	}
	srRef, err := client.ResolveSR(ctx, xcfg)
	if err != nil {
		return xcpNgPlacement{}, err
	}
	networkRef, err := client.ResolveNetwork(ctx, xcfg)
	if err != nil {
		return xcpNgPlacement{}, err
	}
	hostRef, err := client.ResolveHost(ctx, xcfg)
	if err != nil {
		return xcpNgPlacement{}, err
	}
	return xcpNgPlacement{srRef: srRef, networkRef: networkRef, hostRef: hostRef}, nil
}

func ensureWindowsInstallerSupported(isoValue, resolvedName string, summary *ISOE2ESummary) error {
	for _, candidate := range []string{isoValue, resolvedName} {
		if windowsInstallerLooksARM(candidate) {
			summary.Classification = "windows_requirements_blocked"
			summary.Phase = "installer_iso"
			summary.Reason = "Windows ISO appears to target ARM, but the XCP-ng ISO E2E lab expects x86_64/x64 installer media"
			return core.Exit(4, "%s", summary.Reason)
		}
	}
	return nil
}

func windowsInstallerLooksARM(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	normalized := strings.ToLower(strings.ReplaceAll(value, "\\", "/"))
	if strings.Contains(normalized, "/isos-arm/") {
		return true
	}
	return isoE2EWindowsARMLinePattern.MatchString(filepath.Base(normalized))
}

func windowsInstallerRequiresVTPM(values ...string) bool {
	for _, value := range values {
		if isoE2EWindows11LinePattern.MatchString(filepath.Base(strings.TrimSpace(value))) {
			return true
		}
	}
	return false
}

func validateISOE2EOptions(opts *ISOE2EOptions) error {
	if opts.Mode != "read-only" && opts.Mode != "mutate" {
		return fmt.Errorf("mode must be read-only or mutate")
	}
	if opts.OS != "linux" && opts.OS != "windows" {
		return fmt.Errorf("os must be linux or windows")
	}
	if strings.TrimSpace(opts.ISO) == "" {
		return fmt.Errorf("installer ISO is required")
	}
	if opts.NamePrefix == "" {
		opts.NamePrefix = "crabbox-xcpng-iso-e2e"
	}
	return nil
}

func isoE2EVMName(opts ISOE2EOptions) string {
	prefix := strings.TrimSpace(opts.NamePrefix)
	if prefix == "" {
		prefix = "crabbox-xcpng-iso-e2e"
	}
	return fmt.Sprintf("%s-%s-%s", prefix, opts.OS, isoE2ECurrentTime().Format("20060102t150405z"))
}

func WriteISOE2ESummary(path string, summary ISOE2ESummary) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func isoE2EOSLabel(osName string) string {
	if strings.EqualFold(osName, "windows") {
		return "Windows"
	}
	return "Linux"
}

func writeLinuxSeedISO(ctx context.Context, evidenceDir string, payload xcpNgLinuxAutoinstallPayload) (string, error) {
	workDir, err := os.MkdirTemp(evidenceDir, "linux-seed-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(workDir)
	if err := os.WriteFile(filepath.Join(workDir, "user-data"), []byte(payload.UserData), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(workDir, "meta-data"), []byte(payload.MetaData), 0o600); err != nil {
		return "", err
	}
	path, err := reserveISOArtifactPath(evidenceDir, "linux-seed-*.iso")
	if err != nil {
		return "", err
	}
	if err := buildDataISO(ctx, path, workDir, "CIDATA"); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("build Linux seed ISO: %w", err)
	}
	return path, nil
}

func reserveISOArtifactPath(evidenceDir, pattern string) (string, error) {
	file, err := os.CreateTemp(evidenceDir, pattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func writeWindowsAnswerISO(ctx context.Context, evidenceDir string, payload xcpNgWindowsAutounattendPayload) (string, error) {
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		return "", err
	}
	workDir, err := os.MkdirTemp(evidenceDir, "windows-answer-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(workDir)
	if err := os.WriteFile(filepath.Join(workDir, "AUTOUNATTEND.XML"), []byte(payload.AnswerXML), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(workDir, "CRABBOX-BOOTSTRAP.PS1"), []byte(payload.BootstrapPowerShell), 0o600); err != nil {
		return "", err
	}
	artifactDir, err := os.MkdirTemp(evidenceDir, "windows-answer-media-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(artifactDir, fmt.Sprintf("%s-windows-answer.iso", isoE2ECurrentTime().Format("20060102t150405z")))
	if err := buildDataISO(ctx, path, workDir, "CRABBOXWIN"); err != nil {
		_ = os.RemoveAll(artifactDir)
		return "", fmt.Errorf("build Windows answer ISO: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.RemoveAll(artifactDir)
		return "", fmt.Errorf("secure Windows answer ISO: %w", err)
	}
	return path, nil
}

func buildDataISO(ctx context.Context, outputISO, inputDir, volumeName string) error {
	xorriso, err := findXorrisoBinary()
	if err != nil {
		return err
	}
	args := dataISOArgs(outputISO, inputDir, volumeName)
	if out, err := exec.CommandContext(ctx, xorriso, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func injectUbuntuAutoinstallIntoGrubConfig(data []byte) ([]byte, bool, error) {
	foundEntry := false
	modified := false
	updated := isoE2EUbuntuLinuxLinePattern.ReplaceAllStringFunc(string(data), func(line string) string {
		parts := isoE2EUbuntuLinuxLinePattern.FindStringSubmatch(line)
		if len(parts) != 4 {
			return line
		}
		foundEntry = true
		args := strings.Fields(parts[2])
		for _, arg := range args {
			if arg == "autoinstall" {
				return line
			}
		}
		modified = true
		middle := strings.TrimSpace(parts[2])
		if middle == "" {
			middle = "autoinstall"
		} else {
			middle = middle + " autoinstall"
		}
		return parts[1] + middle + parts[3]
	})
	if !foundEntry {
		return nil, false, errors.New("autoinstall boot entry not found in installer grub config")
	}
	return []byte(updated), modified, nil
}

func patchUbuntuAutoinstallGrubFile(path string) (bool, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read installer grub config %s: %w", path, err)
	}
	updated, modified, err := injectUbuntuAutoinstallIntoGrubConfig(data)
	if err != nil {
		return false, false, err
	}
	if !modified {
		return true, false, nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return true, false, fmt.Errorf("chmod installer grub config %s: %w", path, err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return true, false, fmt.Errorf("write installer grub config %s: %w", path, err)
	}
	return true, true, nil
}

func findXorrisoBinary() (string, error) {
	for _, candidate := range []string{"xorriso", "/opt/homebrew/bin/xorriso", "/usr/local/bin/xorriso"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("xorriso not found; install xorriso to run XCP-ng ISO E2E media builders")
}

func ubuntuAutoinstallRemasterArgs(srcISO, outputISO string, mappings [][2]string) []string {
	args := []string{
		"-indev", srcISO,
		"-outdev", outputISO,
		"-boot_image", "any", "replay",
		"-overwrite", "on",
	}
	for _, mapping := range mappings {
		args = append(args, "-map", mapping[0], mapping[1])
	}
	args = append(args, "-commit", "-end")
	return args
}

func dataISOArgs(outputISO, inputDir, volumeName string) []string {
	return []string{
		"-as", "mkisofs",
		"-quiet",
		"-o", outputISO,
		"-J",
		"-r",
		"-V", volumeName,
		inputDir,
	}
}

func remasterUbuntuAutoinstallISO(ctx context.Context, srcISO, evidenceDir string) (string, error) {
	workDir, err := os.MkdirTemp(evidenceDir, "linux-installer-remaster-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(workDir)
	extractDir := filepath.Join(workDir, "src")
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(ctx, "bsdtar", "-C", extractDir, "-xf", srcISO).CombinedOutput(); err != nil {
		return "", fmt.Errorf("extract installer ISO: %v: %s", err, strings.TrimSpace(string(out)))
	}
	grubDir := filepath.Join(extractDir, "boot", "grub")
	mappings := make([][2]string, 0, 2)
	grubPath := filepath.Join(grubDir, "grub.cfg")
	found, _, err := patchUbuntuAutoinstallGrubFile(grubPath)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("autoinstall boot entry not found in installer grub config")
	}
	mappings = append(mappings, [2]string{grubPath, "/boot/grub/grub.cfg"})
	loopbackPath := filepath.Join(grubDir, "loopback.cfg")
	if _, modified, err := patchUbuntuAutoinstallGrubFile(loopbackPath); err != nil {
		return "", err
	} else if modified {
		mappings = append(mappings, [2]string{loopbackPath, "/boot/grub/loopback.cfg"})
	}
	outputISO, err := reserveISOArtifactPath(evidenceDir, "linux-installer-autoinstall-*.iso")
	if err != nil {
		return "", err
	}
	xorriso, err := findXorrisoBinary()
	if err != nil {
		_ = os.Remove(outputISO)
		return "", err
	}
	args := ubuntuAutoinstallRemasterArgs(srcISO, outputISO, mappings)
	if out, err := exec.CommandContext(ctx, xorriso, args...).CombinedOutput(); err != nil {
		_ = os.Remove(outputISO)
		return "", fmt.Errorf("remaster installer ISO with autoinstall boot arg: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return outputISO, nil
}

func minDuration(left, right time.Duration) time.Duration {
	if left <= 0 {
		return right
	}
	if right <= 0 {
		return left
	}
	if left < right {
		return left
	}
	return right
}
