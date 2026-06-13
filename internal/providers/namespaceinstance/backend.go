package namespaceinstance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const providerName = "namespace-instance"

type Backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	if strings.TrimSpace(cfg.NamespaceInstance.WorkRoot) != "" {
		cfg.WorkRoot = cfg.NamespaceInstance.WorkRoot
	}
	return &Backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *Backend) Spec() core.ProviderSpec { return b.spec }

func (b *Backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	name := core.LeaseProviderName(leaseID, slug)
	keyPath, _, err := core.EnsureTestboxKey(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg := b.configForRun()
	machineType := namespaceInstanceMachineTypeForConfig(cfg)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s machine_type=%s duration=%s keep=%v\n", providerName, leaseID, slug, name, machineType, cfg.NamespaceInstance.Duration, req.Keep)

	instance, err := b.createInstance(ctx, cfg, name, leaseID, slug, keyPath+".pub", req.Keep)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if strings.TrimSpace(instance.ID) == "" {
		if instance.ID = strings.TrimSpace(instance.Name); instance.ID == "" {
			return core.LeaseTarget{}, core.Exit(5, "namespace instance create response missing id")
		}
	}
	lease, err := b.prepareLease(ctx, cfg, instance, leaseID, slug, keyPath, req.Keep)
	if err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(instance.ID, err)
		}
		return core.LeaseTarget{}, err
	}
	if err := core.ClaimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			err = b.cleanupFailedAcquire(instance.ID, err)
		}
		return core.LeaseTarget{}, err
	}
	if err := core.UpdateLeaseClaimEndpoint(leaseID, lease.Server, lease.SSH); err != nil {
		if !req.Keep {
			core.RemoveLeaseClaim(leaseID)
			err = b.cleanupFailedAcquire(instance.ID, err)
		}
		return core.LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s instance=%s state=ready\n", leaseID, instance.ID)
	return lease, nil
}

func (b *Backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	claim, claimOK, err := core.ResolveLeaseClaim(req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if claimOK && claim.Provider != "" && claim.Provider != providerName {
		return core.LeaseTarget{}, core.Exit(4, "%q is claimed by provider %s", req.ID, claim.Provider)
	}
	id := strings.TrimSpace(req.ID)
	leaseID := id
	slug := core.NormalizeLeaseSlug(id)
	if claimOK {
		id = firstNonBlank(claim.CloudID, claim.Labels["name"], claim.LeaseID)
		leaseID = claim.LeaseID
		slug = firstNonBlank(claim.Slug, core.NewLeaseSlug(leaseID))
	}
	if id == "" {
		return core.LeaseTarget{}, core.Exit(2, "provider=%s requires --id <instance-id-or-slug>", providerName)
	}
	instance, err := b.describeInstance(ctx, id)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if leaseID == "" {
		leaseID = firstNonBlank(instance.Labels["lease"], namespaceInstanceLeaseIDFromName(instance.Name), namespaceInstanceLeaseIDFromName(instance.ID))
	}
	if slug == "" {
		slug = firstNonBlank(instance.Labels["slug"], core.NormalizeLeaseSlug(instance.Name), core.NewLeaseSlug(leaseID))
	}
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: namespaceInstanceServer(instance, leaseID, slug, b.configForRun(), true), LeaseID: leaseID}, nil
	}
	keyPath, _, err := core.EnsureTestboxKey(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, b.configForRun(), instance, leaseID, slug, keyPath, true)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := core.ClaimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *Backend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	instances, err := b.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	cfg := b.configForRun()
	var out []core.LeaseView
	for _, instance := range instances {
		if !req.All && !namespaceInstanceOwned(instance) {
			continue
		}
		leaseID := firstNonBlank(instance.Labels["lease"], namespaceInstanceLeaseIDFromName(firstNonBlank(instance.Name, instance.ID)))
		slug := firstNonBlank(instance.Labels["slug"], core.NormalizeLeaseSlug(firstNonBlank(instance.Name, instance.ID)))
		out = append(out, namespaceInstanceServer(instance, leaseID, slug, cfg, true))
	}
	return out, nil
}

