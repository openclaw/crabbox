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
	client          lifecycleClient
	placement       xcpNgPlacement
	leaseID         string
	labels          map[string]string
	vm              xcpNgFreshVMResult
	importedInstaller xcpNgConfigDrive
	importedAnswer    xcpNgConfigDrive
	installerDrive  xcpNgConfigDrive
	answerDrive     xcpNgConfigDrive
	installDisk     xcpNgConfigDrive
	remasteredISO   string
	generatedSeed   string
	keyPath         string
	publicKey       string
	sshTarget       core.SSHTarget
	cleanupLocal    []string
	keepLocal       map[string]struct{}
	windowsFallback bool
}

const (
	isoE2EDefaultTimeout      = 90 * time.Minute
	isoE2EInstallerTimeout    = 55 * time.Minute
	isoE2EGuestMetricsTimeout = 20 * time.Minute
	isoE2EInstallDiskBytes    = 24 * 1024 * 1024 * 1024
)

var (
	isoE2ECurrentTime        = func() time.Time { return time.Now().UTC() }
	isoE2EWaitForSSHReady    = func(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error { return core.WaitForSSHReady(ctx, target, os.Stderr, phase, timeout) }
	isoE2ERunSSHQuiet        = func(ctx context.Context, target core.SSHTarget, remote string) error { return core.RunSSHQuiet(ctx, target, remote) }
	isoE2EEnsureTestboxKey   = func(cfg Config, leaseID string) (string, string, error) { return core.EnsureTestboxKeyForConfig(cfg, leaseID) }
	isoE2EProviderKeyForLease = func(leaseID string) string { return core.ProviderKeyForLease(leaseID) }
	isoE2ERemasterUbuntuISO  = remasterUbuntuAutoinstallISO
	isoE2EWriteLinuxSeedISO  = writeLinuxSeedISO
	isoE2EUbuntuLinuxLinePattern = regexp.MustCompile(`(?m)^(\s*linux\s+/casper/vmlinuz\s+)(.*?)(\s+---\s*)$`)
)

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
		if closeErr := client.Close(context.Background()); closeErr != nil && err == nil {
			err = closeErr
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
	if opts.OS == "windows" {
		return runISOE2EWindowsFoundation(ctx, client, placement, opts, summary)
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

func runISOE2EWindowsFoundation(ctx context.Context, client lifecycleClient, placement xcpNgPlacement, opts ISOE2EOptions, summary ISOE2ESummary) (result ISOE2ESummary, err error) {
	result = summary
	now := isoE2ECurrentTime()
	leaseID := fmt.Sprintf("cbx_isoe2e_%d", now.Unix())
	labels := core.DirectLeaseLabels(opts.Config, leaseID, strings.TrimPrefix(opts.NamePrefix, "crabbox-"), "xcp-ng", "", true, now)
	installer, err := client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), opts.ISO)
	if err != nil {
		result.Classification = "environment_blocked"
		result.Reason = err.Error()
		result.Phase = "installer_iso"
		return result, err
	}
	if opts.Mode == "mutate" && installer.Source == "local-file" {
		result.Classification = "environment_blocked"
		result.Reason = "local ISO upload is not implemented yet for Windows; use an ISO VDI name, UUID, or OpaqueRef"
		result.Phase = "installer_iso"
		return result, core.Exit(3, "%s", result.Reason)
	}
	result.Details["installer_source"] = installer.Source
	if installer.UUID != "" {
		result.Details["installer_uuid"] = installer.UUID
	}
	if opts.AnswerISO != "" {
		answerISO, resolveErr := client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), opts.AnswerISO)
		if resolveErr != nil {
			result.Classification = "environment_blocked"
			result.Reason = resolveErr.Error()
			result.Phase = "answer_iso"
			return result, resolveErr
		}
		if answerISO.Source == "local-file" {
			result.Classification = "environment_blocked"
			result.Reason = "local answer ISO upload is not implemented yet for Windows; use an ISO VDI name, UUID, or OpaqueRef"
			result.Phase = "answer_iso"
			return result, core.Exit(3, "%s", result.Reason)
		}
		result.Details["answer_iso_source"] = answerISO.Source
		if answerISO.UUID != "" {
			result.Details["answer_iso_uuid"] = answerISO.UUID
		}
	}
	runtime := &isoE2ERuntime{
		client:    client,
		placement: placement,
		leaseID:   leaseID,
		labels:    labels,
	}
	runtime.labels["workflow"] = "iso-e2e"
	runtime.labels["os"] = opts.OS
	runtime.labels["name_prefix"] = opts.NamePrefix
	if err = runtime.createBaseVM(ctx, opts, &result); err != nil {
		return result, err
	}
	defer runtime.cleanup(context.Background(), &result, &err)
	if err = runtime.attachInstallerISO(ctx, installer, "3"); err != nil {
		result.Classification = "environment_blocked"
		result.Reason = err.Error()
		return result, err
	}
	if opts.AnswerISO != "" {
		answerISO, _ := client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), opts.AnswerISO)
		if answerISO.VDIRef != "" {
			if err = runtime.attachAnswerISO(ctx, answerISO, "4"); err != nil {
				result.Classification = "environment_blocked"
				result.Reason = err.Error()
				return result, err
			}
		}
	}
	if err = runtime.startVM(ctx); err != nil {
		result.Classification = "environment_blocked"
		result.Reason = err.Error()
		return result, err
	}
	result.Classification = "started_unverified"
	result.Phase = "started_vm"
	result.Reason = "VM.start succeeded, but Windows unattended install proof remains owned by PLAN-03"
	return result, nil
}

