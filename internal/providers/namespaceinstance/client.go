package namespaceinstance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type nscClient struct {
	path   string
	cfg    NamespaceInstanceConfig
	runner CommandRunner
}

func newNSCClient(cfg Config, rt Runtime) (*nscClient, error) {
	if rt.Exec == nil {
		return nil, exit(2, "provider=%s requires a local command runner for nsc", providerName)
	}
	return &nscClient{path: "nsc", cfg: cfg.NamespaceInstance, runner: rt.Exec}, nil
}

func (c *nscClient) CheckReadiness(ctx context.Context) (string, error) {
	if _, err := c.run(ctx, "--help"); err != nil {
		return "", fmt.Errorf("nsc CLI unavailable: %w", err)
	}
	if _, err := c.run(ctx, "auth", "check-login"); err != nil {
		return "", fmt.Errorf("nsc auth check-login failed: %w", err)
	}
	result, err := c.run(ctx, "list", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("nsc list readiness check failed: %w", err)
	}
	count, err := parseNSCListCount(result.Stdout)
	if err != nil {
		return "", fmt.Errorf("decode nsc list readiness output: %w", err)
	}
	return strconv.Itoa(count), nil
}

func (c *nscClient) CreateInstance(ctx context.Context, req createInstanceRequest) (namespaceInstance, error) {
	dir, err := os.MkdirTemp("", "crabbox-nsc-create-*")
	if err != nil {
		return namespaceInstance{}, exit(2, "create nsc temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	jsonPath := filepath.Join(dir, "instance.json")
	cidPath := filepath.Join(dir, "instance.cid")
	args := []string{
		"create",
		"--machine_type", req.MachineType,
		"--duration", req.Duration.String(),
		"--purpose", "Crabbox disposable SSH lease",
		"--ssh_key", req.PublicKeyPath,
		"--cidfile", cidPath,
		"--output_json_to", jsonPath,
		"-o", "json",
	}
	if req.Ephemeral {
		args = append(args, "--ephemeral")
	}
	if req.UniqueTag != "" {
		args = append(args, "--unique_tag", req.UniqueTag)
	}
	for _, volume := range req.Volumes {
		if strings.TrimSpace(volume) != "" {
			args = append(args, "--volume", strings.TrimSpace(volume))
		}
	}
	for key, value := range req.Labels {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			args = append(args, "--label", key+"="+value)
		}
	}
	result, err := c.run(ctx, args...)
	if err != nil {
		return namespaceInstance{}, err
	}
	instance, parseErr := parseNSCInstance(result.Stdout)
	if parseErr != nil || instance.ID == "" {
		if data, readErr := os.ReadFile(jsonPath); readErr == nil {
			instance, parseErr = parseNSCInstance(string(data))
		}
	}
	if instance.ID == "" {
		if data, readErr := os.ReadFile(cidPath); readErr == nil {
			instance.ID = strings.TrimSpace(string(data))
		}
	}
	if instance.ID == "" {
		if parseErr != nil {
			return namespaceInstance{}, fmt.Errorf("decode nsc create output: %w", parseErr)
		}
		return namespaceInstance{}, exit(2, "nsc create did not return an instance id")
	}
	return instance, nil
}

func (c *nscClient) DescribeInstance(ctx context.Context, id string) (namespaceInstance, error) {
	result, err := c.run(ctx, "describe", strings.TrimSpace(id), "-o", "json")
	if err != nil {
		if nscNotFoundError(err) {
			return namespaceInstance{}, err
		}
		return namespaceInstance{}, err
	}
	instance, err := parseNSCInstance(result.Stdout)
	if err != nil {
		return namespaceInstance{}, fmt.Errorf("decode nsc describe output: %w", err)
	}
	if instance.ID == "" {
		instance.ID = strings.TrimSpace(id)
	}
	return instance, nil
}

func (c *nscClient) ListInstances(ctx context.Context, all bool) ([]namespaceInstance, error) {
	args := []string{"list", "-o", "json"}
	if all {
		args = append(args, "--all")
	}
	result, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseNSCInstances(result.Stdout)
}

func (c *nscClient) DestroyInstance(ctx context.Context, id string) error {
	_, err := c.run(ctx, "destroy", strings.TrimSpace(id), "--force")
	if err != nil && nscNotFoundError(err) {
		return err
	}
	return err
}

func (c *nscClient) ExtendInstance(ctx context.Context, id string, minimum time.Duration) error {
	if minimum <= 0 {
		return nil
	}
	_, err := c.run(ctx, "extend", strings.TrimSpace(id), "--ensure_minimum", minimum.String())
	return err
}

func (c *nscClient) ResolveSSH(instance namespaceInstance, cfg Config, keyPath string) (SSHTarget, error) {
	target := sshTargetFromConfig(cfg, instance.SSHHost)
	target.Key = keyPath
	if instance.SSHUser != "" {
		target.User = instance.SSHUser
	}
	if instance.SSHPort != "" {
		target.Port = instance.SSHPort
	}
	if target.User == "" {
		target.User = "root"
	}
	if target.Port == "" {
		target.Port = "22"
	}
	if strings.TrimSpace(target.Host) == "" || strings.TrimSpace(target.User) == "" || strings.TrimSpace(target.Port) == "" || strings.TrimSpace(target.Key) == "" {
		return SSHTarget{}, exit(2, "plan_gap: nsc JSON did not expose a normal SSH target for namespace-instance; implement API-backed GetSSHConfig before enabling run/sync")
	}
	return target, nil
}

func (c *nscClient) run(ctx context.Context, args ...string) (LocalCommandResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = contextWithTimeout(ctx, commandTimeout())
		defer cancel()
	}
	argv := c.globalArgs()
	argv = append(argv, args...)
	result, err := c.runner.Run(ctx, LocalCommandRequest{Name: c.path, Args: argv})
	if err != nil {
		if len(args) > 0 && (args[0] == "destroy" || args[0] == "describe") && commandOutputLooksNotFound(result) {
			return result, exit(4, "nsc %s target not found", args[0])
		}
		return result, exit(commandExitCode(result), "nsc %s failed", safeCommand(args))
	}
	return result, nil
}

