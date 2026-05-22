package localcontainer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "local-container"
	sshPort      = "2222"
)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

type inspectContainer struct {
	ID              string            `json:"Id"`
	Name            string            `json:"Name"`
	Created         string            `json:"Created"`
	Config          inspectConfig     `json:"Config"`
	State           inspectState      `json:"State"`
	NetworkSettings inspectNetworking `json:"NetworkSettings"`
}

type inspectConfig struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

type inspectState struct {
	Status  string `json:"Status"`
	Running bool   `json:"Running"`
}

type inspectNetworking struct {
	Ports map[string][]inspectPort `json:"Ports"`
}

type inspectPort struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	applyDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	leaseID := core.NewLeaseID()
	containers, err := b.listContainers(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	servers := make([]core.Server, 0, len(containers))
	for _, container := range containers {
		servers = append(servers, b.serverFromContainer(container, cfg))
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	name := core.LeaseProviderName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s runtime=%s image=%s keep=%v\n", providerName, leaseID, slug, cfg.LocalContainer.Runtime, cfg.LocalContainer.Image, req.Keep)
	containerID, err := b.createContainer(ctx, cfg, name, leaseID, slug, publicKey, req.Keep)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	container, err := b.inspectContainer(ctx, containerID)
	if err != nil {
		if !req.Keep {
			_ = b.removeContainer(context.Background(), containerID)
		}
		return core.LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, cfg, container, leaseID, slug, true)
	if err != nil {
		if !req.Keep {
			_ = b.removeContainer(context.Background(), containerID)
		}
		return core.LeaseTarget{}, err
	}
	if err := core.ClaimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			_ = b.removeContainer(context.Background(), containerID)
		}
		return core.LeaseTarget{}, err
	}
	cleanupKey = false
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s container=%s state=ready\n", leaseID, shortID(container.ID))
	return lease, nil
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	container, leaseID, slug, err := b.resolveContainer(ctx, req.ID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return core.LeaseTarget{Server: b.serverFromContainer(container, cfg), LeaseID: leaseID}, nil
	}
	lease, err := b.prepareLease(ctx, cfg, container, leaseID, slug, false)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		if err := core.ClaimLeaseForRepoProvider(leaseID, slug, providerName, req.Repo.Root, cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	cfg := b.configForRun()
	containers, err := b.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]core.LeaseView, 0, len(containers))
	for _, container := range containers {
		servers = append(servers, b.serverFromContainer(container, cfg))
	}
	return servers, nil
}

func (b *backend) Doctor(ctx context.Context, req core.DoctorRequest) (core.DoctorResult, error) {
	runtime, contextName := b.runtimeInfo(ctx)
	containers, err := b.listContainers(ctx)
	if err != nil {
		return core.DoctorResult{}, err
	}
	probe := "unchecked"
	if req.ProbeSSH {
		probe = "requires_running_lease"
	}
	cfg := b.configForRun()
	msg := fmt.Sprintf("cli=ready control_plane=local inventory=ready api=list mutation=false leases=%d runtime=%s context=%s ssh_probe=%s image=%s docker_socket=%v", len(containers), runtime, blank(contextName, "-"), probe, cfg.LocalContainer.Image, cfg.LocalContainer.DockerSocket)
	return core.DoctorResult{Provider: providerName, Message: msg}, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	id := strings.TrimSpace(req.Lease.Server.CloudID)
	if id == "" {
		container, _, _, err := b.resolveContainer(ctx, req.Lease.LeaseID)
		if err != nil {
			return err
		}
		id = container.ID
	}
	if id == "" {
		return core.Exit(2, "provider=%s release requires a container id", providerName)
	}
	if err := b.removeContainer(ctx, id); err != nil {
		return err
	}
	core.RemoveLeaseClaim(req.Lease.LeaseID)
	core.RemoveStoredTestboxKey(req.Lease.LeaseID)
	return nil
}

func (b *backend) Touch(_ context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.configForRun(), req.State, time.Now().UTC())
	return server, nil
}

