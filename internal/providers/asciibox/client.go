package asciibox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type api interface {
	Check(context.Context) error
	CreateBox(context.Context, createRequest) (boxData, error)
	PrepareSSH(context.Context, string) error
	GetBox(context.Context, string) (boxData, error)
	ListBoxes(context.Context) ([]boxData, error)
	ReleaseBox(context.Context, string) error
}

type client struct {
	apiKey  string
	apiURL  string
	cliPath string
	home    string
	runner  CommandRunner
}

type createRequest struct {
	TTL time.Duration
}

type boxData struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	State        string `json:"state,omitempty"`
	Status       string `json:"status,omitempty"`
	MachineIP    string `json:"machineIp,omitempty"`
	MachineIPAlt string `json:"machine_ip,omitempty"`
	PublicIP     string `json:"publicIp,omitempty"`
	IP           string `json:"ip,omitempty"`
	SSHUser      string `json:"sshUser,omitempty"`
	SSHUserAlt   string `json:"ssh_user,omitempty"`
	URL          string `json:"url,omitempty"`
	DesktopURL   string `json:"desktopUrl,omitempty"`
	ArchiveAfter any    `json:"archiveAfter,omitempty"`
	ExpiresAt    any    `json:"expiresAt,omitempty"`
	CreatedAt    any    `json:"createdAt,omitempty"`
	UpdatedAt    any    `json:"updatedAt,omitempty"`
}

var newAPI = func(cfg Config, rt Runtime) (api, error) {
	apiKey := strings.TrimSpace(cfg.AsciiBox.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=%s requires ASCII_BOX_API_KEY", providerName)
	}
	if rt.Exec == nil {
		return nil, exit(2, "provider=%s requires a local command runner", providerName)
	}
	cliPath := strings.TrimSpace(cfg.AsciiBox.CLIPath)
	if cliPath == "" {
		cliPath = "box"
	}
	apiURL := strings.TrimRight(blank(strings.TrimSpace(cfg.AsciiBox.BaseURL), "https://ascii.dev"), "/")
	return &client{apiKey: apiKey, apiURL: apiURL, cliPath: cliPath, home: asciiBoxCLIHome(), runner: rt.Exec}, nil
}

func (c *client) CreateBox(ctx context.Context, req createRequest) (boxData, error) {
	args := []string{"new"}
	if req.TTL > 0 {
		args = append(args, "--ttl", fmt.Sprintf("%d", int(req.TTL.Round(time.Second).Seconds())))
	}
	result, err := c.run(ctx, args...)
	if err != nil {
		partial, parseErr := decodeNewBox(result.Stdout)
		if partial.ID != "" {
			if parseErr != nil {
				return partial, fmt.Errorf("ascii-box CLI new failed after creating %s: %s", partial.ID, c.formatError(result, err))
			}
			if ready, waitErr := c.waitForBoxReady(ctx, partial); waitErr == nil {
				return ready, nil
			}
			return partial, fmt.Errorf("ascii-box CLI new failed after creating %s: %s", partial.ID, c.formatError(result, err))
		}
		return boxData{}, fmt.Errorf("ascii-box CLI new failed: %s", c.formatError(result, err))
	}
	box, err := decodeNewBox(result.Stdout)
	if err != nil {
		return box, err
	}
	if strings.TrimSpace(box.ID) == "" {
		return boxData{}, fmt.Errorf("ascii-box CLI new response missing box id")
	}
	if !boxReadyForSSH(box) {
		if ready, err := c.waitForBoxReady(ctx, box); err == nil {
			return ready, nil
		}
	}
	return box, nil
}

func (c *client) Check(ctx context.Context) error {
	result, err := c.run(ctx, "limits")
	if err != nil {
		return fmt.Errorf("ascii-box CLI limits failed: %s", c.formatError(result, err))
	}
	return nil
}

func (c *client) PrepareSSH(ctx context.Context, id string) error {
	result, err := c.runWithEnv(ctx, c.sshEnv(), "ssh", id, "--", "true")
	if err != nil {
		return fmt.Errorf("ascii-box CLI ssh setup failed: %s", c.formatError(result, err))
	}
	return nil
}