func runISOE2ELinux(ctx context.Context, client lifecycleClient, placement xcpNgPlacement, opts ISOE2EOptions, summary ISOE2ESummary) (result ISOE2ESummary, err error) {
	result = summary
	now := isoE2ECurrentTime()
	leaseID := fmt.Sprintf("cbx_isoe2e_%d", now.Unix())
	labels := core.DirectLeaseLabels(opts.Config, leaseID, strings.TrimPrefix(opts.NamePrefix, "crabbox-"), "xcp-ng", "", true, now)
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
	keyPath, publicKey, err := isoE2EEnsureTestboxKey(opts.Config, runtime.leaseID)
	if err != nil {
		result.Classification = "test_failed"
		result.Phase = "linux_seed_generation"
		result.Reason = err.Error()
		return result, err
	}
	runtime.keyPath = keyPath
	runtime.publicKey = publicKey
	result.Details["ssh_key"] = filepath.Base(keyPath)
	if err = runtime.prepareInstallerMedia(ctx, opts, &result); err != nil {
		return result, err
	}
	defer runtime.cleanupLocalArtifacts()
	if err = runtime.createBaseVM(ctx, opts, &result); err != nil {
		return result, err
	}
	defer runtime.cleanup(context.Background(), &result, &err)
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
	firstBootIP, ipErr := runtime.waitForGuestIPv4(installerCtx)
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
	if !fileExists(installerPath) {
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
	}
	payload, err := newLinuxAutoinstallPayload(opts.Config, r.leaseID, strings.TrimPrefix(r.leaseID, "cbx_"), r.publicKey)
	if err != nil {
		summary.Classification = "test_failed"
		summary.Phase = "linux_seed_generation"
		summary.Reason = err.Error()
		return err
	}
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
		HVMBoot: map[string]string{
			"order": func() string {
				if opts.OS == "linux" {
					return "dc"
				}
				return "dc"
			}(),
		},
	})
	if err != nil {
		summary.Classification = "environment_blocked"
		summary.Phase = "create_vm"
		summary.Reason = err.Error()
		return err
	}
	r.vm = created
	summary.VMUUID = created.VM.UUID
	summary.VMRef = created.VM.Ref
	summary.Cleanup = "pending"
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
		SRRef:        r.placement.srRef,
		Path:         summary.ISO,
		Name:         filepath.Base(summary.ISO),
		Description:  "Crabbox imported Linux installer ISO",
		Labels:       r.labels,
		DestroyVDI:   true,
		MarkReadOnly: true,
	})
	if importErr != nil {
		summary.Classification = "environment_blocked"
		summary.Phase = "installer_iso"
		summary.Reason = importErr.Error()
		return xcpNgISOMediaRef{}, importErr
	}
	r.importedInstaller = imported
		summary.Evidence["installer_iso_imported"] = imported.Name
	return xcpNgISOMediaRef{VDIRef: imported.VDIRef, NameLabel: imported.Name, Source: "imported-local-file"}, nil
}