func (b *backend) configForRun() core.Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func applyDefaults(cfg *core.Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.TargetOS == core.TargetLinux {
		cfg.WindowsMode = ""
	}
	cfg.SSHFallbackPorts = []string{}
	if cfg.LocalContainer.Runtime == "" {
		cfg.LocalContainer.Runtime = "docker"
	}
	if cfg.LocalContainer.Image == "" {
		cfg.LocalContainer.Image = "debian:bookworm"
	}
	if cfg.LocalContainer.User == "" {
		cfg.LocalContainer.User = "crabbox"
	}
	if cfg.LocalContainer.DockerSocket && isDefaultWorkRoot(cfg.LocalContainer.WorkRoot) && isDefaultWorkRoot(cfg.WorkRoot) {
		cfg.LocalContainer.WorkRoot = defaultDockerSocketWorkRoot()
	}
	if cfg.LocalContainer.WorkRoot == "" {
		if !isDefaultWorkRoot(cfg.WorkRoot) {
			cfg.LocalContainer.WorkRoot = cfg.WorkRoot
		} else {
			cfg.LocalContainer.WorkRoot = "/work/crabbox"
		}
	}
	if cfg.LocalContainer.Network == "" {
		cfg.LocalContainer.Network = "bridge"
	}
	cfg.SSHUser = cfg.LocalContainer.User
	cfg.SSHPort = sshPort
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	cfg.ServerType = cfg.LocalContainer.Image
}

func (b *backend) createContainer(ctx context.Context, cfg core.Config, name, leaseID, slug, publicKey string, keep bool) (string, error) {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, time.Now().UTC())
	labels["runtime"] = cfg.LocalContainer.Runtime
	labels["image"] = cfg.LocalContainer.Image
	labels["ssh_user"] = cfg.LocalContainer.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.LocalContainer.WorkRoot
	labels["docker_socket"] = boolEnv(cfg.LocalContainer.DockerSocket)
	args := []string{
		"run", "-d",
		"--name", name,
		"--hostname", name,
		"--user", "root",
		"--network", cfg.LocalContainer.Network,
		"-p", "127.0.0.1::" + sshPort,
		"-e", "CRABBOX_AUTHORIZED_KEY=" + publicKey,
		"-e", "CRABBOX_SSH_USER=" + cfg.LocalContainer.User,
		"-e", "CRABBOX_WORK_ROOT=" + cfg.LocalContainer.WorkRoot,
		"-e", "CRABBOX_SSH_PORT=" + sshPort,
		"-e", "CRABBOX_DESKTOP=" + boolEnv(cfg.Desktop),
		"-e", "CRABBOX_BROWSER=" + boolEnv(cfg.Browser),
		"-e", "CRABBOX_DOCKER_SOCKET=" + boolEnv(cfg.LocalContainer.DockerSocket),
	}
	for key, value := range labels {
		args = append(args, "--label", key+"="+value)
	}
	if cfg.LocalContainer.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(cfg.LocalContainer.CPUs))
	}
	if memory := strings.TrimSpace(cfg.LocalContainer.Memory); memory != "" {
		args = append(args, "--memory", memory)
	}
	if cfg.LocalContainer.DockerSocket {
		if err := os.MkdirAll(cfg.LocalContainer.WorkRoot, 0o755); err != nil {
			return "", core.Exit(2, "create local-container host work root %s: %v", cfg.LocalContainer.WorkRoot, err)
		}
		leaseWorkRoot := filepath.Join(cfg.LocalContainer.WorkRoot, leaseID)
		if err := os.MkdirAll(leaseWorkRoot, 0o777); err != nil {
			return "", core.Exit(2, "create local-container host lease work root %s: %v", leaseWorkRoot, err)
		}
		if err := os.Chmod(leaseWorkRoot, 0o777); err != nil {
			return "", core.Exit(2, "make local-container host lease work root writable %s: %v", leaseWorkRoot, err)
		}
		args = append(args, "-v", cfg.LocalContainer.WorkRoot+":"+cfg.LocalContainer.WorkRoot)
		socketPath, err := b.dockerSocketMountPath(ctx)
		if err != nil {
			return "", err
		}
		args = append(args, "-v", socketPath+":/var/run/docker.sock")
	}
	args = append(args, cfg.LocalContainer.Image, "/bin/sh", "-lc", bootstrapScript)
	result, err := b.docker(ctx, args, nil, b.rt.Stderr)
	if err != nil {
		return "", commandError("container run", result, err)
	}
	id := strings.TrimSpace(result.Stdout)
	if id == "" {
		return "", core.Exit(2, "%s run did not return a container id", cfg.LocalContainer.Runtime)
	}
	return id, nil
}