func (c *client) GetBox(ctx context.Context, id string) (boxData, error) {
	result, err := c.run(ctx, "info", id)
	if err != nil {
		return boxData{}, fmt.Errorf("ascii-box CLI info failed: %s", c.formatError(result, err))
	}
	return decodeBox([]byte(result.Stdout))
}

func (c *client) ListBoxes(ctx context.Context) ([]boxData, error) {
	result, err := c.run(ctx, "list")
	if err != nil {
		return nil, fmt.Errorf("ascii-box CLI list failed: %s", c.formatError(result, err))
	}
	return decodeBoxes([]byte(result.Stdout))
}

func (c *client) ReleaseBox(ctx context.Context, id string) error {
	stopResult, stopErr := c.run(ctx, "stop", id)
	deleteResult, deleteErr := c.run(ctx, "delete", id)
	if deleteErr != nil {
		if stopErr != nil {
			return fmt.Errorf("ascii-box CLI release failed: stop: %s; delete: %s", c.formatError(stopResult, stopErr), c.formatError(deleteResult, deleteErr))
		}
		return fmt.Errorf("ascii-box CLI delete failed: %s", c.formatError(deleteResult, deleteErr))
	}
	return nil
}

func (c *client) run(ctx context.Context, args ...string) (LocalCommandResult, error) {
	return c.runWithEnv(ctx, c.env(), args...)
}

func (c *client) runWithEnv(ctx context.Context, env []string, args ...string) (LocalCommandResult, error) {
	if err := c.ensureConfig(ctx); err != nil {
		return LocalCommandResult{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cliTimeout(args))
		defer cancel()
	}
	argv := []string{"--no-update", "--json"}
	if c.apiURL != "" {
		argv = append(argv, "--api-url", c.apiURL)
	}
	argv = append(argv, args...)
	return c.runner.Run(ctx, LocalCommandRequest{
		Name: c.cliPath,
		Args: argv,
		Env:  env,
	})
}

func cliTimeout(args []string) time.Duration {
	if len(args) == 0 {
		return 30 * time.Second
	}
	switch args[0] {
	case "new":
		return 5 * time.Minute
	case "ssh", "delete", "stop":
		return 2 * time.Minute
	default:
		return 30 * time.Second
	}
}

func (c *client) ensureConfig(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	result, err := c.runner.Run(ctx, LocalCommandRequest{
		Name: c.cliPath,
		Args: []string{"--no-update", "--json", "config"},
		Env:  c.env(),
	})
	if err != nil {
		return fmt.Errorf("ascii-box CLI config failed: %s", c.formatError(result, err))
	}
	var cfg struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &cfg); err != nil {
		return fmt.Errorf("decode ascii-box CLI config: %w", err)
	}
	configPath := strings.TrimSpace(cfg.Path)
	if configPath == "" {
		return fmt.Errorf("ascii-box CLI config response missing path")
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(map[string]string{
		"api_url": c.apiURL,
		"token":   c.apiKey,
		"channel": "prod",
	}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writePrivateFileAtomic(configPath, data); err != nil {
		return err
	}
	return nil
}

func writePrivateFileAtomic(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to overwrite symlink config file %s", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to overwrite non-regular config file %s", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	keep = true
	return nil
}

func (c *client) waitForBoxReady(ctx context.Context, box boxData) (boxData, error) {
	latest := box
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		if boxReadyForSSH(latest) {
			return latest, nil
		}
		if refreshed, err := c.GetBox(ctx, latest.ID); err == nil {
			latest = mergeBox(latest, refreshed)
			if boxReadyForSSH(latest) {
				return latest, nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return latest, ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return latest, fmt.Errorf("timed out waiting for ascii-box %s to become ready: %w", latest.ID, lastErr)
			}
			return latest, fmt.Errorf("timed out waiting for ascii-box %s to become ready", latest.ID)
		case <-ticker.C:
		}
	}
}

func (c *client) env() []string {
	return setEnv(setEnv(os.Environ(), "HOME", c.home), "BOX_API_KEY", c.apiKey)
}

func (c *client) sshEnv() []string {
	return setEnv(c.env(), "SSH_AUTH_SOCK", "")
}

func (c *client) formatError(result LocalCommandResult, err error) string {
	message := strings.TrimSpace(result.Stderr)
	if message == "" {
		message = strings.TrimSpace(result.Stdout)
	}
	if message == "" && err != nil {
		message = err.Error()
	}
	return redactBoxSecrets(blank(message, "unknown error"))
}

