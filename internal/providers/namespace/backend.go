package namespace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type namespaceLeaseBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewNamespaceLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = namespaceProvider
	cfg.TargetOS = targetLinux
	cfg.SSHFallbackPorts = nil
	if strings.TrimSpace(cfg.Namespace.WorkRoot) != "" {
		cfg.WorkRoot = cfg.Namespace.WorkRoot
	}
	return &namespaceLeaseBackend{spec: spec, cfg: cfg, rt: rt}
}

func (b *namespaceLeaseBackend) Spec() ProviderSpec { return b.spec }

func (b *namespaceLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	leaseID := newLeaseID()
	slug := newLeaseSlug(leaseID)
	name := leaseProviderName(leaseID, slug)
	cfg := b.namespaceConfigForRun()
	size := namespaceSize(cfg)
	image := namespaceImage(cfg)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s image=%s size=%s keep=%v\n", namespaceProvider, leaseID, slug, name, image, size, req.Keep)
	if err := b.createDevbox(ctx, namespaceCreateSpec{
		Name:                name,
		Image:               image,
		Size:                strings.ToLower(size),
		Checkout:            strings.TrimSpace(cfg.Namespace.Repository),
		Site:                strings.TrimSpace(cfg.Namespace.Site),
		VolumeSizeGB:        cfg.Namespace.VolumeSizeGB,
		AutoStopIdleTimeout: fmt.Sprintf("%dm", durationMinutesCeil(namespaceAutoStopIdleTimeout(cfg))),
	}); err != nil {
		return LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, name, leaseID, slug, req.Keep)
	if err != nil {
		if !req.Keep {
			_ = b.deleteDevbox(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, namespaceProvider, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			_ = b.deleteDevbox(context.Background(), name)
		}
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s name=%s state=ready\n", leaseID, name)
	return lease, nil
}

func (b *namespaceLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	name, leaseID, slug, err := resolveNamespaceDevboxName(req.ID, req.Reclaim)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		server := namespaceServer(name, leaseID, slug, b.namespaceConfigForRun(), true)
		return LeaseTarget{Server: server, LeaseID: leaseID}, nil
	}
	lease, err := b.prepareLease(ctx, name, leaseID, slug, true)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := claimLeaseForRepoProvider(leaseID, slug, namespaceProvider, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *namespaceLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	items, err := b.listDevboxes(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(items))
	for _, item := range items {
		servers = append(servers, namespaceItemToServer(item, b.namespaceConfigForRun()))
	}
	return servers, nil
}

func (b *namespaceLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	name := strings.TrimSpace(req.Lease.Server.Name)
	if name == "" {
		name, _, _, _ = resolveNamespaceDevboxName(req.Lease.LeaseID, true)
	}
	if name == "" {
		return exit(2, "namespace devbox release requires a devbox name")
	}
	if b.cfg.Namespace.DeleteOnRelease {
		if err := b.deleteDevbox(ctx, name); err != nil {
			return err
		}
	} else if err := b.shutdownDevbox(ctx, name); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	if err := cleanupNamespaceSSHFiles(name, false, b.rt.Stdout); err != nil {
		return err
	}
	return nil
}

func (b *namespaceLeaseBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.cfg, req.State, time.Now().UTC())
	return server, nil
}

func (b *namespaceLeaseBackend) Cleanup(_ context.Context, req CleanupRequest) error {
	return cleanupNamespaceSSHFiles("", req.DryRun, b.rt.Stdout)
}

func (b *namespaceLeaseBackend) namespaceConfigForRun() Config {
	cfg := b.cfg
	cfg.Provider = namespaceProvider
	cfg.TargetOS = targetLinux
	cfg.ServerType = namespaceSize(cfg)
	cfg.WorkRoot = namespaceWorkRoot(cfg)
	cfg.SSHPort = "22"
	cfg.SSHFallbackPorts = nil
	return cfg
}

func (b *namespaceLeaseBackend) prepareLease(ctx context.Context, name, leaseID, slug string, keep bool) (LeaseTarget, error) {
	cfg := b.namespaceConfigForRun()
	target, err := b.prepareDevbox(ctx, name)
	if err != nil {
		return LeaseTarget{}, err
	}
	target.TargetOS = targetLinux
	target.NetworkKind = networkPublic
	target.ReadyCheck = "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"
	server := namespaceServer(name, leaseID, slug, cfg, keep)
	server.PublicNet.IPv4.IP = target.Host
	if err := waitForSSHReady(ctx, &target, b.rt.Stderr, "namespace devbox ssh", bootstrapWaitTimeout(cfg)); err != nil {
		return LeaseTarget{}, err
	}
	server.Status = "ready"
	server.Labels["state"] = "ready"
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *namespaceLeaseBackend) createDevbox(ctx context.Context, spec namespaceCreateSpec) error {
	tmp, err := os.CreateTemp("", "crabbox-namespace-devbox-*.yaml")
	if err != nil {
		return fmt.Errorf("create namespace devbox spec: %w", err)
	}
	path := tmp.Name()
	remove := true
	defer func() {
		_ = tmp.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	enc := yaml.NewEncoder(tmp)
	enc.SetIndent(2)
	if err := enc.Encode(spec); err != nil {
		return fmt.Errorf("encode namespace devbox spec: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close namespace devbox spec: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write namespace devbox spec: %w", err)
	}
	result, err := b.runCommand(ctx, []string{"create", "--from", path}, b.rt.Stdout, b.rt.Stderr)
	if err != nil {
		return ExitError{Code: result.ExitCode, Message: fmt.Sprintf("namespace devbox create failed: %v", err)}
	}
	return nil
}

func (b *namespaceLeaseBackend) prepareDevbox(ctx context.Context, name string) (SSHTarget, error) {
	target, configureErr := b.configureSSHDevbox(ctx, name)
	if configureErr == nil {
		return target, nil
	}
	out, err := b.commandOutput(ctx, []string{"prepare", name})
	if err != nil {
		return SSHTarget{}, configureErr
	}
	var result namespacePrepareResult
	if err := json.Unmarshal([]byte(extractJSONObject(out)), &result); err != nil {
		return SSHTarget{}, exit(5, "namespace devbox prepare returned invalid JSON: %v", err)
	}
	return namespaceSSHTarget(result)
}

func (b *namespaceLeaseBackend) configureSSHDevbox(ctx context.Context, name string) (SSHTarget, error) {
	if target, err := namespaceSSHTargetFromConfig(name); err == nil {
		return target, nil
	}
	result, err := b.runCommand(ctx, []string{"configure-ssh"}, b.rt.Stdout, b.rt.Stderr)
	if err != nil {
		return SSHTarget{}, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("namespace devbox configure-ssh failed: %v", err)}
	}
	return namespaceSSHTargetFromConfig(name)
}

func (b *namespaceLeaseBackend) listDevboxes(ctx context.Context) ([]namespaceListItem, error) {
	out, err := b.commandOutput(ctx, []string{"list", "-o", "json"})
	if err != nil {
		out, err = b.commandOutput(ctx, []string{"list", "--json"})
		if err != nil {
			return nil, err
		}
	}
	return parseNamespaceList(out)
}

func (b *namespaceLeaseBackend) shutdownDevbox(ctx context.Context, name string) error {
	result, err := b.runCommand(ctx, []string{"shutdown", name, "--force"}, b.rt.Stdout, b.rt.Stderr)
	if err != nil {
		result, err = b.runCommand(ctx, []string{"stop", name, "--force"}, b.rt.Stdout, b.rt.Stderr)
		if err != nil {
			return ExitError{Code: result.ExitCode, Message: fmt.Sprintf("namespace devbox shutdown failed: %v", err)}
		}
	}
	return nil
}

func (b *namespaceLeaseBackend) deleteDevbox(ctx context.Context, name string) error {
	result, err := b.runCommand(ctx, []string{"delete", name, "--force"}, b.rt.Stdout, b.rt.Stderr)
	if err != nil {
		result, err = b.runCommand(ctx, []string{"destroy", name, "--force"}, b.rt.Stdout, b.rt.Stderr)
		if err != nil {
			return ExitError{Code: result.ExitCode, Message: fmt.Sprintf("namespace devbox delete failed: %v", err)}
		}
	}
	return nil
}

func (b *namespaceLeaseBackend) commandOutput(ctx context.Context, args []string) (string, error) {
	result, err := b.runCommand(ctx, args, nil, nil)
	if err != nil {
		return "", ExitError{Code: result.ExitCode, Message: fmt.Sprintf("namespace devbox failed: %v: %s", err, strings.TrimSpace(result.Stdout+result.Stderr))}
	}
	return result.Stdout + result.Stderr, nil
}

func (b *namespaceLeaseBackend) runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	return b.rt.Exec.Run(ctx, LocalCommandRequest{Name: "devbox", Args: args, Stdout: stdout, Stderr: stderr})
}