func (b *Backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	if _, err := b.commandOutput(ctx, b.withCommonArgs([]string{"auth", "check-login"}, b.configForRun())); err != nil {
		return core.DoctorResult{}, core.Exit(1, "namespace nsc auth check failed: %v", err)
	}
	instances, err := b.listInstances(ctx)
	if err != nil {
		return core.DoctorResult{}, core.Exit(1, "namespace nsc list failed: %v", err)
	}
	return core.InventoryDoctorResult(providerName, len(instances)), nil
}

func (b *Backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	id := firstNonBlank(req.Lease.Server.CloudID, req.Lease.Server.Name)
	if id == "" {
		if claim, ok, err := core.ResolveLeaseClaim(req.Lease.LeaseID); err != nil {
			return err
		} else if ok {
			id = firstNonBlank(claim.CloudID, claim.Labels["name"], claim.LeaseID)
		}
	}
	if id == "" {
		return core.Exit(2, "provider=%s release requires an instance id", providerName)
	}
	if err := b.destroyInstance(ctx, id); err != nil {
		return err
	}
	core.RemoveLeaseClaim(req.Lease.LeaseID)
	core.RemoveStoredTestboxKey(req.Lease.LeaseID)
	return nil
}

func (b *Backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("destroyed namespace instance lease=%s instance=%s", lease.LeaseID, lease.Server.CloudID)
}

func (b *Backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	id := firstNonBlank(server.CloudID, server.Name)
	duration := b.configForRun().NamespaceInstance.Duration
	if id != "" && duration > 0 {
		if err := b.extendInstance(ctx, id, duration); err != nil {
			return core.Server{}, err
		}
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.configForRun(), req.State, time.Now().UTC())
	return server, nil
}

func (b *Backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	views, err := b.List(ctx, core.ListRequest{All: false})
	if err != nil {
		return err
	}
	servers := make([]core.Server, 0, len(views))
	for _, view := range views {
		servers = append(servers, view)
	}
	helper := shared.DirectSSHBackend{
		SpecValue: b.spec,
		Cfg:       b.configForRun(),
		RT:        b.rt,
		Delete: func(ctx context.Context, _ core.Config, server core.Server) error {
			return b.destroyInstance(ctx, firstNonBlank(server.CloudID, server.Name))
		},
	}
	return helper.CleanupServers(ctx, req, servers)
}

func (b *Backend) configForRun() core.Config {
	cfg := b.cfg
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = namespaceInstanceMachineTypeForConfig(cfg)
	cfg.Network = core.NetworkPublic
	cfg.SSHPort = firstNonBlank(cfg.SSHPort, "22")
	cfg.SSHFallbackPorts = nil
	if strings.TrimSpace(cfg.NamespaceInstance.WorkRoot) != "" {
		cfg.WorkRoot = cfg.NamespaceInstance.WorkRoot
	}
	if cfg.NamespaceInstance.Duration <= 0 {
		cfg.NamespaceInstance.Duration = 30 * time.Minute
	}
	return cfg
}

func (b *Backend) createInstance(ctx context.Context, cfg core.Config, name, leaseID, slug, publicKeyPath string, keep bool) (namespaceInstance, error) {
	args := []string{
		"create",
		"--machine_type", namespaceInstanceMachineTypeForConfig(cfg),
		"--duration", durationArg(cfg.NamespaceInstance.Duration),
		"--ssh_key", publicKeyPath,
		"--unique_tag", name,
		"--label", "crabbox=true",
		"--label", "provider=" + providerName,
		"--label", "lease=" + leaseID,
		"--label", "slug=" + slug,
		"--label", fmt.Sprintf("keep=%t", keep),
		"-o", "json",
	}
	if cfg.NamespaceInstance.Ephemeral {
		args = append(args, "--ephemeral")
	}
	args = b.withCommonArgs(args, cfg)
	for _, volume := range cfg.NamespaceInstance.Volumes {
		args = append(args, "--volume", volume)
	}
	out, err := b.commandOutput(ctx, args)
	if err != nil {
		return namespaceInstance{}, err
	}
	return parseNamespaceInstanceObject(out)
}

