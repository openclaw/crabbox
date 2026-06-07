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
	slug, err := core.AllocateClaimLeaseSlug(leaseID, req.RequestedSlug)
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
	if err := core.ClaimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
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
			core.RemoveLeaseClaim(leaseID)
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
		if err := core.ClaimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := core.UpdateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
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
		name, _, _, _ = resolveName(req.Lease.LeaseID)
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
		core.RemoveLeaseClaim(req.Lease.LeaseID)
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
			core.RemoveLeaseClaim(leaseID)
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
	if err := core.WaitForSSHReady(ctx, &target, b.rt.Stderr, "KubeVirt SSH", core.BootstrapWaitTimeout(b.cfg)); err != nil {
		return core.LeaseTarget{}, err
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
		return keyPath, publicKey, nil
	}
	data, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return "", "", core.Exit(2, "read KubeVirt SSH public key %s.pub: %v", keyPath, err)
	}
	publicKey = strings.TrimSpace(string(data))
	if publicKey == "" {
		return "", "", core.Exit(2, "KubeVirt SSH public key %s.pub is empty", keyPath)
	}
	return keyPath, publicKey, nil
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

func resolveName(identifier string) (string, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", "", core.Exit(2, "provider=%s requires --id <vm-name-or-slug>", providerName)
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName); err != nil {
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
	if claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName); err != nil {
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