func (b *backend) dockerSocketMountPath(ctx context.Context) (string, error) {
	if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); host != "" {
		return dockerSocketMountPathFromHost(host)
	}
	if result, err := b.docker(ctx, []string{"context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"}, nil, nil); err == nil {
		host := strings.TrimSpace(result.Stdout)
		if host != "" && host != "<no value>" {
			var decoded string
			if err := json.Unmarshal([]byte(host), &decoded); err == nil {
				host = decoded
			}
			return dockerSocketMountPathFromHost(host)
		}
	}
	return validateDockerSocketMountPath("/var/run/docker.sock")
}

func dockerSocketMountPathFromHost(host string) (string, error) {
	path, ok := localDockerSocketPath(host)
	if !ok {
		return "", core.Exit(2, "local-container docker socket requested but active Docker host %q is not a local Unix socket", host)
	}
	return validateDockerSocketMountPath(path)
}

func localDockerSocketPath(host string) (string, bool) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", false
	}
	if strings.HasPrefix(host, "/") {
		return host, true
	}
	if strings.HasPrefix(host, "unix://") {
		u, err := url.Parse(host)
		if err == nil && u.Path != "" {
			return u.Path, true
		}
		path := strings.TrimPrefix(host, "unix://")
		if strings.HasPrefix(path, "/") {
			return path, true
		}
	}
	return "", false
}

func validateDockerSocketMountPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", core.Exit(2, "local-container docker socket requested but %s is not available: %v", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", core.Exit(2, "local-container docker socket requested but %s is not a socket", path)
	}
	return path, nil
}

func defaultDockerSocketWorkRoot() string {
	if cache, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cache) != "" {
		return filepath.Join(cache, "crabbox", "local-container-work")
	}
	return filepath.Join(os.TempDir(), "crabbox-local-container-work")
}

