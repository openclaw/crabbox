package localcontainer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	checkpointMetadataRuntime  = "runtime"
	checkpointMetadataHost     = "docker_host"
	checkpointMetadataContext  = "docker_context"
	checkpointMetadataConfig   = "docker_config"
	checkpointMetadataEndpoint = "docker_endpoint"
	checkpointMetadataDaemonID = "docker_daemon_id"
	checkpointMetadataForkID   = "fork_image_id"
	checkpointMetadataForkName = "fork_image_name"
	checkpointMetadataUser     = "container_user"
	checkpointMetadataWorkRoot = "container_work_root"
)

var checkpointScopeMetadataKeys = []string{
	checkpointMetadataRuntime,
	checkpointMetadataHost,
	checkpointMetadataContext,
	checkpointMetadataConfig,
	checkpointMetadataEndpoint,
	checkpointMetadataDaemonID,
}

type checkpointScope struct {
	Runtime  string
	Host     string
	Context  string
	Config   string
	Endpoint string
	DaemonID string
}

type checkpointMount struct {
	Destination string `json:"Destination"`
}

var checkpointLeaseLabelKeys = []string{
	"bootstrap_dir",
	"browser",
	"class",
	"code",
	"container_id",
	"crabbox",
	"crabbox_exposed_ports",
	"created_at",
	"created_by",
	"desktop",
	"desktop_env",
	checkpointMetadataConfig,
	checkpointMetadataContext,
	checkpointMetadataDaemonID,
	checkpointMetadataEndpoint,
	checkpointMetadataHost,
	"docker_socket",
	"expires_at",
	"host_work_root",
	"idle_timeout",
	"idle_timeout_secs",
	"image",
	"keep",
	"last_touched_at",
	"lease",
	"market",
	"pond",
	"profile",
	"provider",
	"provider_key",
	"runtime",
	"runtime_context",
	"server_type",
	"slug",
	"ssh_port",
	"ssh_user",
	"state",
	"tailscale",
	"tailscale_exit_node",
	"tailscale_exit_node_allow_lan_access",
	"tailscale_hostname",
	"tailscale_state",
	"tailscale_tags",
	"target",
	"ttl_secs",
	"windows_mode",
	"work_root",
}

func (Provider) NativeCheckpointWorkdir(req core.NativeCheckpointWorkdirRequest) string {
	if override := strings.TrimSpace(req.Override); override != "" {
		return override
	}
	cfg := req.Config
	if workRoot := strings.TrimSpace(req.Server.Labels["work_root"]); workRoot != "" {
		cfg.WorkRoot = workRoot
	}
	return core.RemoteJoin(cfg, req.LeaseID, req.RepoName)
}

