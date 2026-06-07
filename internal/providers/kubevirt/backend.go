package kubevirt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type leaseBackend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b *leaseBackend) Spec() core.ProviderSpec { return b.spec }

func (b *leaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	if err := validateAcquireConfig(b.cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	slug, err := b.allocateLeaseSlug(ctx, leaseID, req.RequestedSlug)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	name := core.LeaseProviderName(leaseID, slug)
	keyPath, publicKey, err := b.sshKey(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s namespace=%s keep=%v\n", providerName, leaseID, slug, name, b.cfg.KubeVirt.Namespace, req.Keep)
	if err := b.createVM(ctx, name, leaseID, slug, publicKey, req.Keep); err != nil {
		if !req.Keep {
			b.removeGeneratedKey(leaseID)
		}
		return core.LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, name, leaseID, slug, keyPath, req.Keep, nil)
	if err != nil {
		if !req.Keep {
			_ = b.deleteVM(context.Background(), name)
			b.removeGeneratedKey(leaseID)
		}
		return core.LeaseTarget{}, err
	}
	if err := b.claimLeaseForRepo(leaseID, slug, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			_ = b.deleteVM(context.Background(), name)
			b.removeGeneratedKey(leaseID)
		}
		return core.LeaseTarget{}, err
	}
	if err := core.UpdateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
		if !req.Keep {
			_ = b.deleteVM(context.Background(), name)
			b.removeGeneratedKey(leaseID)
			b.removeLeaseClaim(leaseID)
		}
		return core.LeaseTarget{}, err
	}
	return lease, nil
}

