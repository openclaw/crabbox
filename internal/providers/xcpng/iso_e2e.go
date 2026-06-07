package xcpng

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		opts.Timeout = 20 * time.Minute
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
	client, err := newXAPIClient(ctx, opts.Config)
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
	installer, err := client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), opts.ISO)
	if err != nil {
		summary.Classification = "environment_blocked"
		summary.Reason = err.Error()
		summary.Phase = "installer_iso"
		return summary, err
	}
	if opts.Mode == "mutate" && installer.Source == "local-file" {
		summary.Classification = "environment_blocked"
		summary.Reason = "local ISO upload is not implemented yet; use an ISO VDI name, UUID, or OpaqueRef"
		summary.Phase = "installer_iso"
		return summary, core.Exit(3, "%s", summary.Reason)
	}
	summary.Details["installer_source"] = installer.Source
	if installer.UUID != "" {
		summary.Details["installer_uuid"] = installer.UUID
	}
	if opts.AnswerISO != "" {
		answerISO, resolveErr := client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), opts.AnswerISO)
		if resolveErr != nil {
			summary.Classification = "environment_blocked"
			summary.Reason = resolveErr.Error()
			summary.Phase = "answer_iso"
			return summary, resolveErr
		}
		if opts.Mode == "mutate" && answerISO.Source == "local-file" {
			summary.Classification = "environment_blocked"
			summary.Reason = "local answer ISO upload is not implemented yet; use an ISO VDI name, UUID, or OpaqueRef"
			summary.Phase = "answer_iso"
			return summary, core.Exit(3, "%s", summary.Reason)
		}
		summary.Details["answer_iso_source"] = answerISO.Source
		if answerISO.UUID != "" {
			summary.Details["answer_iso_uuid"] = answerISO.UUID
		}
	}
	if opts.Mode == "read-only" {
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
	leaseID := fmt.Sprintf("cbx_isoe2e_%d", time.Now().UTC().Unix())
	labels := core.DirectLeaseLabels(opts.Config, leaseID, strings.TrimPrefix(opts.NamePrefix, "crabbox-"), "xcp-ng", "", true, time.Now().UTC())
	labels["workflow"] = "iso-e2e"
	labels["os"] = opts.OS
	labels["name_prefix"] = opts.NamePrefix
	vmName := isoE2EVMName(opts)
	created, err := client.CreateFreshVM(ctx, xcpNgFreshVMRequest{
		Name:        vmName,
		Description: "Crabbox XCP-ng ISO E2E harness",
		HostRef:     placement.hostRef,
		Network: func() *xcpNgVIFSpec {
			if placement.networkRef == "" {
				return nil
			}
			return &xcpNgVIFSpec{Device: "0", NetworkRef: placement.networkRef, MTU: 1500, Labels: labels}
		}(),
		Labels:     labels,
		SecureBoot: strings.EqualFold(opts.OS, "windows"),
	})
	if err != nil {
		summary.Classification = "environment_blocked"
		summary.Reason = err.Error()
		summary.Phase = "create_vm"
		return summary, err
	}
	summary.VMUUID = created.VM.UUID
	summary.VMRef = created.VM.Ref
	summary.Phase = "attach_media"
	summary.Cleanup = "pending"
	defer func() {
		if created.VM.Ref == "" {
			return
		}
		cleanupErr := client.DeleteServer(context.Background(), created.VM.Ref)
		if cleanupErr != nil {
			summary.Cleanup = "resource_cleanup_failed"
			summary.Details["cleanup_error"] = cleanupErr.Error()
			if summary.Classification == "started_unverified" {
				summary.Classification = "resource_cleanup_failed"
			}
			if err == nil {
				err = cleanupErr
			}
			return
		}
		summary.Cleanup = "cleaned"
	}()
	if _, err := client.AttachISO(ctx, xcpNgISOAttachRequest{VMRef: xapiRef(created.VM.Ref), ISO: installer, UserDevice: "3", Bootable: true, Labels: labels, Unpluggable: true}); err != nil {
		summary.Classification = "environment_blocked"
		summary.Reason = err.Error()
		return summary, err
	}
	if opts.AnswerISO != "" {
		answerISO, _ := client.ResolveISOMedia(ctx, xcpNgProviderConfig(opts.Config), opts.AnswerISO)
		if answerISO.VDIRef != "" {
			if _, err := client.AttachISO(ctx, xcpNgISOAttachRequest{VMRef: xapiRef(created.VM.Ref), ISO: answerISO, UserDevice: "4", Bootable: false, Labels: labels, Unpluggable: true}); err != nil {
				summary.Classification = "environment_blocked"
				summary.Reason = err.Error()
				return summary, err
			}
		}
	}
	summary.Phase = "boot"
	if err := client.StartVM(ctx, xapiRef(created.VM.Ref)); err != nil {
		summary.Classification = "environment_blocked"
		summary.Reason = err.Error()
		return summary, err
	}
	// Foundation-only harness: VM.start proves lifecycle wiring, not installer boot.
	summary.Classification = "started_unverified"
	summary.Phase = "started_vm"
	summary.Reason = "VM.start succeeded, but installer boot proof is deferred to later OS-specific plans"
	return summary, nil
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
	return fmt.Sprintf("%s-%s-%s", prefix, opts.OS, time.Now().UTC().Format("20060102t150405z"))
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