type namespaceCreateSpec struct {
	Name                string `yaml:"name,omitempty"`
	Image               string `yaml:"image,omitempty"`
	Size                string `yaml:"size,omitempty"`
	Checkout            string `yaml:"checkout,omitempty"`
	Site                string `yaml:"site,omitempty"`
	VolumeSizeGB        int    `yaml:"volume_size_gb,omitempty"`
	AutoStopIdleTimeout string `yaml:"auto_stop_idle_timeout,omitempty"`
}

type namespacePrepareResult struct {
	SSHEndpoint string `json:"ssh_endpoint"`
	SSHKeyPath  string `json:"ssh_key_path"`
	SSHConfig   bool   `json:"-"`
}

type namespaceListItem struct {
	Name       string
	ID         string
	Status     string
	Size       string
	Repository string
	Created    string
}

func namespaceSSHTarget(result namespacePrepareResult) (SSHTarget, error) {
	endpoint := strings.TrimSpace(result.SSHEndpoint)
	keyPath := strings.TrimSpace(result.SSHKeyPath)
	if endpoint == "" {
		return SSHTarget{}, exit(5, "namespace devbox prepare response missing ssh_endpoint")
	}
	user, hostPort, ok := strings.Cut(endpoint, "@")
	if !ok || strings.TrimSpace(user) == "" || strings.TrimSpace(hostPort) == "" {
		return SSHTarget{}, exit(5, "namespace devbox prepare returned invalid ssh_endpoint %q", endpoint)
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		idx := strings.LastIndex(hostPort, ":")
		if idx <= 0 || idx == len(hostPort)-1 {
			return SSHTarget{}, exit(5, "namespace devbox prepare returned invalid ssh_endpoint %q", endpoint)
		}
		host = hostPort[:idx]
		port = hostPort[idx+1:]
	}
	return SSHTarget{User: user, Host: host, Port: port, Key: keyPath, SSHConfigProxy: result.SSHConfig}, nil
}