func (b *leaseBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	name, leaseID, slug, keep, err := b.resolveIdentity(ctx, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: b.server(name, leaseID, slug, keep), LeaseID: leaseID}, nil
	}
	keyPath, err := b.resolveSSHKey(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	persistedLabels, err := b.persistedVMLabels(ctx, name, leaseID, slug)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.startVM(ctx, name); err != nil {
		return core.LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, name, leaseID, slug, keyPath, keep, persistedLabels)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := b.claimLeaseForRepo(leaseID, slug, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := core.UpdateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *leaseBackend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	item, name, leaseID, slug, err := b.resolveStatusItem(ctx, req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	return b.statusView(ctx, item, name, leaseID, slug), nil
}

func (b *leaseBackend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	items, err := b.listVMs(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]core.Server, 0, len(items))
	for _, item := range items {
		servers = append(servers, b.itemToServer(item))
	}
	return servers, nil
}

func (b *leaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	if _, err := b.runKubectl(ctx, nil, "version", "--client=true", "-o", "json"); err != nil {
		return core.DoctorResult{}, core.Exit(5, "kubectl unavailable: %v", err)
	}
	if _, err := b.runVirtctl(ctx, nil, "version", "--client"); err != nil {
		return core.DoctorResult{}, core.Exit(5, "virtctl unavailable: %v", err)
	}
	servers, err := b.List(ctx, core.ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	return core.CLIDoctorResult(providerName, len(servers), "unchecked"), nil
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	name := strings.TrimSpace(req.Lease.Server.Name)
	if name == "" {
		name, _, _, _ = b.resolveName(req.Lease.LeaseID)
	}
	if name == "" {
		return core.Exit(2, "KubeVirt release requires a VM name")
	}
	expectedSlug := ""
	if req.Lease.Server.Labels != nil {
		expectedSlug = req.Lease.Server.Labels["slug"]
	}
	if _, _, _, _, err := b.validateVMIdentity(ctx, name, req.Lease.LeaseID, expectedSlug); err != nil {
		return err
	}
	var err error
	if b.cfg.KubeVirt.DeleteOnRelease {
		err = b.deleteVM(ctx, name)
	} else {
		err = b.stopVM(ctx, name)
	}
	if err == nil && b.cfg.KubeVirt.DeleteOnRelease {
		b.removeLeaseClaim(req.Lease.LeaseID)
		b.removeGeneratedKey(req.Lease.LeaseID)
	}
	return err
}

func (b *leaseBackend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("released KubeVirt lease=%s name=%s", lease.LeaseID, lease.Server.Name)
}

func (b *leaseBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.cfg, req.State, time.Now().UTC())
	if err := b.patchVMAnnotations(ctx, server.Name, server.Labels); err != nil {
		return core.Server{}, err
	}
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	servers, err := b.List(ctx, core.ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	for _, server := range servers {
		shouldDelete, reason := core.ShouldCleanupServer(server, time.Now().UTC())
		if !shouldDelete {
			fmt.Fprintf(b.rt.Stderr, "skip vm id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would delete vm id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "delete vm id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
		if _, _, _, _, err := b.validateVMIdentity(ctx, server.Name, server.Labels["lease"], server.Labels["slug"]); err != nil {
			return err
		}
		if err := b.deleteVM(ctx, server.Name); err != nil {
			return err
		}
		if leaseID := strings.TrimSpace(server.Labels["lease"]); leaseID != "" {
			b.removeLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
		}
	}
	return nil
}

func (b *leaseBackend) prepareLease(ctx context.Context, name, leaseID, slug, keyPath string, keep bool, persistedLabels map[string]string) (core.LeaseTarget, error) {
	target := b.sshTarget(name, keyPath)
	server := b.server(name, leaseID, slug, keep)
	for key, value := range persistedLabels {
		server.Labels[key] = value
	}
	server.PublicNet.IPv4.IP = name
	vmiStatus, err := b.waitForVMIReadyForSSH(ctx, name, kubeVirtStatusTimeout(b.cfg))
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := core.WaitForSSHReady(ctx, &target, b.rt.Stderr, "KubeVirt SSH", core.BootstrapWaitTimeout(b.cfg)); err != nil {
		return core.LeaseTarget{}, core.Exit(5, "%v; KubeVirt VMI diagnostics: %s", err, b.vmiDiagnostics(ctx, name, vmiStatus, nil))
	}
	server.Status = "ready"
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.cfg, "ready", time.Now().UTC())
	if err := b.patchVMAnnotations(ctx, name, server.Labels); err != nil {
		return core.LeaseTarget{}, err
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *leaseBackend) createVM(ctx context.Context, name, leaseID, slug, publicKey string, keep bool) error {
	if err := validateAcquireConfig(b.cfg); err != nil {
		return err
	}
	leaseLabels := b.server(name, leaseID, slug, keep).Labels
	manifest, err := renderManifest(b.cfg.KubeVirt.Template, name, b.cfg.KubeVirt.Namespace, leaseID, slug, publicKey, leaseLabels)
	if err != nil {
		return core.Exit(2, "%v", err)
	}
	tmp, err := os.CreateTemp("", "crabbox-kubevirt-*.yaml")
	if err != nil {
		return fmt.Errorf("create KubeVirt manifest: %w", err)
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(manifest); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write KubeVirt manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close KubeVirt manifest: %w", err)
	}
	if _, err := b.runKubectl(ctx, b.rt.Stdout, "apply", "-f", path); err != nil {
		return err
	}
	_, err = b.runVirtctl(ctx, b.rt.Stdout, "start", name)
	if err != nil && !keep {
		_ = b.deleteVM(context.Background(), name)
	}
	return err
}

func (b *leaseBackend) startVM(ctx context.Context, name string) error {
	_, err := b.runVirtctl(ctx, b.rt.Stdout, "start", name)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already running") {
		return nil
	}
	return err
}

func (b *leaseBackend) stopVM(ctx context.Context, name string) error {
	_, err := b.runVirtctl(ctx, b.rt.Stdout, "stop", name)
	return err
}

func (b *leaseBackend) deleteVM(ctx context.Context, name string) error {
	_, err := b.runKubectl(ctx, b.rt.Stdout, "delete", "virtualmachine.kubevirt.io/"+name, "--wait=true")
	return err
}

func (b *leaseBackend) patchVMAnnotations(ctx context.Context, name string, labels map[string]string) error {
	annotations := map[string]string{}
	for key, value := range labels {
		annotations[annotationBase+key] = value
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": annotations},
	})
	if err != nil {
		return fmt.Errorf("encode KubeVirt annotation patch: %w", err)
	}
	_, err = b.runKubectl(ctx, nil, "patch", "virtualmachine.kubevirt.io/"+name, "--type=merge", "-p", string(patch))
	return err
}

func (b *leaseBackend) listVMs(ctx context.Context) ([]kubeVirtItem, error) {
	out, err := b.runKubectl(ctx, nil, "get", "virtualmachines.kubevirt.io", "-l", managedByLabel+"=crabbox", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list kubeVirtList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, core.Exit(5, "KubeVirt inventory returned invalid JSON: %v", err)
	}
	return list.Items, nil
}

func (b *leaseBackend) getVM(ctx context.Context, name string) (kubeVirtItem, error) {
	out, err := b.runKubectl(ctx, nil, "get", "virtualmachine.kubevirt.io/"+name, "-o", "json")
	if err != nil {
		return kubeVirtItem{}, err
	}
	var item kubeVirtItem
	if err := json.Unmarshal([]byte(out), &item); err != nil {
		return kubeVirtItem{}, core.Exit(5, "KubeVirt VM lookup returned invalid JSON: %v", err)
	}
	return item, nil
}

func (b *leaseBackend) getVMI(ctx context.Context, name string) (kubeVirtVMI, error) {
	out, err := b.runKubectl(ctx, nil, "get", "virtualmachineinstances.kubevirt.io/"+name, "-o", "json")
	if err != nil {
		return kubeVirtVMI{}, err
	}
	var item kubeVirtVMI
	if err := json.Unmarshal([]byte(out), &item); err != nil {
		return kubeVirtVMI{}, core.Exit(5, "KubeVirt VMI lookup returned invalid JSON: %v", err)
	}
	return item, nil
}

func (b *leaseBackend) waitForVMIReadyForSSH(ctx context.Context, name string, timeout time.Duration) (kubeVirtVMIStatus, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	var last kubeVirtVMIStatus
	var lastErr error
	for {
		if ctx.Err() != nil {
			return last, context.Cause(ctx)
		}
		item, err := b.getVMI(ctx, name)
		if err == nil {
			lastErr = nil
			last = item.Status
			if vmiAllowsSSHProbe(last) {
				return last, nil
			}
			if vmiTerminalPhase(last.Phase) {
				return last, core.Exit(5, "KubeVirt VMI %s reached terminal phase before SSH: %s", name, b.vmiDiagnostics(ctx, name, last, nil))
			}
		} else if kubeVirtNotFound(err) {
			lastErr = err
		} else {
			return last, err
		}
		if time.Now().After(deadline) {
			return last, core.Exit(5, "timed out waiting for KubeVirt VMI %s to be scheduled for SSH probing: %s", name, b.vmiDiagnostics(ctx, name, last, lastErr))
		}
		if b.rt.Stderr != nil {
			fmt.Fprintf(b.rt.Stderr, "waiting for KubeVirt VMI %s to be scheduled... elapsed=%s remaining=%s %s\n", name, time.Since(start).Round(time.Second), time.Until(deadline).Round(time.Second), vmiStatusSummary(last, lastErr))
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return last, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

func (b *leaseBackend) vmiDiagnostics(ctx context.Context, name string, status kubeVirtVMIStatus, lastErr error) string {
	parts := []string{vmiStatusSummary(status, lastErr)}
	if events := b.eventSummary(ctx, name); events != "" {
		parts = append(parts, "events="+events)
	}
	return strings.Join(parts, " ")
}

func (b *leaseBackend) eventSummary(ctx context.Context, name string) string {
	out, err := b.runKubectl(ctx, nil, "get", "events", "--field-selector", "involvedObject.name="+name, "--sort-by=.lastTimestamp", "-o", "json")
	if err != nil {
		return ""
	}
	var list kubeVirtEventList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return ""
	}
	if len(list.Items) == 0 {
		return ""
	}
	start := len(list.Items) - 5
	if start < 0 {
		start = 0
	}
	parts := make([]string, 0, len(list.Items)-start)
	for _, item := range list.Items[start:] {
		piece := strings.TrimSpace(item.Type)
		if item.Reason != "" {
			if piece != "" {
				piece += "/"
			}
			piece += item.Reason
		}
		if item.Message != "" {
			piece += ":" + item.Message
		}
		parts = append(parts, compactDiagnostic(piece, 240))
	}
	return strings.Join(parts, "; ")
}

func (b *leaseBackend) findVMByLabel(ctx context.Context, key, value string) (kubeVirtItem, bool, error) {
	items, err := b.listVMs(ctx)
	if err != nil {
		return kubeVirtItem{}, false, err
	}
	matches := make([]kubeVirtItem, 0, 1)
	for _, item := range items {
		if item.Metadata.Labels[key] == value {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 0:
		return kubeVirtItem{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return kubeVirtItem{}, false, core.Exit(4, "KubeVirt label %s=%s matched %d VMs", key, value, len(matches))
	}
}

func (b *leaseBackend) runKubectl(ctx context.Context, stdout io.Writer, args ...string) (string, error) {
	commandArgs := b.kubeArgs()
	commandArgs = append(commandArgs, args...)
	return b.runCommand(ctx, b.cfg.KubeVirt.Kubectl, commandArgs, stdout)
}

func (b *leaseBackend) runVirtctl(ctx context.Context, stdout io.Writer, args ...string) (string, error) {
	commandArgs := b.kubeArgs()
	commandArgs = append(commandArgs, args...)
	return b.runCommand(ctx, b.cfg.KubeVirt.Virtctl, commandArgs, stdout)
}

func (b *leaseBackend) runCommand(ctx context.Context, command string, args []string, stdout io.Writer) (string, error) {
	result, err := b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:   strings.TrimSpace(command),
		Args:   args,
		Stdout: stdout,
		Stderr: b.rt.Stderr,
	})
	if err != nil {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		return "", core.Exit(result.ExitCode, "%s failed: %v: %s", command, err, message)
	}
	return result.Stdout, nil
}

func (b *leaseBackend) kubeArgs() []string {
	args := []string{}
	if value := strings.TrimSpace(b.cfg.KubeVirt.Kubeconfig); value != "" {
		args = append(args, "--kubeconfig", value)
	}
	if value := strings.TrimSpace(b.cfg.KubeVirt.Context); value != "" {
		args = append(args, "--context", value)
	}
	args = append(args, "--namespace", b.cfg.KubeVirt.Namespace)
	return args
}

func (b *leaseBackend) sshKey(leaseID string) (string, string, error) {
	keyPath := strings.TrimSpace(b.cfg.KubeVirt.SSHKey)
	publicKey := strings.TrimSpace(b.cfg.KubeVirt.SSHPublicKey)
	if keyPath == "" {
		return core.EnsureTestboxKey(leaseID)
	}
	if publicKey != "" {
		resolved, err := kubeVirtPublicKeyValue(publicKey)
		if err != nil {
			return "", "", err
		}
		publicKey = resolved
		if publicKey == "" {
			return "", "", core.Exit(2, "KubeVirt SSH public key is empty")
		}
		return keyPath, publicKey, nil
	}
	publicKey, err := readKubeVirtPublicKeyFile(keyPath + ".pub")
	if err != nil {
		return "", "", err
	}
	return keyPath, publicKey, nil
}

func kubeVirtPublicKeyValue(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || looksLikeInlineSSHPublicKey(value) {
		return value, nil
	}
	if publicKey, err := readKubeVirtPublicKeyFile(value); err == nil {
		return publicKey, nil
	} else if looksLikePublicKeyPath(value) {
		return "", err
	}
	return value, nil
}

func readKubeVirtPublicKeyFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", core.Exit(2, "read KubeVirt SSH public key %s: %v", path, err)
	}
	publicKey := strings.TrimSpace(string(data))
	if publicKey == "" {
		return "", core.Exit(2, "KubeVirt SSH public key %s is empty", path)
	}
	return publicKey, nil
}

func looksLikeInlineSSHPublicKey(value string) bool {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return false
	}
	switch fields[0] {
	case "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
		return true
	default:
		return false
	}
}

func looksLikePublicKeyPath(value string) bool {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "~/") {
		return true
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	return strings.Contains(value, string(os.PathSeparator)) || strings.HasSuffix(value, ".pub")
}

func (b *leaseBackend) resolveSSHKey(leaseID string) (string, error) {
	if keyPath := strings.TrimSpace(b.cfg.KubeVirt.SSHKey); keyPath != "" {
		return keyPath, nil
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(keyPath); err != nil {
		if os.IsNotExist(err) {
			return "", core.Exit(4, "stored SSH key for KubeVirt lease %s is missing; configure kubevirt.sshKey or recreate the VM", leaseID)
		}
		return "", core.Exit(2, "read stored SSH key for KubeVirt lease %s: %v", leaseID, err)
	}
	return keyPath, nil
}

func (b *leaseBackend) removeGeneratedKey(leaseID string) {
	if strings.TrimSpace(b.cfg.KubeVirt.SSHKey) == "" {
		core.RemoveStoredTestboxKey(leaseID)
	}
}

func (b *leaseBackend) sshTarget(name, keyPath string) core.SSHTarget {
	return core.SSHTarget{
		User:           b.cfg.KubeVirt.SSHUser,
		Host:           name,
		Key:            keyPath,
		Port:           b.cfg.KubeVirt.SSHPort,
		TargetOS:       core.TargetLinux,
		NetworkKind:    core.NetworkPublic,
		SSHConfigProxy: true,
		ProxyCommand:   b.proxyCommand(name),
		ReadyCheck:     "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null",
	}
}

func (b *leaseBackend) proxyCommand(name string) string {
	parts := []string{b.cfg.KubeVirt.Virtctl}
	parts = append(parts, b.kubeArgs()...)
	parts = append(parts, "port-forward", "--stdio=true", "vm/"+name+"/"+b.cfg.KubeVirt.Namespace, "%p")
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, core.ShellQuote(part))
	}
	return strings.Join(quoted, " ")
}