func (b *Backend) describeInstance(ctx context.Context, id string) (namespaceInstance, error) {
	out, err := b.commandOutput(ctx, b.withCommonArgs([]string{"describe", id, "-o", "json"}, b.configForRun()))
	if err != nil {
		return namespaceInstance{}, err
	}
	return parseNamespaceInstanceObject(out)
}

func (b *Backend) listInstances(ctx context.Context) ([]namespaceInstance, error) {
	out, err := b.commandOutput(ctx, b.withCommonArgs([]string{"list", "-o", "json"}, b.configForRun()))
	if err != nil {
		return nil, err
	}
	return parseNamespaceInstanceList(out)
}

func (b *Backend) destroyInstance(ctx context.Context, id string) error {
	result, err := b.runCommand(ctx, b.withCommonArgs([]string{"destroy", id, "--force"}, b.configForRun()), b.rt.Stdout, b.rt.Stderr)
	if err == nil {
		return nil
	}
	if isNamespaceNotFound(result.Stdout + "\n" + result.Stderr + "\n" + err.Error()) {
		return nil
	}
	return core.Exit(result.ExitCode, "namespace instance destroy %s failed: %v", id, err)
}

func (b *Backend) extendInstance(ctx context.Context, id string, duration time.Duration) error {
	result, err := b.runCommand(ctx, b.withCommonArgs([]string{"extend", id, "--ensure_minimum", durationArg(duration)}, b.configForRun()), b.rt.Stdout, b.rt.Stderr)
	if err != nil {
		return core.Exit(result.ExitCode, "namespace instance extend %s failed: %v", id, err)
	}
	return nil
}

func (b *Backend) prepareLease(ctx context.Context, cfg core.Config, instance namespaceInstance, leaseID, slug, keyPath string, keep bool) (core.LeaseTarget, error) {
	target, err := namespaceInstanceSSHTarget(instance, keyPath, cfg)
	if err != nil {
		refreshed, describeErr := b.describeInstance(ctx, instance.ID)
		if describeErr != nil {
			return core.LeaseTarget{}, err
		}
		instance = mergeNamespaceInstance(instance, refreshed)
		target, err = namespaceInstanceSSHTarget(instance, keyPath, cfg)
		if err != nil {
			return core.LeaseTarget{}, err
		}
	}
	target.TargetOS = core.TargetLinux
	target.NetworkKind = core.NetworkPublic
	target.ReadyCheck = "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"
	server := namespaceInstanceServer(instance, leaseID, slug, cfg, keep)
	server.PublicNet.IPv4.IP = target.Host
	if err := namespaceInstanceWaitForSSH(ctx, &target, b.rt.Stderr, "namespace instance ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	server.Status = "ready"
	server.Labels["state"] = "ready"
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *Backend) withCommonArgs(args []string, cfg core.Config) []string {
	if strings.TrimSpace(cfg.NamespaceInstance.Region) != "" {
		args = append(args, "--region", strings.TrimSpace(cfg.NamespaceInstance.Region))
	}
	if strings.TrimSpace(cfg.NamespaceInstance.Endpoint) != "" {
		args = append(args, "--endpoint", strings.TrimSpace(cfg.NamespaceInstance.Endpoint))
	}
	if strings.TrimSpace(cfg.NamespaceInstance.Keychain) != "" {
		args = append(args, "--keychain", strings.TrimSpace(cfg.NamespaceInstance.Keychain))
	}
	return args
}

func (b *Backend) commandOutput(ctx context.Context, args []string) (string, error) {
	result, err := b.runCommand(ctx, args, nil, nil)
	if err != nil {
		return "", core.Exit(result.ExitCode, "namespace nsc failed: %v: %s", err, strings.TrimSpace(result.Stdout+result.Stderr))
	}
	if result.Stderr != "" && b.rt.Stderr != nil {
		_, _ = io.WriteString(b.rt.Stderr, result.Stderr)
	}
	return result.Stdout, nil
}

func (b *Backend) runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (core.LocalCommandResult, error) {
	return b.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: "nsc", Args: args, Stdout: stdout, Stderr: stderr})
}