func namespaceSSHTargetFromConfig(name string) (SSHTarget, error) {
	host := namespaceSSHHost(name)
	path := filepath.Join(os.Getenv("HOME"), ".namespace", "ssh", host+".ssh")
	data, err := os.ReadFile(path)
	if err != nil {
		return SSHTarget{}, exit(5, "namespace devbox ssh config missing for %s: %v", name, err)
	}
	user := "devbox"
	key := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "host":
			if fields[1] != host {
				return SSHTarget{}, exit(5, "namespace devbox ssh config host mismatch: got %s want %s", fields[1], host)
			}
		case "user":
			user = fields[1]
		case "identityfile":
			key = expandHomePath(fields[1])
		}
	}
	if key == "" {
		return SSHTarget{}, exit(5, "namespace devbox ssh config missing IdentityFile for %s", name)
	}
	return SSHTarget{User: user, Host: host, Port: "22", Key: key, SSHConfigProxy: true}, nil
}

func cleanupNamespaceSSHFiles(name string, dryRun bool, stdout io.Writer) error {
	files, err := namespaceSSHCleanupFiles(name)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		if strings.TrimSpace(name) == "" {
			fmt.Fprintln(stdout, "namespace ssh cleanup no crabbox files found")
		}
		return nil
	}
	action := "delete"
	if dryRun {
		action = "would-delete"
	}
	for _, path := range files {
		fmt.Fprintf(stdout, "namespace ssh cleanup %s %s\n", action, path)
		if dryRun {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("namespace ssh cleanup %s: %w", path, err)
		}
	}
	return nil
}