func (c *nscClient) globalArgs() []string {
	var args []string
	if value := strings.TrimSpace(c.cfg.Endpoint); value != "" {
		args = append(args, "--endpoint", value)
	}
	if value := strings.TrimSpace(c.cfg.Keychain); value != "" {
		args = append(args, "--keychain", value)
	}
	if value := strings.TrimSpace(c.cfg.Region); value != "" {
		args = append(args, "--region", value)
	}
	return args
}

func parseNSCListCount(out string) (int, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return 0, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(out), &items); err == nil {
		return len(items), nil
	}
	var wrapped struct {
		Instances []json.RawMessage `json:"instances"`
		Items     []json.RawMessage `json:"items"`
		Results   []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &wrapped); err != nil {
		return 0, err
	}
	switch {
	case wrapped.Instances != nil:
		return len(wrapped.Instances), nil
	case wrapped.Items != nil:
		return len(wrapped.Items), nil
	case wrapped.Results != nil:
		return len(wrapped.Results), nil
	default:
		return 0, nil
	}
}

func commandExitCode(result LocalCommandResult) int {
	if result.ExitCode != 0 {
		return result.ExitCode
	}
	return 1
}

func safeCommand(args []string) string {
	if len(args) == 0 {
		return "command"
	}
	switch args[0] {
	case "--help":
		return "--help"
	case "auth":
		return "auth check-login"
	case "list":
		return "list"
	case "create":
		return "create"
	case "describe":
		return "describe"
	case "destroy":
		return "destroy"
	case "extend":
		return "extend"
	default:
		return "command"
	}
}

type createInstanceRequest struct {
	MachineType   string
	Duration      time.Duration
	Ephemeral     bool
	PublicKeyPath string
	UniqueTag     string
	Labels        map[string]string
	Volumes       []string
}

type namespaceInstance struct {
	ID          string
	Name        string
	Status      string
	MachineType string
	Region      string
	Labels      map[string]string
	Deadline    string
	SSHHost     string
	SSHUser     string
	SSHPort     string
}