func (b *Backend) cleanupFailedAcquire(id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := b.destroyInstance(ctx, id); err != nil {
		return fmt.Errorf("%w; namespace cleanup failed for instance %s: %v", cause, id, err)
	}
	return cause
}

type namespaceInstance struct {
	ID          string
	Name        string
	Status      string
	MachineType string
	Labels      map[string]string
	SSH         namespaceSSH
	CreatedAt   string
	Deadline    string
}

type namespaceSSH struct {
	User string
	Host string
	Port string
}

func namespaceInstanceSSHTarget(instance namespaceInstance, keyPath string, cfg core.Config) (core.SSHTarget, error) {
	host := firstNonBlank(instance.SSH.Host)
	port := firstNonBlank(instance.SSH.Port, cfg.SSHPort, "22")
	user := firstNonBlank(instance.SSH.User, cfg.SSHUser, "root")
	if host == "" {
		return core.SSHTarget{}, core.Exit(5, "namespace instance %s missing SSH host", firstNonBlank(instance.ID, instance.Name))
	}
	return core.SSHTarget{User: user, Host: host, Port: port, Key: keyPath}, nil
}

func namespaceInstanceServer(instance namespaceInstance, leaseID, slug string, cfg core.Config, keep bool) core.Server {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	for key, value := range instance.Labels {
		if strings.TrimSpace(value) != "" {
			labels[key] = value
		}
	}
	labels["provider"] = providerName
	labels["lease"] = leaseID
	labels["slug"] = slug
	labels["name"] = firstNonBlank(instance.Name, instance.ID)
	labels["state"] = firstNonBlank(instance.Status, "unknown")
	server := core.Server{
		CloudID:  firstNonBlank(instance.ID, instance.Name),
		Provider: providerName,
		Name:     firstNonBlank(instance.Name, instance.ID),
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = firstNonBlank(instance.MachineType, namespaceInstanceMachineTypeForConfig(cfg))
	return server
}

func parseNamespaceInstanceObject(output string) (namespaceInstance, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(extractJSONValue(output)), &raw); err != nil {
		return namespaceInstance{}, core.Exit(5, "namespace nsc returned invalid JSON: %v", err)
	}
	for _, key := range []string{"instance", "data"} {
		if nested, ok := raw[key].(map[string]any); ok {
			raw = nested
			break
		}
	}
	return namespaceInstanceFromMap(raw), nil
}

func parseNamespaceInstanceList(output string) ([]namespaceInstance, error) {
	value := extractJSONValue(output)
	var raws []map[string]any
	if err := json.Unmarshal([]byte(value), &raws); err != nil {
		var wrapped map[string][]map[string]any
		if err2 := json.Unmarshal([]byte(value), &wrapped); err2 != nil {
			if strings.TrimSpace(output) == "" || strings.Contains(strings.ToLower(output), "no instance") {
				return nil, nil
			}
			return nil, core.Exit(5, "namespace nsc list returned invalid JSON: %v", err)
		}
		for _, key := range []string{"instances", "items", "data"} {
			if wrapped[key] != nil {
				raws = wrapped[key]
				break
			}
		}
	}
	out := make([]namespaceInstance, 0, len(raws))
	for _, raw := range raws {
		out = append(out, namespaceInstanceFromMap(raw))
	}
	return out, nil
}