func (Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (core.NativeCheckpointCreateResult, error) {
	containerID := strings.TrimSpace(req.Server.CloudID)
	if containerID == "" {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "docker-commit checkpoint requires a running container")
	}
	scope, err := checkpointScopeForServer(ctx, req.Config, req.Server)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	if !isDockerRuntime(scope.Runtime) {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "docker-commit checkpoints require the Docker runtime; use --mode archive with %s", scope.Runtime)
	}
	metadata := checkpointMetadataForServer(scope, req.Config, req.Server)
	canonicalWorkdir, err := checkpointCanonicalWorkdir(ctx, scope, containerID, req.Workdir)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	mounts, err := checkpointMounts(ctx, scope, containerID)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	if destination := checkpointMountIntersectingWorkdir(mounts, canonicalWorkdir); destination != "" {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "docker-commit checkpoint cannot capture workdir %s because it intersects mounted volume %s; use --mode archive", req.Workdir, destination)
	}

	var commitStderr strings.Builder
	commitCmd := checkpointCommand(
		ctx,
		scope,
		"commit",
		"--change", checkpointResetLeaseLabelsChange(),
		"--change", checkpointBootableCommandChange(),
		containerID,
	)
	commitCmd.Stderr = &commitStderr
	out, err := commitCmd.Output()
	if err != nil {
		return core.NativeCheckpointCreateResult{}, core.Exit(7, "docker commit %s: %v: %s", containerID, err, trimCheckpointFailure(commitStderr.String()))
	}
	imageID, err := parseCheckpointImageID(string(out))
	if err != nil {
		return core.NativeCheckpointCreateResult{}, core.Exit(7, "docker commit %s: %v", containerID, err)
	}
	imageName := checkpointImageName(defaultCheckpointName(req), imageID)
	if tagOut, err := checkpointCommand(ctx, scope, "tag", imageID, imageName).CombinedOutput(); err != nil {
		cleanupCtx, cancel := checkpointRollbackContext()
		cleanupOut, cleanupErr := checkpointCommand(cleanupCtx, scope, "rmi", imageID).CombinedOutput()
		cancel()
		detail := trimCheckpointFailure(string(tagOut))
		if cleanupErr != nil {
			detail += fmt.Sprintf("; cleanup failed: %v: %s", cleanupErr, trimCheckpointFailure(string(cleanupOut)))
			return core.NativeCheckpointCreateResult{
				Image: core.NativeCheckpointImage{
					ID:       imageID,
					State:    "cleanup_failed",
					Provider: providerName,
					Kind:     core.CheckpointKindDockerCommit,
					Direct:   true,
				},
				Metadata: metadata,
			}, core.Exit(7, "docker tag %s: %v: %s", imageID, err, detail)
		}
		return core.NativeCheckpointCreateResult{}, core.Exit(7, "docker tag %s: %v: %s", imageID, err, detail)
	}
	return core.NativeCheckpointCreateResult{
		Image: core.NativeCheckpointImage{
			ID:       imageID,
			Name:     imageName,
			State:    "available",
			Provider: providerName,
			Kind:     core.CheckpointKindDockerCommit,
			Direct:   true,
		},
		Metadata: metadata,
	}, nil
}

func checkpointRollbackContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), rollbackTimeout)
}

func (Provider) VerifyNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	scope := checkpointScopeFromRequest(req)
	if err := validateCheckpointScope(ctx, scope); err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	currentID, missing, err := inspectCheckpointTag(ctx, scope, req.Image)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	if missing {
		return core.NativeCheckpointVerifyResult{ProviderState: "missing", NextAction: "delete_local"}, nil
	}
	if !strings.EqualFold(currentID, req.Image.ID) {
		return core.NativeCheckpointVerifyResult{
			ProviderState: "conflict",
			NextAction:    "restore_tag_or_delete_local",
			Error:         fmt.Sprintf("checkpoint tag %s points to %s, recorded image is %s", req.Image.Name, currentID, req.Image.ID),
		}, nil
	}
	return core.NativeCheckpointVerifyResult{ProviderState: "available", NextAction: "delete"}, nil
}

func (Provider) DeleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	scope := checkpointScopeFromRequest(req)
	if err := validateCheckpointScope(ctx, scope); err != nil {
		return err
	}
	currentID, missing, err := inspectCheckpointTag(ctx, scope, req.Image)
	if err != nil {
		return err
	}
	if missing {
		return nil
	}
	if !strings.EqualFold(currentID, req.Image.ID) {
		return core.Exit(7, "refusing to delete checkpoint tag %s: recorded image %s but tag now points to %s; restore the tag or use --local-only", req.Image.Name, req.Image.ID, currentID)
	}
	target := firstCheckpointValue(req.Image.Name, req.Image.ID)
	args := []string{"rmi"}
	if req.Image.Name != "" {
		args = append(args, "-f")
	}
	args = append(args, target)
	if out, err := checkpointCommand(ctx, scope, args...).CombinedOutput(); err != nil {
		return core.Exit(7, "docker rmi %s: %v: %s", target, err, trimCheckpointFailure(string(out)))
	}
	return nil
}