var (
	boxTokenParamRE = regexp.MustCompile(`(?i)([?&](?:box_token|token|access_token|auth_token)=)[^&\s"']+`)
	boxSecretRE     = regexp.MustCompile(`box_[A-Za-z0-9_-]+`)
)

func redactBoxSecrets(value string) string {
	value = boxTokenParamRE.ReplaceAllString(value, "${1}REDACTED")
	return boxSecretRE.ReplaceAllString(value, "box_REDACTED")
}

func asciiBoxCLIHome() string {
	if configured := strings.TrimSpace(os.Getenv("CRABBOX_ASCII_BOX_HOME")); configured != "" {
		return expandUserPath(configured)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "crabbox", "ascii-box")
	}
	return filepath.Join(os.TempDir(), "crabbox-ascii-box")
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	set := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			out = append(out, prefix+value)
			set = true
			continue
		}
		out = append(out, entry)
	}
	if !set {
		out = append(out, prefix+value)
	}
	return out
}

func decodeNewBox(output string) (boxData, error) {
	var latest boxData
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event struct {
			Event string `json:"event"`
			boxData
			Data boxData `json:"data"`
			Box  boxData `json:"box"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return boxData{}, fmt.Errorf("decode ascii-box CLI new line: %w", err)
		}
		box := event.boxData
		if box.ID == "" {
			box = event.Data
		}
		if box.ID == "" {
			box = event.Box
		}
		if box.ID != "" {
			latest = mergeBox(latest, box)
		}
		if event.Event == "ready" && latest.ID != "" {
			return latest, nil
		}
		if event.Event == "error" {
			return latest, fmt.Errorf("ascii-box CLI new failed: %s", redactBoxSecrets(string(line)))
		}
	}
	if err := scanner.Err(); err != nil {
		return boxData{}, err
	}
	if latest.ID == "" {
		return boxData{}, fmt.Errorf("decode ascii-box CLI new: no box event")
	}
	return latest, nil
}

func mergeBox(base, update boxData) boxData {
	if update.ID != "" {
		base.ID = update.ID
	}
	if update.Name != "" {
		base.Name = update.Name
	}
	if update.State != "" {
		base.State = update.State
	}
	if update.Status != "" {
		base.Status = update.Status
	}
	if update.IP != "" {
		base.IP = update.IP
	}
	if update.MachineIP != "" {
		base.MachineIP = update.MachineIP
	}
	if update.MachineIPAlt != "" {
		base.MachineIPAlt = update.MachineIPAlt
	}
	if update.PublicIP != "" {
		base.PublicIP = update.PublicIP
	}
	if update.SSHUser != "" {
		base.SSHUser = update.SSHUser
	}
	if update.SSHUserAlt != "" {
		base.SSHUserAlt = update.SSHUserAlt
	}
	if update.URL != "" {
		base.URL = update.URL
	}
	if update.DesktopURL != "" {
		base.DesktopURL = update.DesktopURL
	}
	if update.ArchiveAfter != nil {
		base.ArchiveAfter = update.ArchiveAfter
	}
	if update.ExpiresAt != nil {
		base.ExpiresAt = update.ExpiresAt
	}
	if update.CreatedAt != nil {
		base.CreatedAt = update.CreatedAt
	}
	if update.UpdatedAt != nil {
		base.UpdatedAt = update.UpdatedAt
	}
	return base
}

func decodeBox(data []byte) (boxData, error) {
	var wrapped struct {
		Box boxData `json:"box"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && strings.TrimSpace(wrapped.Box.ID) != "" {
		return wrapped.Box, nil
	}
	var box boxData
	if err := json.Unmarshal(data, &box); err != nil {
		return boxData{}, fmt.Errorf("decode ascii-box box: %w", err)
	}
	return box, nil
}

func decodeBoxes(data []byte) ([]boxData, error) {
	var wrapped struct {
		Boxes []boxData `json:"boxes"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Boxes != nil {
		return wrapped.Boxes, nil
	}
	var boxes []boxData
	if err := json.Unmarshal(data, &boxes); err != nil {
		return nil, fmt.Errorf("decode ascii-box boxes: %w", err)
	}
	return boxes, nil
}