func namespaceInstanceFromMap(raw map[string]any) namespaceInstance {
	labels := labelsFromAny(firstValue(raw, "labels", "metadata"))
	sshMap := mapFromAny(firstValue(raw, "ssh", "ssh_config", "sshConfig"))
	instance := namespaceInstance{
		ID:          stringValue(firstValue(raw, "id", "instance_id", "instanceId", "cid")),
		Name:        stringValue(firstValue(raw, "name", "display_name", "displayName")),
		Status:      stringValue(firstValue(raw, "status", "state")),
		MachineType: stringValue(firstValue(raw, "machine_type", "machineType", "shape")),
		Labels:      labels,
		CreatedAt:   stringValue(firstValue(raw, "created_at", "createdAt")),
		Deadline:    stringValue(firstValue(raw, "deadline", "expires_at", "expiresAt")),
		SSH: namespaceSSH{
			User: stringValue(firstValue(raw, "ssh_user", "sshUser", "username")),
			Host: stringValue(firstValue(raw, "ssh_host", "sshHost", "endpoint", "public_ip", "publicIp")),
			Port: stringValue(firstValue(raw, "ssh_port", "sshPort", "port")),
		},
	}
	if instance.SSH.User == "" {
		instance.SSH.User = stringValue(firstValue(sshMap, "user", "username"))
	}
	if instance.SSH.Host == "" {
		instance.SSH.Host = stringValue(firstValue(sshMap, "host", "hostname", "endpoint"))
	}
	if instance.SSH.Port == "" {
		instance.SSH.Port = stringValue(firstValue(sshMap, "port"))
	}
	if instance.ID == "" {
		instance.ID = stringValue(firstValue(labels, "lease", "name"))
	}
	return instance
}

func mergeNamespaceInstance(base, next namespaceInstance) namespaceInstance {
	if base.ID == "" {
		base.ID = next.ID
	}
	if base.Name == "" {
		base.Name = next.Name
	}
	if base.Status == "" {
		base.Status = next.Status
	}
	if base.MachineType == "" {
		base.MachineType = next.MachineType
	}
	if base.SSH.Host == "" {
		base.SSH = next.SSH
	}
	if len(base.Labels) == 0 {
		base.Labels = next.Labels
	}
	return base
}

func namespaceInstanceOwned(instance namespaceInstance) bool {
	return instance.Labels["crabbox"] == "true" || instance.Labels["provider"] == providerName || strings.HasPrefix(instance.Name, "crabbox-")
}

func namespaceInstanceLeaseIDFromName(name string) string {
	slug := core.NormalizeLeaseSlug(name)
	if strings.HasPrefix(slug, "cbx-") {
		return strings.Replace(slug, "cbx-", "cbx_", 1)
	}
	if slug == "" {
		slug = "namespace-instance"
	}
	return "nsi_" + slug
}

func extractJSONValue(output string) string {
	trimmed := strings.TrimSpace(output)
	arrayStart := strings.Index(trimmed, "[")
	objectStart := strings.Index(trimmed, "{")
	switch {
	case objectStart >= 0 && (arrayStart < 0 || objectStart < arrayStart):
		return trimmed[objectStart:]
	case arrayStart >= 0:
		return trimmed[arrayStart:]
	}
	return trimmed
}

func labelsFromAny(value any) map[string]string {
	out := map[string]string{}
	for key, value := range mapFromAny(value) {
		if s := stringValue(value); s != "" {
			out[key] = s
		}
	}
	return out
}

func mapFromAny(value any) map[string]any {
	if value == nil {
		return nil
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	if typed, ok := value.(map[string]string); ok {
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			out[key] = value
		}
		return out
	}
	return nil
}

func firstValue[V any](values map[string]V, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return fmt.Sprintf("%.0f", v)
	case int:
		return fmt.Sprintf("%d", v)
	default:
		return ""
	}
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func durationArg(duration time.Duration) string {
	if duration <= 0 {
		duration = 30 * time.Minute
	}
	return duration.Truncate(time.Second).String()
}

func isNamespaceNotFound(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "not found") || strings.Contains(value, "no such instance") || strings.Contains(value, "not_exist")
}

var namespaceInstanceWaitForSSH = core.WaitForSSHReady