func inspectCheckpointTag(ctx context.Context, scope checkpointScope, image core.NativeCheckpointImage) (string, bool, error) {
	target := firstCheckpointValue(image.Name, image.ID)
	var stderr strings.Builder
	cmd := checkpointCommand(ctx, scope, "image", "inspect", target, "--format", "{{.Id}}")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := trimCheckpointFailure(stderr.String())
		if checkpointInspectReportsMissing(detail) {
			return "", true, nil
		}
		return "", false, core.Exit(7, "docker image inspect %s: %v: %s", target, err, detail)
	}
	imageID, err := parseCheckpointImageID(string(out))
	if err != nil {
		return "", false, core.Exit(7, "docker image inspect %s: %v", target, err)
	}
	return imageID, false, nil
}

func checkpointScopeForServer(ctx context.Context, cfg core.Config, server core.Server) (checkpointScope, error) {
	runtimeName := leaseRuntime(server, cfg.LocalContainer.Runtime)
	if metadata := checkpointScopeMetadataFromLabels(server.Labels); len(metadata) != 0 {
		scope := checkpointScopeFromMetadata(metadata, runtimeName)
		if err := validateCheckpointScope(ctx, scope); err != nil {
			return checkpointScope{}, err
		}
		return scope, nil
	}
	scope := checkpointScope{Runtime: runtimeName}
	if !isDockerRuntime(runtimeName) {
		return scope, nil
	}
	if contextName := strings.TrimSpace(server.Labels["runtime_context"]); contextName != "" {
		scope.Context = contextName
	} else if contextName := strings.TrimSpace(os.Getenv("DOCKER_CONTEXT")); contextName != "" {
		scope.Context = contextName
	} else if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); host != "" {
		scope.Host = host
		scope.Endpoint = host
	} else {
		configPath, err := checkpointConfigPath()
		if err != nil {
			return checkpointScope{}, err
		}
		scope.Config = configPath
		var stderr strings.Builder
		cmd := exec.CommandContext(ctx, runtimeName, "context", "show")
		cmd.Env = checkpointEnvForScope(scope)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return checkpointScope{}, core.Exit(7, "resolve Docker context: %v: %s", err, trimCheckpointFailure(stderr.String()))
		}
		scope.Context = strings.TrimSpace(string(out))
		if scope.Context == "" {
			return checkpointScope{}, core.Exit(7, "resolve Docker context: command returned an empty context")
		}
	}
	if scope.Config == "" && scope.Host == "" {
		configPath, err := checkpointConfigPath()
		if err != nil {
			return checkpointScope{}, err
		}
		scope.Config = configPath
	}
	if scope.Endpoint == "" {
		endpoint, err := checkpointContextEndpoint(ctx, scope)
		if err != nil {
			return checkpointScope{}, err
		}
		scope.Endpoint = endpoint
	}
	daemonID, err := checkpointDaemonID(ctx, scope)
	if err != nil {
		return checkpointScope{}, err
	}
	scope.DaemonID = daemonID
	return scope, nil
}

func checkpointScopeFromRequest(req core.NativeCheckpointResourceRequest) checkpointScope {
	return checkpointScopeFromMetadata(req.Metadata, req.Config.LocalContainer.Runtime)
}

func checkpointScopeFromMetadata(metadata map[string]string, fallbackRuntime string) checkpointScope {
	runtimeName := strings.TrimSpace(metadata[checkpointMetadataRuntime])
	if runtimeName == "" {
		runtimeName = firstCheckpointValue(fallbackRuntime, "docker")
	}
	return checkpointScope{
		Runtime:  runtimeName,
		Host:     strings.TrimSpace(metadata[checkpointMetadataHost]),
		Context:  strings.TrimSpace(metadata[checkpointMetadataContext]),
		Config:   strings.TrimSpace(metadata[checkpointMetadataConfig]),
		Endpoint: strings.TrimSpace(metadata[checkpointMetadataEndpoint]),
		DaemonID: strings.TrimSpace(metadata[checkpointMetadataDaemonID]),
	}
}