func namespaceSSHCleanupFiles(name string) ([]string, error) {
	dir := filepath.Join(os.Getenv("HOME"), ".namespace", "ssh")
	if strings.TrimSpace(name) != "" {
		host := namespaceSSHHost(name)
		return namespaceFilterCleanupFiles([]string{
			filepath.Join(dir, host+".ssh"),
			filepath.Join(dir, host+".key"),
		}), nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "crabbox-*.devbox.namespace.*"))
	if err != nil {
		return nil, err
	}
	return namespaceFilterCleanupFiles(matches), nil
}

func namespaceFilterCleanupFiles(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "crabbox-") {
			continue
		}
		if strings.HasSuffix(base, ".devbox.namespace.ssh") || strings.HasSuffix(base, ".devbox.namespace.key") {
			out = append(out, path)
		}
	}
	return out
}

func namespaceSSHHost(name string) string {
	name = strings.TrimSpace(name)
	if strings.HasSuffix(name, ".devbox.namespace") {
		return name
	}
	return name + ".devbox.namespace"
}

func expandHomePath(path string) string {
	path = strings.Trim(path, "\"'")
	if path == "~" {
		return os.Getenv("HOME")
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(os.Getenv("HOME"), path[2:])
	}
	return path
}

func namespaceServer(name, leaseID, slug string, cfg Config, keep bool) Server {
	labels := directLeaseLabels(cfg, leaseID, slug, namespaceProvider, "", keep, time.Now().UTC())
	labels["name"] = name
	labels["target"] = targetLinux
	labels["state"] = "starting"
	server := Server{
		CloudID:  name,
		Provider: namespaceProvider,
		Name:     name,
		Status:   "starting",
		Labels:   labels,
	}
	server.ServerType.Name = namespaceSize(cfg)
	return server
}

func namespaceItemToServer(item namespaceListItem, cfg Config) Server {
	name := blank(item.Name, item.ID)
	slug := namespaceSlugFromName(name)
	leaseID := namespaceLeaseIDFromName(name)
	labels := directLeaseLabels(cfg, leaseID, slug, namespaceProvider, "", true, time.Now().UTC())
	labels["name"] = name
	labels["state"] = blank(item.Status, "unknown")
	if item.Repository != "" {
		labels["repo"] = item.Repository
	}
	if item.Created != "" {
		labels["created"] = item.Created
	}
	server := Server{
		CloudID:  name,
		Provider: namespaceProvider,
		Name:     name,
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = blank(item.Size, namespaceSize(cfg))
	return server
}

var crabboxNamespaceNamePattern = regexp.MustCompile(`^crabbox-(.+)-[0-9a-f]{8}$`)

func namespaceSlugFromName(name string) string {
	if match := crabboxNamespaceNamePattern.FindStringSubmatch(strings.TrimSpace(name)); len(match) == 2 {
		return normalizeLeaseSlug(match[1])
	}
	return normalizeLeaseSlug(name)
}

func namespaceLeaseIDFromName(name string) string {
	slug := normalizeLeaseSlug(name)
	if slug == "" {
		slug = "devbox"
	}
	if len(slug) > 80 {
		slug = slug[:80]
	}
	return "nsd_" + slug
}

func resolveNamespaceDevboxName(identifier string, reclaim bool) (string, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", "", exit(2, "provider=%s requires --id <devbox-name-or-slug>", namespaceProvider)
	}
	if claim, ok, err := resolveLeaseClaim(identifier); err != nil {
		return "", "", "", err
	} else if ok {
		if claim.Provider != "" && claim.Provider != namespaceProvider {
			return "", "", "", exit(4, "%q is claimed by provider %s", identifier, claim.Provider)
		}
		_ = reclaim
		slug := blank(claim.Slug, newLeaseSlug(claim.LeaseID))
		if strings.HasPrefix(claim.LeaseID, "nsd_") {
			return slug, claim.LeaseID, slug, nil
		}
		return leaseProviderName(claim.LeaseID, slug), claim.LeaseID, slug, nil
	}
	if strings.HasPrefix(identifier, "cbx_") {
		slug := newLeaseSlug(identifier)
		return leaseProviderName(identifier, slug), identifier, slug, nil
	}
	slug := normalizeLeaseSlug(identifier)
	return identifier, namespaceLeaseIDFromName(identifier), slug, nil
}

