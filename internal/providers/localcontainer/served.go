package localcontainer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ServedContainerOptions selects the outbound agent's stricter container policy.
// The ordinary local-container path never supplies these options.
type ServedContainerOptions struct {
	Labels       map[string]string
	Deadline     int64
	runtimeImage string
}

// The watchdog lives inside PID 1's container namespace; --rm removes the container
// even after the agent exits or loses coordinator connectivity. Only root refreshes it.
const servedWatchdog = `
trap 'exit 0' TERM
umask 077
printf '%s\n' "$CRABBOX_SERVE_DEADLINE" > /run/crabbox-serve-deadline
(
 while :; do
  deadline=$(cat /run/crabbox-serve-deadline 2>/dev/null) || break
  case "$deadline" in ''|*[!0-9]*) break;; esac
  [ "$(date +%s)" -lt "$deadline" ] || break
  sleep 1
 done
 kill -TERM 1
 sleep 1
 kill -KILL -1
) &
umask 022
# Never inherit host keys from a configured or prepared image.
rm -f /etc/ssh/ssh_host_*
if [ ! -f /etc/ssh/ssh_host_ed25519_key ] && command -v ssh-keygen >/dev/null 2>&1; then
 ssh-keygen -A || exit 1
fi
`

type ServedRuntime struct {
	imageMu         sync.RWMutex
	baseImageID     string
	b               *backend
	preparedImageID string
	baseImage       servedImage
	preparationCtx  context.Context
	preparationDone chan struct{}
	preparing       bool
}
type ServedContainer struct {
	ID, LeaseID string
	Labels      map[string]string
	Running     bool
	Host, Port  string
}

func NewServedRuntime(cfg core.Config, rt core.Runtime) *ServedRuntime {
	cfg.LocalContainer.Network = "bridge"
	cfg.LocalContainer.User = "crabbox"
	cfg.LocalContainer.WorkRoot = "/work/crabbox"
	cfg.WorkRoot = "/work/crabbox"
	cfg.LocalContainer.DockerSocket = false
	cfg.LocalContainer.Volumes = nil
	cfg.Desktop = false
	cfg.Browser = false
	cfg.Code = false
	b := newBackend(Provider{}.Spec(), cfg, rt).(*backend)
	return &ServedRuntime{b: b}
}
func (r *ServedRuntime) List(ctx context.Context) ([]ServedContainer, error) {
	containers, err := r.b.listContainersForProvider(ctx, "serve")
	if err != nil {
		return nil, err
	}
	out := make([]ServedContainer, 0, len(containers))
	for _, c := range containers {
		host, port, _ := containerSSHHostPort(c)
		out = append(out, ServedContainer{ID: c.ID, LeaseID: c.Config.Labels["lease"], Labels: c.Config.Labels, Running: c.State.Running, Host: host, Port: port})
	}
	return out, nil
}
func (r *ServedRuntime) Create(ctx context.Context, id, key string, cpus, memoryMB int, image string, volumes []core.CacheVolumeConfig, options ServedContainerOptions) (ServedContainer, error) {
	cfg := r.b.cfg
	cfg.LocalContainer.CPUs = cpus
	cfg.LocalContainer.Memory = strconv.Itoa(memoryMB) + "m"
	cfg.Cache.Volumes = volumes
	if image != r.Image() {
		return ServedContainer{}, fmt.Errorf("serve_image_mismatch")
	}
	r.imageMu.RLock()
	options.runtimeImage = r.preparedImageID
	r.imageMu.RUnlock()
	idResult, _, err := r.b.createContainerWithOptions(ctx, cfg, "crabbox-serve-"+id, id, "", key, "", false, &options)
	if err != nil && options.runtimeImage != "" && r.preparedImageMissing(ctx, options.runtimeImage) {
		r.imageMu.Lock()
		if r.preparedImageID == options.runtimeImage {
			r.preparedImageID = ""
		}
		preparationCtx := r.preparationCtx
		r.imageMu.Unlock()
		// Other agents can prune this shared image while we are idle. This lease
		// keeps the configured image and bootstraps normally while we rebuild.
		options.runtimeImage = ""
		if preparationCtx != nil {
			r.StartPreparation(preparationCtx)
		}
		idResult, _, err = r.b.createContainerWithOptions(ctx, cfg, "crabbox-serve-"+id, id, "", key, "", false, &options)
	}
	if err != nil {
		return ServedContainer{}, err
	}
	c, err := r.b.inspectContainer(ctx, idResult)
	if err != nil {
		return ServedContainer{}, err
	}
	host, port, err := containerSSHHostPort(c)
	return ServedContainer{ID: c.ID, LeaseID: id, Labels: c.Config.Labels, Running: c.State.Running, Host: host, Port: port}, err
}
func (r *ServedRuntime) Remove(ctx context.Context, c ServedContainer, machineID string) error {
	current, err := r.b.inspectContainer(ctx, c.ID)
	if err != nil {
		return err
	}
	if current.Config.Labels["crabbox.serve"] != "true" || current.Config.Labels["crabbox.serve.machine"] != machineID {
		return fmt.Errorf("refusing to remove foreign container")
	}
	return r.b.removeContainer(ctx, c.ID)
}
func (r *ServedRuntime) Refresh(ctx context.Context, c ServedContainer, machineID string, deadline int64) error {
	current, err := r.b.inspectContainer(ctx, c.ID)
	if err != nil {
		return err
	}
	if current.Config.Labels["crabbox.serve.machine"] != machineID || current.Config.Labels["crabbox.serve"] != "true" {
		return fmt.Errorf("refusing to refresh foreign container")
	}
	_, err = r.b.docker(ctx, []string{"exec", "--user", "root", c.ID, "sh", "-c", "umask 077; printf '%s\\n' " + strconv.FormatInt(deadline, 10) + " > /run/crabbox-serve-deadline.new && mv /run/crabbox-serve-deadline.new /run/crabbox-serve-deadline"}, nil, nil)
	return err
}
func (r *ServedRuntime) HostKey(ctx context.Context, c ServedContainer) (string, error) {
	result, err := r.b.docker(ctx, []string{"exec", c.ID, "cat", "/etc/ssh/ssh_host_ed25519_key.pub"}, nil, nil)
	return strings.TrimSpace(result.Stdout), err
}
func (r *ServedRuntime) Version(ctx context.Context) (string, error) {
	result, err := r.b.docker(ctx, []string{"version", "--format", "{{.Server.Version}}"}, nil, nil)
	return strings.TrimSpace(result.Stdout), err
}
func (r *ServedRuntime) PruneCaches(ctx context.Context, volumes []core.CacheVolumeConfig) error {
	mounts, err := localContainerCacheVolumeMounts(volumes)
	if err != nil {
		return err
	}
	var volumeErr error
	for _, mount := range mounts {
		name, _, _ := strings.Cut(mount, ":")
		if _, err := r.b.docker(ctx, []string{"volume", "rm", name}, nil, r.b.rt.Stderr); err != nil {
			volumeErr = errors.Join(volumeErr, err)
		}
	}
	return errors.Join(volumeErr, r.prunePreparedImages(ctx))
}