func (b *leaseBackend) server(name, leaseID, slug string, keep bool) core.Server {
	labels := core.DirectLeaseLabels(b.cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	labels["name"] = name
	labels["namespace"] = b.cfg.KubeVirt.Namespace
	labels["target"] = core.TargetLinux
	labels["state"] = "starting"
	server := core.Server{CloudID: b.cfg.KubeVirt.Namespace + "/" + name, Provider: providerName, Name: name, Status: "starting", Labels: labels}
	server.ServerType.Name = "virtualmachine"
	return server
}

func (b *leaseBackend) itemToServer(item kubeVirtItem) core.Server {
	name := strings.TrimSpace(item.Metadata.Name)
	leaseID := strings.TrimSpace(item.Metadata.Labels[leaseIDLabel])
	slug := strings.TrimSpace(item.Metadata.Labels[slugLabel])
	if slug == "" {
		slug = slugFromName(name)
	}
	if leaseID == "" {
		leaseID = leaseIDFromName(name)
	}
	server := b.server(name, leaseID, slug, true)
	for key, value := range item.Metadata.Annotations {
		if strings.HasPrefix(key, annotationBase) {
			server.Labels[strings.TrimPrefix(key, annotationBase)] = value
		}
	}
	server.Status = core.Blank(strings.TrimSpace(item.Status.PrintableStatus), "unknown")
	if strings.TrimSpace(server.Labels["state"]) == "" {
		server.Labels["state"] = server.Status
	}
	if item.Metadata.CreationTimestamp != "" {
		server.Labels["created"] = item.Metadata.CreationTimestamp
	}
	return server
}

func (b *leaseBackend) statusView(ctx context.Context, item kubeVirtItem, name, leaseID, slug string) core.StatusView {
	server := b.itemToServer(item)
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	state := kubeVirtVMState(item, server)
	server.Status = state
	server.Labels["state"] = state
	server.PublicNet.IPv4.IP = name
	target := b.sshTarget(name, b.statusSSHKey(leaseID))
	ready := false
	if kubeVirtStatusReady(state) {
		allowProbe := true
		if vmi, err := b.getVMI(ctx, name); err == nil {
			allowProbe = vmiAllowsSSHProbe(vmi.Status)
		}
		ready = allowProbe && core.ProbeSSHReady(ctx, &target, 4*time.Second)
	}
	return core.StatusView{
		ID:               leaseID,
		Slug:             slug,
		Provider:         providerName,
		TargetOS:         core.TargetLinux,
		State:            state,
		ServerID:         server.DisplayID(),
		ServerType:       server.ServerType.Name,
		Host:             server.PublicNet.IPv4.IP,
		Pond:             server.Labels["pond"],
		Network:          core.NetworkPublic,
		SSHHost:          target.Host,
		SSHUser:          target.User,
		SSHPort:          target.Port,
		SSHFallbackPorts: target.FallbackPorts,
		SSHKey:           target.Key,
		LastTouchedAt:    core.Blank(core.LeaseLabelTimeDisplay(server.Labels["last_touched_at"]), server.Labels["last_touched_at"]),
		IdleFor:          core.IdleForString(server.Labels["last_touched_at"], time.Now()),
		IdleTimeout:      core.LeaseLabelDurationDisplay(server.Labels["idle_timeout_secs"], server.Labels["idle_timeout"]),
		ExpiresAt:        core.Blank(core.LeaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"]),
		Labels:           server.Labels,
		HasHost:          target.Host != "",
		Ready:            ready,
	}
}

func (b *leaseBackend) statusSSHKey(leaseID string) string {
	if keyPath := strings.TrimSpace(b.cfg.KubeVirt.SSHKey); keyPath != "" {
		return keyPath
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(keyPath); err == nil {
		return keyPath
	}
	return ""
}

func kubeVirtVMState(item kubeVirtItem, server core.Server) string {
	if state := strings.TrimSpace(item.Status.PrintableStatus); state != "" {
		return state
	}
	return core.Blank(strings.TrimSpace(server.Labels["state"]), server.Status)
}

func kubeVirtStatusReady(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "ready", "running":
		return true
	default:
		return false
	}
}

type kubeVirtList struct {
	Items []kubeVirtItem `json:"items"`
}

type kubeVirtItem struct {
	Metadata struct {
		Name              string            `json:"name"`
		CreationTimestamp string            `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Status struct {
		PrintableStatus string `json:"printableStatus"`
	} `json:"status"`
}

type kubeVirtVMI struct {
	Status kubeVirtVMIStatus `json:"status"`
}

type kubeVirtVMIStatus struct {
	Phase      string              `json:"phase"`
	Conditions []kubeVirtCondition `json:"conditions"`
}

type kubeVirtCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type kubeVirtEventList struct {
	Items []kubeVirtEvent `json:"items"`
}

type kubeVirtEvent struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func kubeVirtStatusTimeout(cfg core.Config) time.Duration {
	timeout := core.BootstrapWaitTimeout(cfg)
	if timeout > 10*time.Minute {
		return 10 * time.Minute
	}
	return timeout
}

func vmiReportsRunning(status kubeVirtVMIStatus) bool {
	if strings.EqualFold(status.Phase, "Running") {
		return true
	}
	for _, condition := range status.Conditions {
		if condition.Type == "Ready" && strings.EqualFold(condition.Status, "True") {
			return true
		}
	}
	return false
}

func vmiAllowsSSHProbe(status kubeVirtVMIStatus) bool {
	return vmiReportsRunning(status) || strings.EqualFold(status.Phase, "Scheduled")
}

func vmiTerminalPhase(phase string) bool {
	return strings.EqualFold(phase, "Failed") || strings.EqualFold(phase, "Succeeded")
}

func kubeVirtNotFound(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "notfound")
}

func vmiStatusSummary(status kubeVirtVMIStatus, lastErr error) string {
	if lastErr != nil {
		return "lookup=" + compactDiagnostic(lastErr.Error(), 300)
	}
	phase := core.Blank(strings.TrimSpace(status.Phase), "unknown")
	if len(status.Conditions) == 0 {
		return "phase=" + phase + " conditions=none"
	}
	parts := make([]string, 0, len(status.Conditions))
	for _, condition := range status.Conditions {
		piece := condition.Type + "=" + condition.Status
		if condition.Reason != "" {
			piece += "/" + condition.Reason
		}
		if condition.Message != "" {
			piece += ":" + condition.Message
		}
		parts = append(parts, compactDiagnostic(piece, 240))
	}
	return "phase=" + phase + " conditions=" + strings.Join(parts, ",")
}

func compactDiagnostic(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if limit > 0 && len(value) > limit {
		return value[:limit] + "..."
	}
	return value
}

var crabboxNamePattern = regexp.MustCompile(`^crabbox-(.+)-[0-9a-f]{8}$`)

func slugFromName(name string) string {
	if match := crabboxNamePattern.FindStringSubmatch(strings.TrimSpace(name)); len(match) == 2 {
		return core.NormalizeLeaseSlug(match[1])
	}
	return core.NormalizeLeaseSlug(name)
}

func leaseIDFromName(name string) string {
	slug := core.NormalizeLeaseSlug(name)
	if slug == "" {
		slug = "vm"
	}
	if len(slug) > 80 {
		slug = slug[:80]
	}
	return "kv_" + slug
}

func (b *leaseBackend) claimScope() string {
	return kubeVirtClaimScope(b.cfg)
}

func kubeVirtClaimScope(cfg core.Config) string {
	kubeconfig := strings.TrimSpace(cfg.KubeVirt.Kubeconfig)
	if kubeconfig == "" {
		kubeconfig = strings.TrimSpace(os.Getenv("KUBECONFIG"))
	}
	if kubeconfig == "" {
		kubeconfig = "<default>"
	}
	contextName := strings.TrimSpace(cfg.KubeVirt.Context)
	if contextName == "" {
		contextName = "<current>"
	}
	namespace := strings.TrimSpace(cfg.KubeVirt.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	return "kubeconfig:" + kubeconfig + "|context:" + contextName + "|namespace:" + namespace
}

func (b *leaseBackend) allocateLeaseSlug(ctx context.Context, leaseID, requested string) (string, error) {
	items, err := b.listVMs(ctx)
	if err != nil {
		return "", err
	}
	base := core.NormalizeLeaseSlug(requested)
	checkClaims := base != ""
	if base == "" {
		base = core.NewLeaseSlug(leaseID)
	}
	slug := base
	for attempt := 0; attempt < 20; attempt++ {
		inUse := kubeVirtSlugInUse(slug, leaseID, items)
		if !inUse && checkClaims {
			inUse, err = b.claimSlugInUse(slug, leaseID)
		}
		if err != nil {
			return "", err
		}
		if !inUse {
			return slug, nil
		}
		slug = core.SlugWithCollisionSuffix(base, fmt.Sprintf("%s-%d", leaseID, attempt))
	}
	return core.SlugWithCollisionSuffix(base, leaseID), nil
}

func kubeVirtSlugInUse(slug, leaseID string, items []kubeVirtItem) bool {
	slug = core.NormalizeLeaseSlug(slug)
	if slug == "" {
		return false
	}
	for _, item := range items {
		itemSlug := core.NormalizeLeaseSlug(item.Metadata.Labels[slugLabel])
		if itemSlug == "" {
			itemSlug = slugFromName(item.Metadata.Name)
		}
		itemLeaseID := strings.TrimSpace(item.Metadata.Labels[leaseIDLabel])
		if itemLeaseID == "" {
			itemLeaseID = leaseIDFromName(item.Metadata.Name)
		}
		if itemLeaseID != leaseID && itemSlug == slug {
			return true
		}
	}
	return false
}

func (b *leaseBackend) claimSlugInUse(slug, leaseID string) (bool, error) {
	slug = core.NormalizeLeaseSlug(slug)
	if slug == "" {
		return false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return false, err
	}
	scope := b.claimScope()
	for _, claim := range claims {
		if claim.Provider != providerName || strings.TrimSpace(claim.ProviderScope) != scope {
			continue
		}
		if claim.LeaseID != "" && claim.LeaseID != leaseID && core.NormalizeLeaseSlug(claim.Slug) == slug {
			return true, nil
		}
	}
	return false, nil
}

func (b *leaseBackend) claimLeaseForRepo(leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScope(leaseID, slug, providerName, b.claimScope(), repoRoot, idleTimeout, reclaim)
}

func (b *leaseBackend) removeLeaseClaim(leaseID string) {
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil || claim.LeaseID == "" || claim.Provider != providerName {
		return
	}
	scope := strings.TrimSpace(claim.ProviderScope)
	if scope != "" && scope != b.claimScope() {
		return
	}
	core.RemoveLeaseClaim(leaseID)
}

func (b *leaseBackend) resolveClaim(identifier string) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, nil
	}
	scope := b.claimScope()
	if claim, err := core.ReadLeaseClaim(identifier); err != nil {
		return core.LeaseClaim{}, false, err
	} else if b.claimMatchesScope(claim, scope) {
		return claim, true, nil
	} else if claim.LeaseID != "" && strings.HasPrefix(identifier, "cbx_") {
		return core.LeaseClaim{}, false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	slug := core.NormalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if !b.claimMatchesScope(claim, scope) {
			continue
		}
		if claim.LeaseID == identifier || (slug != "" && core.NormalizeLeaseSlug(claim.Slug) == slug) {
			return claim, true, nil
		}
	}
	return core.LeaseClaim{}, false, nil
}

func (b *leaseBackend) claimMatchesScope(claim core.LeaseClaim, scope string) bool {
	return claim.LeaseID != "" && claim.Provider == providerName && strings.TrimSpace(claim.ProviderScope) == scope
}

func (b *leaseBackend) resolveName(identifier string) (string, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", "", core.Exit(2, "provider=%s requires --id <vm-name-or-slug>", providerName)
	}
	if claim, ok, err := b.resolveClaim(identifier); err != nil {
		return "", "", "", err
	} else if ok {
		slug := core.Blank(claim.Slug, core.NewLeaseSlug(claim.LeaseID))
		name := core.Blank(claim.Labels["name"], core.LeaseProviderName(claim.LeaseID, slug))
		return name, claim.LeaseID, slug, nil
	}
	if strings.HasPrefix(identifier, "cbx_") {
		slug := core.NewLeaseSlug(identifier)
		return core.LeaseProviderName(identifier, slug), identifier, slug, nil
	}
	slug := core.NormalizeLeaseSlug(identifier)
	return identifier, leaseIDFromName(identifier), slug, nil
}

func (b *leaseBackend) resolveIdentity(ctx context.Context, identifier string) (string, string, string, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", "", false, core.Exit(2, "provider=%s requires --id <vm-name-or-slug>", providerName)
	}
	if claim, ok, err := b.resolveClaim(identifier); err != nil {
		return "", "", "", false, err
	} else if ok {
		slug := core.Blank(claim.Slug, core.NewLeaseSlug(claim.LeaseID))
		name := core.Blank(claim.Labels["name"], core.LeaseProviderName(claim.LeaseID, slug))
		return b.validateVMIdentity(ctx, name, claim.LeaseID, slug)
	}
	if strings.HasPrefix(identifier, "cbx_") {
		if item, ok, err := b.findVMByLabel(ctx, leaseIDLabel, identifier); err != nil {
			return "", "", "", false, err
		} else if ok {
			name, leaseID, slug, keep, err := identityFromItem(item, identifier)
			if err != nil {
				return "", "", "", false, err
			}
			if leaseID != identifier {
				return "", "", "", false, core.Exit(4, "KubeVirt VM %q lease identity changed: expected %s, found %s", name, identifier, leaseID)
			}
			return name, leaseID, slug, keep, nil
		}
		return "", "", "", false, core.Exit(4, "KubeVirt lease %q was not found in namespace %s", identifier, b.cfg.KubeVirt.Namespace)
	}
	item, err := b.findVMByNameOrSlug(ctx, identifier)
	if err != nil {
		return "", "", "", false, err
	}
	return identityFromItem(item, identifier)
}

func (b *leaseBackend) resolveStatusItem(ctx context.Context, identifier string) (kubeVirtItem, string, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return kubeVirtItem{}, "", "", "", core.Exit(2, "provider=%s requires --id <vm-name-or-slug>", providerName)
	}
	if claim, ok, err := b.resolveClaim(identifier); err != nil {
		return kubeVirtItem{}, "", "", "", err
	} else if ok {
		slug := core.Blank(claim.Slug, core.NewLeaseSlug(claim.LeaseID))
		name := core.Blank(claim.Labels["name"], core.LeaseProviderName(claim.LeaseID, slug))
		item, err := b.getVM(ctx, name)
		if err != nil {
			return kubeVirtItem{}, "", "", "", err
		}
		actualName, leaseID, actualSlug, _, err := identityFromItem(item, name)
		if err != nil {
			return kubeVirtItem{}, "", "", "", err
		}
		if leaseID != claim.LeaseID {
			return kubeVirtItem{}, "", "", "", core.Exit(4, "KubeVirt VM %q lease identity changed: expected %s, found %s", actualName, claim.LeaseID, leaseID)
		}
		if actualSlug != core.NormalizeLeaseSlug(slug) {
			return kubeVirtItem{}, "", "", "", core.Exit(4, "KubeVirt VM %q slug identity changed: expected %s, found %s", actualName, slug, actualSlug)
		}
		return item, actualName, leaseID, actualSlug, nil
	}
	if strings.HasPrefix(identifier, "cbx_") {
		item, ok, err := b.findVMByLabel(ctx, leaseIDLabel, identifier)
		if err != nil {
			return kubeVirtItem{}, "", "", "", err
		}
		if !ok {
			return kubeVirtItem{}, "", "", "", core.Exit(4, "KubeVirt lease %q was not found in namespace %s", identifier, b.cfg.KubeVirt.Namespace)
		}
		name, leaseID, slug, _, err := identityFromItem(item, identifier)
		if err != nil {
			return kubeVirtItem{}, "", "", "", err
		}
		if leaseID != identifier {
			return kubeVirtItem{}, "", "", "", core.Exit(4, "KubeVirt VM %q lease identity changed: expected %s, found %s", name, identifier, leaseID)
		}
		return item, name, leaseID, slug, nil
	}
	item, err := b.findVMByNameOrSlug(ctx, identifier)
	if err != nil {
		return kubeVirtItem{}, "", "", "", err
	}
	name, leaseID, slug, _, err := identityFromItem(item, identifier)
	if err != nil {
		return kubeVirtItem{}, "", "", "", err
	}
	return item, name, leaseID, slug, nil
}

func (b *leaseBackend) findVMByNameOrSlug(ctx context.Context, identifier string) (kubeVirtItem, error) {
	items, err := b.listVMs(ctx)
	if err != nil {
		return kubeVirtItem{}, err
	}
	for _, item := range items {
		if strings.TrimSpace(item.Metadata.Name) == identifier {
			return item, nil
		}
	}
	slug := core.NormalizeLeaseSlug(identifier)
	if slug == "" {
		return kubeVirtItem{}, core.Exit(4, "KubeVirt VM or slug %q was not found in namespace %s", identifier, b.cfg.KubeVirt.Namespace)
	}
	matches := make([]kubeVirtItem, 0, 1)
	for _, item := range items {
		if core.NormalizeLeaseSlug(item.Metadata.Labels[slugLabel]) == slug {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return kubeVirtItem{}, core.Exit(4, "KubeVirt VM or slug %q was not found in namespace %s", identifier, b.cfg.KubeVirt.Namespace)
	default:
		return kubeVirtItem{}, core.Exit(4, "KubeVirt slug %q matched %d VMs in namespace %s", identifier, len(matches), b.cfg.KubeVirt.Namespace)
	}
}

func (b *leaseBackend) validateVMIdentity(ctx context.Context, name, expectedLeaseID, expectedSlug string) (string, string, string, bool, error) {
	item, err := b.getVM(ctx, name)
	if err != nil {
		return "", "", "", false, err
	}
	actualName, leaseID, slug, keep, err := identityFromItem(item, name)
	if err != nil {
		return "", "", "", false, err
	}
	if expectedLeaseID = strings.TrimSpace(expectedLeaseID); expectedLeaseID != "" && leaseID != expectedLeaseID {
		return "", "", "", false, core.Exit(4, "KubeVirt VM %q lease identity changed: expected %s, found %s", actualName, expectedLeaseID, leaseID)
	}
	if expectedSlug = core.NormalizeLeaseSlug(expectedSlug); expectedSlug != "" && slug != expectedSlug {
		return "", "", "", false, core.Exit(4, "KubeVirt VM %q slug identity changed: expected %s, found %s", actualName, expectedSlug, slug)
	}
	return actualName, leaseID, slug, keep, nil
}

func (b *leaseBackend) persistedVMLabels(ctx context.Context, name, expectedLeaseID, expectedSlug string) (map[string]string, error) {
	item, err := b.getVM(ctx, name)
	if err != nil {
		return nil, err
	}
	actualName, leaseID, slug, _, err := identityFromItem(item, name)
	if err != nil {
		return nil, err
	}
	if leaseID != expectedLeaseID {
		return nil, core.Exit(4, "KubeVirt VM %q lease identity changed: expected %s, found %s", actualName, expectedLeaseID, leaseID)
	}
	if slug != core.NormalizeLeaseSlug(expectedSlug) {
		return nil, core.Exit(4, "KubeVirt VM %q slug identity changed: expected %s, found %s", actualName, expectedSlug, slug)
	}
	return leaseLabelsFromItem(item), nil
}

func identityFromItem(item kubeVirtItem, fallbackName string) (string, string, string, bool, error) {
	name := core.Blank(strings.TrimSpace(item.Metadata.Name), fallbackName)
	if item.Metadata.Labels[managedByLabel] != "crabbox" {
		return "", "", "", false, core.Exit(4, "KubeVirt VM %q is not managed by Crabbox", name)
	}
	leaseID := strings.TrimSpace(item.Metadata.Labels[leaseIDLabel])
	slug := strings.TrimSpace(item.Metadata.Labels[slugLabel])
	if leaseID == "" || slug == "" {
		return "", "", "", false, core.Exit(4, "KubeVirt VM %q is missing Crabbox lease identity labels", name)
	}
	labels := leaseLabelsFromItem(item)
	return name, leaseID, slug, persistedKeep(labels), nil
}

func leaseLabelsFromItem(item kubeVirtItem) map[string]string {
	labels := map[string]string{}
	for key, value := range item.Metadata.Annotations {
		if strings.HasPrefix(key, annotationBase) {
			labels[strings.TrimPrefix(key, annotationBase)] = value
		}
	}
	return labels
}

func persistedKeep(labels map[string]string) bool {
	value := strings.TrimSpace(labels["keep"])
	if value == "" {
		return true
	}
	return strings.EqualFold(value, "true")
}