func namespaceImage(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Namespace.Image), "builtin:base")
}

func namespaceSize(cfg Config) string {
	if strings.TrimSpace(cfg.Namespace.Size) != "" {
		if size := namespaceValidSize(strings.TrimSpace(cfg.Namespace.Size)); size != "" {
			return size
		}
	}
	if strings.TrimSpace(cfg.ServerType) != "" {
		if size := namespaceValidSize(strings.TrimSpace(cfg.ServerType)); size != "" {
			return size
		}
	}
	return "M"
}

func namespaceValidSize(value string) string {
	size := strings.ToUpper(strings.TrimSpace(value))
	switch size {
	case "S", "M", "L", "XL":
		return size
	default:
		return ""
	}
}

func namespaceWorkRoot(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Namespace.WorkRoot), "/workspaces/crabbox")
}

func namespaceAutoStopIdleTimeout(cfg Config) time.Duration {
	if cfg.Namespace.AutoStopIdleTimeout > 0 {
		return cfg.Namespace.AutoStopIdleTimeout
	}
	if cfg.IdleTimeout > 0 {
		return cfg.IdleTimeout
	}
	return 30 * time.Minute
}

func extractJSONObject(output string) string {
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end >= start {
		return output[start : end+1]
	}
	return output
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

func parseNamespaceList(output string) ([]namespaceListItem, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" || strings.HasPrefix(trimmed, "No devbox available yet.") {
		return nil, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(extractJSONValue(output)), &raw); err != nil {
		return nil, exit(5, "namespace devbox list returned invalid JSON: %v", err)
	}
	values := []any{}
	switch typed := raw.(type) {
	case []any:
		values = typed
	case map[string]any:
		for _, key := range []string{"devboxes", "items", "instances"} {
			if list, ok := typed[key].([]any); ok {
				values = list
				break
			}
		}
	}
	items := make([]namespaceListItem, 0, len(values))
	for _, value := range values {
		obj, ok := value.(map[string]any)
		if !ok {
			continue
		}
		item := namespaceListItem{
			Name:       firstJSONText(obj, "name", "display_name"),
			ID:         firstJSONText(obj, "id", "devbox_id"),
			Status:     firstJSONText(obj, "status", "state"),
			Size:       firstJSONText(obj, "size", "machine_size"),
			Repository: firstJSONText(obj, "repository", "repo"),
			Created:    firstJSONText(obj, "created", "created_at", "createdAt"),
		}
		if item.Name == "" && item.ID == "" {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

func firstJSONText(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := obj[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		case fmt.Stringer:
			if text := strings.TrimSpace(value.String()); text != "" {
				return text
			}
		case float64:
			return fmt.Sprintf("%.0f", value)
		}
	}
	return ""
}