func (r *isoE2ERuntime) attachInstallDisk(ctx context.Context) error {
	disk, err := r.client.AttachDisk(ctx, xcpNgDiskAttachRequest{
		VMRef:       xapiRef(r.vm.VM.Ref),
		SRRef:       r.placement.srRef,
		Name:        r.vm.VM.Name + "-install-disk",
		Description: "Crabbox Linux ISO install disk",
		SizeBytes:   isoE2EInstallDiskBytes,
		UserDevice:  "0",
		Labels:      r.labels,
		Unpluggable: true,
		DestroyVDI:  true,
	})
	if err != nil {
		return err
	}
	r.installDisk = disk
	return nil
}

func (r *isoE2ERuntime) attachInstallerISO(ctx context.Context, installer xcpNgISOMediaRef, userDevice string) error {
	drive, err := r.client.AttachISO(ctx, xcpNgISOAttachRequest{VMRef: xapiRef(r.vm.VM.Ref), ISO: installer, UserDevice: userDevice, Bootable: true, Labels: r.labels, Unpluggable: true})
	if err != nil {
		if r.importedInstaller.VDIRef != "" && r.importedInstaller.DestroyVDI {
			if deleteErr := r.client.DeleteConfigDrive(context.Background(), r.importedInstaller); deleteErr == nil {
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
		SRRef:        r.placement.srRef,
		Path:         answerValue,
		Name:         filepath.Base(answerValue),
		Description:  "Crabbox Linux autoinstall seed ISO",
		Labels:       r.labels,
		DestroyVDI:   true,
		MarkReadOnly: true,
	})
	if importErr != nil {
		summary.Classification = "environment_blocked"
		summary.Phase = "answer_iso"
		summary.Reason = importErr.Error()
		return importErr
	}
	r.importedAnswer = imported
	summary.Details["answer_iso_source"] = "generated-local-file"
	return r.attachAnswerISO(ctx, xcpNgISOMediaRef{VDIRef: imported.VDIRef, NameLabel: imported.Name, Source: "generated-local-file"}, "4")
}

func (r *isoE2ERuntime) attachAnswerISO(ctx context.Context, answerISO xcpNgISOMediaRef, userDevice string) error {
	drive, err := r.client.AttachISO(ctx, xcpNgISOAttachRequest{VMRef: xapiRef(r.vm.VM.Ref), ISO: answerISO, UserDevice: userDevice, Bootable: false, Labels: r.labels, Unpluggable: true})
	if err != nil {
		if r.importedAnswer.VDIRef != "" && r.importedAnswer.DestroyVDI {
			if deleteErr := r.client.DeleteConfigDrive(context.Background(), r.importedAnswer); deleteErr == nil {
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

func (r *isoE2ERuntime) waitForGuestIPv4(ctx context.Context) (string, error) {
	deadline := time.Now().Add(minDuration(isoE2EGuestMetricsTimeout, time.Until(time.Now().Add(isoE2EGuestMetricsTimeout))))
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	var lastErr error
	for {
		ip, err := r.client.GuestIPv4(ctx, xapiRef(r.vm.VM.Ref))
		if err == nil && ip != "" {
			return ip, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr != nil {
				return "", exit(5, "timed out waiting for XCP-ng guest IPv4 after Linux install: %v", lastErr)
			}
			return "", exit(5, "timed out waiting for XCP-ng guest IPv4 after Linux install")
		}
		select {
		case <-ctx.Done():
			if lastErr != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				return "", exit(5, "timed out waiting for XCP-ng guest IPv4 after Linux install: %v", lastErr)
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
	cleanupErr := r.client.DeleteServer(ctx, r.vm.VM.Ref)
	for _, drive := range []xcpNgConfigDrive{r.answerDrive, r.installerDrive, r.importedAnswer, r.importedInstaller} {
		if !drive.DestroyVDI || drive.VDIRef == "" {
			continue
		}
		if err := r.client.DeleteConfigDrive(ctx, drive); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
	}
	if cleanupErr != nil {
		summary.Cleanup = "resource_cleanup_failed"
		summary.Details["cleanup_error"] = cleanupErr.Error()
		if summary.Classification == "linux_install_passed" || summary.Classification == "started_unverified" {
			summary.Classification = "resource_cleanup_failed"
		}
		if *runErr == nil {
			*runErr = cleanupErr
		}
		return
	}
	summary.Cleanup = "cleaned"
}

func (r *isoE2ERuntime) cleanupLocalArtifacts() {
	for _, path := range r.cleanupLocal {
		if _, keep := r.keepLocal[path]; keep {
			continue
		}
		_ = os.Remove(path)
	}
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
	path := filepath.Join(evidenceDir, fmt.Sprintf("%s-linux-seed.iso", isoE2ECurrentTime().Format("20060102t150405z")))
	args := []string{"makehybrid", "-quiet", "-o", path, workDir, "-iso", "-joliet", "-default-volume-name", "cidata"}
	if out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build Linux seed ISO: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
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
	grubPath := filepath.Join(extractDir, "boot", "grub", "grub.cfg")
	data, err := os.ReadFile(grubPath)
	if err != nil {
		return "", fmt.Errorf("read installer grub config: %w", err)
	}
	updated := isoE2EUbuntuLinuxLinePattern.ReplaceAllStringFunc(string(data), func(line string) string {
		parts := isoE2EUbuntuLinuxLinePattern.FindStringSubmatch(line)
		if len(parts) != 4 {
			return line
		}
		args := strings.Fields(parts[2])
		for _, arg := range args {
			if arg == "autoinstall" {
				return line
			}
		}
		middle := strings.TrimSpace(parts[2])
		if middle == "" {
			middle = "autoinstall"
		} else {
			middle = middle + " autoinstall"
		}
		return parts[1] + middle + parts[3]
	})
	if updated == string(data) || !strings.Contains(updated, "autoinstall") {
		return "", errors.New("autoinstall boot entry not found in installer grub config")
	}
	if err := os.WriteFile(grubPath, []byte(updated), 0o600); err != nil {
		return "", fmt.Errorf("write installer grub config: %w", err)
	}
	outputISO := filepath.Join(evidenceDir, fmt.Sprintf("%s-linux-installer-autoinstall.iso", isoE2ECurrentTime().Format("20060102t150405z")))
	args := []string{
		"makehybrid", "-quiet", "-o", outputISO, extractDir,
		"-iso", "-joliet",
		"-default-volume-name", "Ubuntu-Server-Autoinstall",
		"-eltorito-boot", "boot/grub/i386-pc/eltorito.img",
		"-no-emul-boot",
		"-eltorito-platform", "0",
		"-eltorito-specification", `({"eltorito-boot"="boot/grub/i386-pc/eltorito.img";"no-emul-boot"=1;"eltorito-platform"=0;},{"eltorito-boot"="EFI/boot/bootx64.efi";"no-emul-boot"=1;"eltorito-platform"=239;})`,
	}
	if out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", args...).CombinedOutput(); err != nil {
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