func checkpointForkMetadata(metadata map[string]string, image core.NativeCheckpointForkRecord) map[string]string {
	out := make(map[string]string, len(metadata)+2)
	for key, value := range metadata {
		out[key] = value
	}
	out[checkpointMetadataForkID] = strings.TrimSpace(image.ImageID)
	out[checkpointMetadataForkName] = strings.TrimSpace(firstCheckpointValue(image.Name, image.Resource))
	return out
}

func checkpointScopeMetadata(scope checkpointScope) map[string]string {
	return map[string]string{
		checkpointMetadataRuntime:  scope.Runtime,
		checkpointMetadataHost:     scope.Host,
		checkpointMetadataContext:  scope.Context,
		checkpointMetadataConfig:   scope.Config,
		checkpointMetadataEndpoint: scope.Endpoint,
		checkpointMetadataDaemonID: scope.DaemonID,
	}
}

func checkpointMetadataForServer(scope checkpointScope, cfg core.Config, server core.Server) map[string]string {
	metadata := checkpointScopeMetadata(scope)
	metadata[checkpointMetadataUser] = firstCheckpointValue(server.Labels["ssh_user"], cfg.LocalContainer.User, cfg.SSHUser)
	metadata[checkpointMetadataWorkRoot] = firstCheckpointValue(server.Labels["work_root"], cfg.LocalContainer.WorkRoot, cfg.WorkRoot)
	return metadata
}

func checkpointScopeMetadataFromLabels(labels map[string]string) map[string]string {
	if strings.TrimSpace(labels[checkpointMetadataDaemonID]) == "" {
		return nil
	}
	metadata := make(map[string]string, len(checkpointScopeMetadataKeys))
	for _, key := range checkpointScopeMetadataKeys {
		if value := strings.TrimSpace(labels[key]); value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

func validateCheckpointScope(ctx context.Context, scope checkpointScope) error {
	if scope.Context != "" && scope.Endpoint != "" {
		current, err := checkpointContextEndpoint(ctx, scope)
		if err != nil {
			return err
		}
		if current != scope.Endpoint {
			return core.Exit(7, "Docker context %s endpoint changed from %s to %s; refusing checkpoint operation", scope.Context, scope.Endpoint, current)
		}
	}
	if scope.DaemonID == "" {
		return core.Exit(7, "checkpoint is missing its Docker daemon identity; refusing checkpoint operation")
	}
	currentDaemonID, err := checkpointDaemonID(ctx, scope)
	if err != nil {
		return err
	}
	if currentDaemonID != scope.DaemonID {
		return core.Exit(7, "Docker daemon changed from %s to %s; refusing checkpoint operation", scope.DaemonID, currentDaemonID)
	}
	return nil
}

func validateCheckpointFork(ctx context.Context, cfg core.Config) error {
	metadata := cfg.LocalContainer.CheckpointMetadata
	if len(metadata) == 0 {
		return nil
	}
	scope := checkpointScopeFromMetadata(metadata, cfg.LocalContainer.Runtime)
	if err := validateCheckpointScope(ctx, scope); err != nil {
		return err
	}
	image := core.NativeCheckpointImage{
		ID:   strings.TrimSpace(metadata[checkpointMetadataForkID]),
		Name: strings.TrimSpace(metadata[checkpointMetadataForkName]),
	}
	if image.ID == "" || image.Name == "" {
		return core.Exit(7, "local-container checkpoint fork is missing its recorded image identity")
	}
	currentID, missing, err := inspectCheckpointTag(ctx, scope, image)
	if err != nil {
		return err
	}
	if missing {
		return core.Exit(7, "local-container checkpoint image %s is missing", image.Name)
	}
	if !strings.EqualFold(currentID, image.ID) {
		return core.Exit(7, "refusing to fork checkpoint tag %s: recorded image %s but tag now points to %s", image.Name, image.ID, currentID)
	}
	return nil
}

func checkpointContextEndpoint(ctx context.Context, scope checkpointScope) (string, error) {
	var stderr strings.Builder
	cmd := checkpointCommand(ctx, scope, "context", "inspect", scope.Context, "--format", `{{(index .Endpoints "docker").Host}}`)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", core.Exit(7, "resolve Docker context %s endpoint: %v: %s", scope.Context, err, trimCheckpointFailure(stderr.String()))
	}
	endpoint := strings.TrimSpace(string(out))
	if endpoint == "" {
		return "", core.Exit(7, "resolve Docker context %s endpoint: command returned an empty endpoint", scope.Context)
	}
	return endpoint, nil
}

func checkpointDaemonID(ctx context.Context, scope checkpointScope) (string, error) {
	var stderr strings.Builder
	cmd := checkpointCommand(ctx, scope, "info", "--format", "{{.ID}}")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", core.Exit(7, "resolve Docker daemon identity: %v: %s", err, trimCheckpointFailure(stderr.String()))
	}
	daemonID := strings.TrimSpace(string(out))
	if daemonID == "" {
		return "", core.Exit(7, "resolve Docker daemon identity: command returned an empty id")
	}
	return daemonID, nil
}