func (r *ServedRuntime) prunePreparedImages(ctx context.Context) error {
	result, err := r.b.docker(ctx, []string{"image", "ls", "--filter", "label=crabbox.serve.prepared=true", "--format", "{{.ID}}", "--no-trunc"}, nil, nil)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(result.Stdout) {
		used, err := r.b.docker(ctx, []string{"ps", "-q", "--filter", "ancestor=" + id}, nil, nil)
		if err != nil {
			return err
		}
		if strings.TrimSpace(used.Stdout) != "" {
			continue
		}
		// No force: the runtime also protects containers created after the check.
		if _, err := r.b.docker(ctx, []string{"image", "rm", id}, nil, r.b.rt.Stderr); err != nil {
			return err
		}
	}
	return nil
}

func (r *ServedRuntime) Image() string { return r.b.cfg.LocalContainer.Image }

func (r *ServedRuntime) Evidence(ctx context.Context, c ServedContainer) (*core.ImageEvidence, error) {
	observed, err := r.b.inspectContainer(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	// Repository digests describe the configured base; the container records the
	// actual runtime image. Only this runtime's prepared ID maps to its base.
	runtimeID := observed.Image
	r.imageMu.RLock()
	if r.preparedImageID != "" && runtimeID == r.preparedImageID {
		observed.Image = r.baseImageID
	}
	r.imageMu.RUnlock()
	evidence, err := r.b.observeImageEvidence(ctx, r.b.cfg, observed, core.LeaseClaim{})
	if err == nil {
		evidence.ConfiguredReference = r.Image()
		evidence.RuntimeImageID = runtimeID
	}
	return evidence, err
}

func ServedReadyCheck() string {
	return localContainerReadyCheck(core.Config{LocalContainer: core.LocalContainerConfig{WorkRoot: "/work/crabbox"}})
}