func (b *backend) prepareLease(ctx context.Context, cfg core.Config, container inspectContainer, leaseID, slug string, wait bool) (core.LeaseTarget, error) {
	server := b.serverFromContainer(container, cfg)
	if user := strings.TrimSpace(server.Labels["ssh_user"]); user != "" {
		cfg.LocalContainer.User = user
		cfg.SSHUser = user
	}
	if root := strings.TrimSpace(server.Labels["work_root"]); root != "" {
		cfg.LocalContainer.WorkRoot = root
		cfg.WorkRoot = root
	}
	host, port, err := containerSSHHostPort(container)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			cfg.SSHKey = keyPath
		}
	}
	target := core.SSHTargetFromConfig(cfg, host)
	target.Port = port
	target.ReadyCheck = localContainerReadyCheck(cfg)
	if wait {
		if err := core.WaitForSSHReady(ctx, &target, b.rt.Stderr, "local container ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		server.Status = "ready"
		server.Labels["state"] = "ready"
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *backend) listContainers(ctx context.Context) ([]inspectContainer, error) {
	result, err := b.docker(ctx, []string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=" + providerName, "--format", "{{.ID}}"}, nil, nil)
	if err != nil {
		return nil, commandError("container list", result, err)
	}
	ids := strings.Fields(result.Stdout)
	containers := make([]inspectContainer, 0, len(ids))
	for _, id := range ids {
		container, err := b.inspectContainer(ctx, id)
		if err != nil {
			return nil, err
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func (b *backend) inspectContainer(ctx context.Context, id string) (inspectContainer, error) {
	result, err := b.docker(ctx, []string{"inspect", id}, nil, nil)
	if err != nil {
		return inspectContainer{}, commandError("container inspect", result, err)
	}
	var containers []inspectContainer
	if err := json.Unmarshal([]byte(result.Stdout), &containers); err != nil {
		return inspectContainer{}, core.Exit(2, "parse container inspect for %s: %v", id, err)
	}
	if len(containers) == 0 {
		return inspectContainer{}, core.Exit(4, "container not found: %s", id)
	}
	return containers[0], nil
}

func (b *backend) resolveContainer(ctx context.Context, identifier string) (inspectContainer, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return inspectContainer{}, "", "", core.Exit(2, "provider=%s requires --id <lease-id-or-slug-or-container>", providerName)
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName); err != nil {
		return inspectContainer{}, "", "", err
	} else if ok {
		return b.findContainerForClaim(ctx, claim)
	}
	containers, err := b.listContainers(ctx)
	if err != nil {
		return inspectContainer{}, "", "", err
	}
	normalized := core.NormalizeLeaseSlug(identifier)
	for _, container := range containers {
		labels := container.Config.Labels
		leaseID := labels["lease"]
		slug := labels["slug"]
		name := strings.TrimPrefix(container.Name, "/")
		if container.ID == identifier || shortID(container.ID) == identifier || name == identifier || leaseID == identifier || (normalized != "" && core.NormalizeLeaseSlug(slug) == normalized) {
			return container, leaseID, slug, nil
		}
	}
	return inspectContainer{}, "", "", core.Exit(4, "local-container lease not found: %s", identifier)
}

func (b *backend) findContainerForClaim(ctx context.Context, claim core.LeaseClaim) (inspectContainer, string, string, error) {
	containers, err := b.listContainers(ctx)
	if err != nil {
		return inspectContainer{}, "", "", err
	}
	for _, container := range containers {
		labels := container.Config.Labels
		if labels["lease"] == claim.LeaseID {
			return container, labels["lease"], labels["slug"], nil
		}
	}
	for _, container := range containers {
		labels := container.Config.Labels
		if claim.Slug != "" && core.NormalizeLeaseSlug(labels["slug"]) == core.NormalizeLeaseSlug(claim.Slug) {
			return container, labels["lease"], labels["slug"], nil
		}
	}
	return inspectContainer{}, "", "", core.Exit(4, "local-container lease not found: %s", firstNonBlank(claim.Slug, claim.LeaseID))
}

func (b *backend) removeContainer(ctx context.Context, id string) error {
	result, err := b.docker(ctx, []string{"rm", "-f", id}, nil, b.rt.Stderr)
	if err != nil {
		return commandError("container remove", result, err)
	}
	return nil
}

func (b *backend) runtimeInfo(ctx context.Context) (string, string) {
	version, err := b.docker(ctx, []string{"version", "--format", "{{.Client.Version}}"}, nil, nil)
	if err != nil {
		return "unknown", ""
	}
	contextName, _ := b.docker(ctx, []string{"context", "show"}, nil, nil)
	return strings.TrimSpace(version.Stdout), strings.TrimSpace(contextName.Stdout)
}

func (b *backend) docker(ctx context.Context, args []string, stdout, stderr io.Writer) (core.LocalCommandResult, error) {
	cfg := b.configForRun()
	return b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:   cfg.LocalContainer.Runtime,
		Args:   args,
		Stdout: stdout,
		Stderr: stderr,
	})
}

func (b *backend) serverFromContainer(container inspectContainer, cfg core.Config) core.Server {
	labels := map[string]string{}
	for key, value := range container.Config.Labels {
		labels[key] = value
	}
	labels["container_id"] = shortID(container.ID)
	if labels["provider"] == "" {
		labels["provider"] = providerName
	}
	if labels["server_type"] == "" {
		labels["server_type"] = container.Config.Image
	}
	if labels["state"] == "" {
		labels["state"] = container.State.Status
	}
	host, port, _ := containerSSHHostPort(container)
	if port != "" {
		labels["ssh_port"] = port
	}
	server := core.Server{
		CloudID:  container.ID,
		Provider: providerName,
		Name:     strings.TrimPrefix(container.Name, "/"),
		Status:   container.State.Status,
		Labels:   labels,
	}
	if container.State.Running && labels["state"] == "ready" {
		server.Status = "ready"
	}
	server.PublicNet.IPv4.IP = host
	server.ServerType.Name = firstNonBlank(labels["server_type"], cfg.LocalContainer.Image)
	return server
}

func containerSSHHostPort(container inspectContainer) (string, string, error) {
	ports := container.NetworkSettings.Ports[sshPort+"/tcp"]
	if len(ports) == 0 {
		return "", "", core.Exit(4, "container %s has no published SSH port", shortID(container.ID))
	}
	host := strings.TrimSpace(ports[0].HostIP)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return host, strings.TrimSpace(ports[0].HostPort), nil
}

func commandError(action string, result core.LocalCommandResult, err error) error {
	code := result.ExitCode
	if code == 0 {
		code = 1
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail != "" {
		return core.Exit(code, "%s failed: %v: %s", action, err, detail)
	}
	return core.Exit(code, "%s failed: %v", action, err)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func blank(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func boolEnv(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func localContainerReadyCheck(cfg core.Config) string {
	checks := []string{
		"git --version >/tmp/crabbox-ready.log 2>&1",
		"rsync --version >>/tmp/crabbox-ready.log 2>&1",
		"python3 --version >>/tmp/crabbox-ready.log 2>&1",
		"test -d " + shellQuote(cfg.LocalContainer.WorkRoot),
	}
	if cfg.Desktop {
		checks = append(checks,
			"pgrep -f 'Xvfb :99' >/dev/null",
			"pgrep -f 'x11vnc.*-rfbport 5900' >/dev/null",
			"ss -ltn | grep -q '127.0.0.1:5900'",
			"test -s /var/lib/crabbox/vnc.password",
		)
	}
	if cfg.Browser {
		checks = append(checks,
			"test -s /var/lib/crabbox/browser.env",
			". /var/lib/crabbox/browser.env",
			"test -x \"$BROWSER\"",
			"\"$BROWSER\" --version >>/tmp/crabbox-ready.log 2>&1",
		)
	}
	return strings.Join(checks, " && ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func isDefaultWorkRoot(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || value == "/work/crabbox"
}

const bootstrapScript = `
set -eu
export DEBIAN_FRONTEND=noninteractive
need_install=0
	for tool in /usr/sbin/sshd git rsync curl sudo python3; do
	  command -v "$tool" >/dev/null 2>&1 || need_install=1
	done
	if [ "$need_install" = "1" ] && command -v apt-get >/dev/null 2>&1; then
	  apt-get update
	  apt-get install -y --no-install-recommends openssh-server ca-certificates git rsync curl sudo python3
	fi
if [ "${CRABBOX_DESKTOP:-0}" = "1" ] && command -v apt-get >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends xvfb xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-terminal xfconf xfce4-settings x11vnc xauth dbus-x11 x11-xserver-utils xterm scrot ffmpeg xdotool wmctrl xclip xsel fonts-dejavu-core fonts-liberation iproute2 openssl procps netcat-openbsd novnc websockify
fi
if [ "${CRABBOX_BROWSER:-0}" = "1" ] && command -v apt-get >/dev/null 2>&1; then
  apt-get update
  if apt-cache show chromium >/dev/null 2>&1; then
    apt-get install -y --no-install-recommends chromium || true
  fi
  if ! command -v chromium >/dev/null 2>&1 || ! chromium --version >/dev/null 2>&1; then
    rm -f /usr/local/bin/crabbox-browser
    if apt-cache show firefox-esr >/dev/null 2>&1; then
      apt-get install -y --no-install-recommends firefox-esr || true
    fi
  fi
  if ! command -v chromium >/dev/null 2>&1 && ! command -v firefox-esr >/dev/null 2>&1 && apt-cache show firefox >/dev/null 2>&1; then
    apt-get install -y --no-install-recommends firefox || true
  fi
fi
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && ! command -v docker >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends docker.io
fi
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && ! command -v docker >/dev/null 2>&1; then
  echo "docker socket requested but docker CLI is not installed; use a Debian/Ubuntu-compatible image or preinstall docker" >&2
  exit 127
fi
if ! command -v /usr/sbin/sshd >/dev/null 2>&1; then
  echo "missing /usr/sbin/sshd; use a Debian/Ubuntu-compatible image or a prebuilt Crabbox runner image" >&2
  exit 127
fi
user="${CRABBOX_SSH_USER:-crabbox}"
work_root="${CRABBOX_WORK_ROOT:-/work/crabbox}"
ssh_port="${CRABBOX_SSH_PORT:-2222}"
if ! id "$user" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$user"
fi
home_dir="$(getent passwd "$user" | cut -d: -f6)"
if [ -z "$home_dir" ]; then
  home_dir="/home/$user"
fi
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && [ -S /var/run/docker.sock ]; then
  socket_gid="$(stat -c '%g' /var/run/docker.sock 2>/dev/null || true)"
  if [ -n "$socket_gid" ]; then
    socket_group="$(getent group "$socket_gid" | cut -d: -f1 || true)"
    if [ -z "$socket_group" ]; then
      socket_group="crabbox-docker"
      groupadd -g "$socket_gid" "$socket_group" 2>/dev/null || socket_group=""
    fi
    if [ -n "$socket_group" ]; then
      usermod -aG "$socket_group" "$user" || true
    fi
  fi
fi
mkdir -p /run/sshd "$work_root" "$home_dir/.ssh"
printf '%s\n' "$CRABBOX_AUTHORIZED_KEY" > "$home_dir/.ssh/authorized_keys"
chmod 700 "$home_dir/.ssh"
chmod 600 "$home_dir/.ssh/authorized_keys"
if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ]; then
  chown -R "$user" "$home_dir/.ssh"
else
  chown -R "$user" "$home_dir/.ssh" "$work_root"
fi
if command -v sudo >/dev/null 2>&1; then
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$user" > /etc/sudoers.d/crabbox
  chmod 440 /etc/sudoers.d/crabbox
fi
if [ "${CRABBOX_DESKTOP:-0}" = "1" ]; then
  install -d -m 0750 -o "$user" /var/lib/crabbox
  if [ ! -s /var/lib/crabbox/vnc.password ]; then
    (umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)
  fi
  x11vnc -storepasswd "$(cat /var/lib/crabbox/vnc.password)" /var/lib/crabbox/vnc.pass >/dev/null
  chown "$user" /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
  chmod 0600 /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass
  cat >/usr/local/bin/crabbox-start-desktop <<'DESKTOP'
#!/bin/sh
set -eu
user="${CRABBOX_SSH_USER:-crabbox}"
runtime="/tmp/crabbox-runtime-$user"
install -d -m 0700 -o "$user" "$runtime"
if ! pgrep -u "$user" -f 'Xvfb :99' >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "XDG_RUNTIME_DIR='$runtime' Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/crabbox-xvfb.log 2>&1 &"
fi
sleep 1
if ! pgrep -u "$user" -f 'xfce4-session|startxfce4' >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' dbus-launch startxfce4 >/tmp/crabbox-desktop.log 2>&1 &"
fi
sleep 2
if command -v xfce4-terminal >/dev/null 2>&1 && ! pgrep -u "$user" -f 'xfce4-terminal.*Crabbox Desktop' >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xfce4-terminal --title='Crabbox Desktop' --geometry=110x32+48+48 >/tmp/crabbox-terminal.log 2>&1 &" || true
elif command -v xterm >/dev/null 2>&1 && ! pgrep -u "$user" -f 'xterm -title Crabbox Desktop' >/dev/null 2>&1; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' xterm -title 'Crabbox Desktop' -geometry 110x32+48+48 -bg '#111827' -fg '#e5e7eb' >/tmp/crabbox-terminal.log 2>&1 &" || true
fi
if ! ss -ltn | grep -q '127.0.0.1:5900'; then
  su "$user" -s /bin/sh -c "DISPLAY=:99 XDG_RUNTIME_DIR='$runtime' x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -rfbauth /var/lib/crabbox/vnc.pass -o /tmp/crabbox-x11vnc.log >/tmp/crabbox-x11vnc.stdout.log 2>&1 &"
fi
DESKTOP
  chmod 0755 /usr/local/bin/crabbox-start-desktop
  CRABBOX_SSH_USER="$user" /usr/local/bin/crabbox-start-desktop
fi
if [ "${CRABBOX_BROWSER:-0}" = "1" ]; then
  browser_path=""
  for candidate in google-chrome chromium firefox-esr firefox; do
    if candidate_path="$(command -v "$candidate" 2>/dev/null)" && "$candidate_path" --version >/dev/null 2>&1; then
      browser_path="$candidate_path"
      break
    fi
  done
  if [ -z "$browser_path" ]; then
    echo "browser requested but no supported browser package is available for this image architecture" >&2
    exit 127
  fi
  browser_wrapper=/usr/local/bin/crabbox-browser
  case "$(basename "$browser_path")" in
    firefox*|iceweasel*)
      printf '%s\n' '#!/bin/sh' "exec \"$browser_path\" --width 1500 --height 900 \"\$@\"" > "$browser_wrapper"
      ;;
    *)
      printf '%s\n' '#!/bin/sh' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$browser_wrapper"
      ;;
  esac
  chmod 0755 "$browser_wrapper"
  install -d -m 0755 /var/lib/crabbox
  printf 'CHROME_BIN=%s\nBROWSER=%s\n' "$browser_wrapper" "$browser_wrapper" > /var/lib/crabbox/browser.env
  chown "$user" /var/lib/crabbox/browser.env
  chmod 0644 /var/lib/crabbox/browser.env
fi
exec /usr/sbin/sshd -D -e -p "$ssh_port"
`