func checkpointConfigPath() (string, error) {
	configPath := strings.TrimSpace(os.Getenv("DOCKER_CONFIG"))
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", core.Exit(7, "resolve Docker config directory: %v", err)
		}
		configPath = filepath.Join(home, ".docker")
	}
	if strings.HasPrefix(configPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", core.Exit(7, "resolve Docker config directory: %v", err)
		}
		configPath = filepath.Join(home, strings.TrimPrefix(configPath, "~/"))
	}
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return "", core.Exit(7, "resolve Docker config directory %s: %v", configPath, err)
	}
	return filepath.Clean(absolute), nil
}

func checkpointCommand(ctx context.Context, scope checkpointScope, args ...string) *exec.Cmd {
	full := args
	if scope.Context != "" {
		full = append([]string{"--context", scope.Context}, args...)
	}
	cmd := exec.CommandContext(ctx, scope.Runtime, full...)
	if env := checkpointEnvForScope(scope); env != nil {
		cmd.Env = env
	}
	return cmd
}

func checkpointEnvForScope(scope checkpointScope) []string {
	if scope.Config == "" && scope.Host == "" && scope.Context == "" {
		return nil
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+2)
	for _, value := range base {
		if strings.HasPrefix(value, "DOCKER_CONTEXT=") ||
			(scope.Host != "" || scope.Context != "") && strings.HasPrefix(value, "DOCKER_HOST=") ||
			scope.Config != "" && strings.HasPrefix(value, "DOCKER_CONFIG=") {
			continue
		}
		out = append(out, value)
	}
	if scope.Config != "" {
		out = append(out, "DOCKER_CONFIG="+scope.Config)
	}
	if scope.Host != "" {
		out = append(out, "DOCKER_HOST="+scope.Host)
	}
	return out
}

func checkpointCanonicalWorkdir(ctx context.Context, scope checkpointScope, containerID, workdir string) (string, error) {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return "", core.Exit(2, "docker-commit checkpoint requires a workspace path")
	}
	out, err := checkpointCommand(ctx, scope, "exec", containerID, "sh", "-c", `cd "$1" && pwd -P`, "sh", workdir).CombinedOutput()
	if err != nil {
		return "", core.Exit(7, "resolve docker workdir %s: %v: %s", workdir, err, trimCheckpointFailure(string(out)))
	}
	canonical := strings.TrimSpace(string(out))
	if !strings.HasPrefix(canonical, "/") {
		return "", core.Exit(7, "resolve docker workdir %s: expected an absolute path, got %q", workdir, canonical)
	}
	return path.Clean(canonical), nil
}