func parseNSCInstances(out string) ([]namespaceInstance, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, nil
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal([]byte(out), &rawItems); err != nil {
		var wrapped map[string]json.RawMessage
		if wrapErr := json.Unmarshal([]byte(out), &wrapped); wrapErr != nil {
			instance, instErr := parseNSCInstance(out)
			if instErr != nil {
				return nil, err
			}
			if instance.ID == "" {
				return nil, nil
			}
			return []namespaceInstance{instance}, nil
		}
		for _, key := range []string{"instances", "items", "results"} {
			if raw := wrapped[key]; len(raw) > 0 {
				if arrErr := json.Unmarshal(raw, &rawItems); arrErr != nil {
					return nil, arrErr
				}
				break
			}
		}
	}
	outItems := make([]namespaceInstance, 0, len(rawItems))
	for _, raw := range rawItems {
		instance, err := parseNSCInstance(string(raw))
		if err != nil {
			return nil, err
		}
		if instance.ID != "" {
			outItems = append(outItems, instance)
		}
	}
	return outItems, nil
}

func parseNSCInstance(out string) (namespaceInstance, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return namespaceInstance{}, nil
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(out), &root); err != nil {
		if looksLikeInstanceID(out) {
			return namespaceInstance{ID: out}, nil
		}
		return namespaceInstance{}, err
	}
	if nested := firstMap(root, "instance", "environment", "metadata", "result"); nested != nil {
		for k, v := range root {
			if _, ok := nested[k]; !ok {
				nested[k] = v
			}
		}
		root = nested
	}
	labels := stringMap(firstAny(root, "labels", "label"))
	sshMap := firstMap(root, "ssh", "sshConfig", "ssh_config", "sshTarget", "ssh_target")
	host := firstString(root, "sshHost", "ssh_host", "host", "hostname", "ip", "address", "endpoint")
	user := firstString(root, "sshUser", "ssh_user", "user", "username", "login")
	port := firstString(root, "sshPort", "ssh_port", "port")
	if sshMap != nil {
		host = firstNonEmpty(host, firstString(sshMap, "host", "hostname", "ip", "address", "endpoint"))
		user = firstNonEmpty(user, firstString(sshMap, "user", "username", "login"))
		port = firstNonEmpty(port, firstString(sshMap, "port", "sshPort", "ssh_port"))
	}
	return namespaceInstance{
		ID:          firstString(root, "id", "instanceId", "instance_id", "cid", "name"),
		Name:        firstString(root, "name", "purpose", "displayName", "display_name"),
		Status:      firstString(root, "status", "state", "phase"),
		MachineType: firstString(root, "machineType", "machine_type", "shape", "type"),
		Region:      firstString(root, "region", "location"),
		Labels:      labels,
		Deadline:    firstString(root, "deadline", "expiresAt", "expires_at", "expiration", "ttl"),
		SSHHost:     host,
		SSHUser:     user,
		SSHPort:     port,
	}, nil
}

func looksLikeInstanceID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.ContainsAny(value, " \t\r\n{}[]")
}

func firstAny(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			return value
		}
	}
	return nil
}

func firstMap(m map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := m[key].(map[string]any); ok {
			return value
		}
	}
	return nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := m[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case float64:
			if v == float64(int64(v)) {
				return strconv.FormatInt(int64(v), 10)
			}
			return strconv.FormatFloat(v, 'f', -1, 64)
		case json.Number:
			return v.String()
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stringMap(value any) map[string]string {
	out := map[string]string{}
	switch typed := value.(type) {
	case map[string]any:
		for k, v := range typed {
			if key := strings.TrimSpace(k); key != "" {
				out[key] = fmt.Sprint(v)
			}
		}
	case map[string]string:
		for k, v := range typed {
			if key := strings.TrimSpace(k); key != "" {
				out[key] = strings.TrimSpace(v)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nscNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}

func commandOutputLooksNotFound(result LocalCommandResult) bool {
	msg := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}