func checkpointMounts(ctx context.Context, scope checkpointScope, containerID string) ([]checkpointMount, error) {
	out, err := checkpointCommand(ctx, scope, "inspect", containerID, "--format", "{{json .Mounts}}").CombinedOutput()
	if err != nil {
		return nil, core.Exit(7, "docker inspect %s mounts: %v: %s", containerID, err, trimCheckpointFailure(string(out)))
	}
	var mounts []checkpointMount
	if err := json.Unmarshal(out, &mounts); err != nil {
		return nil, core.Exit(7, "decode docker mounts for %s: %v", containerID, err)
	}
	return mounts, nil
}

func checkpointMountIntersectingWorkdir(mounts []checkpointMount, workdir string) string {
	workdir = path.Clean(strings.TrimSpace(workdir))
	if workdir == "." {
		return ""
	}
	for _, mount := range mounts {
		destination := path.Clean(strings.TrimSpace(mount.Destination))
		if destination == "." {
			continue
		}
		if destination == "/" || workdir == "/" || workdir == destination ||
			strings.HasPrefix(workdir, destination+"/") ||
			strings.HasPrefix(destination, workdir+"/") {
			return destination
		}
	}
	return ""
}

func checkpointResetLeaseLabelsChange() string {
	var change strings.Builder
	change.WriteString("LABEL")
	for _, key := range checkpointLeaseLabelKeys {
		fmt.Fprintf(&change, ` %s=""`, key)
	}
	return change.String()
}

func checkpointBootableCommandChange() string {
	return `CMD ["/bin/sh","-c","while :; do sleep 3600; done"]`
}

func checkpointImageName(name, imageID string) string {
	shortID := strings.TrimPrefix(strings.ToLower(imageID), "sha256:")
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	const prefix = "crabbox-checkpoint-"
	const maxRepositoryLength = 255
	maxNameLength := maxRepositoryLength - len(prefix) - 1 - len(shortID)
	return prefix + normalizeCheckpointRepositoryComponent(name, maxNameLength) + "-" + shortID
}

func normalizeCheckpointRepositoryComponent(value string, maxLength int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	out.Grow(min(len(value), maxLength))
	separator := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			required := 1
			if separator && out.Len() > 0 {
				required++
			}
			if out.Len()+required > maxLength {
				break
			}
			if separator && out.Len() > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(r)
			separator = false
			continue
		}
		separator = true
	}
	normalized := strings.Trim(out.String(), "-")
	if normalized == "" {
		return "checkpoint"
	}
	return normalized
}

func parseCheckpointImageID(output string) (string, error) {
	imageID := ""
	for _, field := range strings.Fields(output) {
		if !validCheckpointImageID(field) {
			continue
		}
		if imageID != "" && !strings.EqualFold(imageID, field) {
			return "", fmt.Errorf("ambiguous image ids in output %q", strings.TrimSpace(output))
		}
		imageID = strings.ToLower(field)
	}
	if imageID == "" {
		return "", fmt.Errorf("missing sha256 image id in output %q", strings.TrimSpace(output))
	}
	return imageID, nil
}

func validCheckpointImageID(value string) bool {
	digest := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "sha256:")
	if len(digest) != 64 {
		return false
	}
	for _, r := range digest {
		if r < '0' || r > '9' && r < 'a' || r > 'f' {
			return false
		}
	}
	return true
}

func checkpointInspectReportsMissing(output string) bool {
	value := strings.ToLower(output)
	return strings.Contains(value, "no such image") ||
		strings.Contains(value, "no such object") ||
		strings.Contains(value, "image not found") ||
		strings.Contains(value, "reference does not exist")
}

func defaultCheckpointName(req core.NativeCheckpointCreateRequest) string {
	return firstCheckpointValue(req.Name, req.RepoName, req.LeaseID, "checkpoint")
}

func firstCheckpointValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func trimCheckpointFailure(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	const max = 500
	if len(value) > max {
		return value[:max] + "..."
	}
	return value
}
