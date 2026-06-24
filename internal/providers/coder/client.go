package coder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type coderClient struct {
	cliPath string
	runner  interface {
		Run(context.Context, LocalCommandRequest) (LocalCommandResult, error)
	}
	stdout io.Writer
	stderr io.Writer
}

func newCoderClient(cfg Config, rt Runtime) (*coderClient, error) {
	if rt.Exec == nil {
		return nil, exit(2, "coder provider requires command runner")
	}
	cliPath := strings.TrimSpace(cfg.Coder.CLIPath)
	if cliPath == "" {
		cliPath = "coder"
	}
	return &coderClient{cliPath: cliPath, runner: rt.Exec, stdout: rt.Stdout, stderr: rt.Stderr}, nil
}

func (c *coderClient) run(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	return c.runner.Run(ctx, LocalCommandRequest{Name: c.cliPath, Args: args, Stdout: stdout, Stderr: stderr})
}

func (c *coderClient) output(ctx context.Context, args []string) (string, error) {
	result, err := c.run(ctx, args, nil, nil)
	if err != nil {
		msg := strings.TrimSpace(result.Stdout + result.Stderr)
		if msg == "" {
			msg = err.Error()
		}
		return "", ExitError{Code: result.ExitCode, Message: fmt.Sprintf("coder %s failed: %s", strings.Join(args, " "), msg)}
	}
	return result.Stdout, nil
}

func (c *coderClient) version(ctx context.Context) error {
	result, err := c.run(ctx, []string{"version"}, nil, nil)
	if err != nil {
		msg := strings.TrimSpace(result.Stdout + result.Stderr)
		if msg == "" {
			msg = err.Error()
		}
		return ExitError{Code: result.ExitCode, Message: "coder cli unavailable: " + msg}
	}
	return nil
}

func (c *coderClient) whoami(ctx context.Context) error {
	result, err := c.run(ctx, []string{"whoami", "-o", "json"}, nil, nil)
	if err != nil {
		msg := strings.TrimSpace(result.Stdout + result.Stderr)
		if msg == "" {
			msg = err.Error()
		}
		if coderWhoamiMissingLogin(msg) {
			return ExitError{Code: result.ExitCode, Message: "coder credential unavailable: run `coder login <url>`; mutation=false detail=" + msg}
		}
		return ExitError{Code: result.ExitCode, Message: "coder credential check failed: mutation=false detail=" + msg}
	}
	return nil
}

func (c *coderClient) list(ctx context.Context) ([]coderWorkspace, error) {
	out, err := c.output(ctx, []string{"list", "-o", "json"})
	if err != nil {
		return nil, err
	}
	return parseCoderWorkspaces(out)
}

func (c *coderClient) listAll(ctx context.Context) ([]coderWorkspace, error) {
	out, err := c.output(ctx, []string{"list", "--all", "-o", "json"})
	if err != nil {
		return nil, err
	}
	return parseCoderWorkspaces(out)
}

func (c *coderClient) create(ctx context.Context, cfg Config, name string) error {
	args := []string{"create", "--yes", "--template", strings.TrimSpace(cfg.Coder.Template)}
	if preset := strings.TrimSpace(cfg.Coder.Preset); preset != "" {
		args = append(args, "--preset", preset)
	}
	if wait := strings.TrimSpace(cfg.Coder.Wait); strings.EqualFold(wait, "no") {
		args = append(args, "--no-wait")
	}
	if cfg.Coder.UseParameterDefaults {
		args = append(args, "--use-parameter-defaults")
	}
	for _, param := range cfg.Coder.Parameters {
		args = append(args, "--parameter", strings.TrimSpace(param))
	}
	if file := strings.TrimSpace(cfg.Coder.RichParameterFile); file != "" {
		args = append(args, "--rich-parameter-file", file)
	}
	args = append(args, name)
	result, err := c.run(ctx, args, c.stdout, c.stderr)
	if err != nil {
		return ExitError{Code: result.ExitCode, Message: fmt.Sprintf("coder create workspace %s failed: %s", name, strings.TrimSpace(result.Stdout+result.Stderr))}
	}
	return nil
}

func (c *coderClient) start(ctx context.Context, name string) error {
	result, err := c.run(ctx, []string{"start", "--yes", name}, c.stdout, c.stderr)
	if err != nil {
		return ExitError{Code: result.ExitCode, Message: fmt.Sprintf("coder start workspace %s failed: %s", name, strings.TrimSpace(result.Stdout+result.Stderr))}
	}
	return nil
}

func (c *coderClient) stop(ctx context.Context, name string) error {
	result, err := c.run(ctx, []string{"stop", "--yes", name}, c.stdout, c.stderr)
	if err != nil {
		return ExitError{Code: result.ExitCode, Message: fmt.Sprintf("coder stop workspace %s failed: %s", name, strings.TrimSpace(result.Stdout+result.Stderr))}
	}
	return nil
}

func (c *coderClient) delete(ctx context.Context, name string) error {
	result, err := c.run(ctx, []string{"delete", "--yes", name}, c.stdout, c.stderr)
	if err != nil {
		return ExitError{Code: result.ExitCode, Message: fmt.Sprintf("coder delete workspace %s failed: %s", name, strings.TrimSpace(result.Stdout+result.Stderr))}
	}
	return nil
}

type coderWorkspace struct {
	ID         string
	Name       string
	Owner      string
	Template   string
	Status     string
	Autostart  string
	Outdated   bool
	Transition string
	Agents     []coderAgent
	Labels     map[string]string
}

type coderAgent struct {
	Name      string
	OS        string
	Status    string
	Lifecycle string
}

func parseCoderWorkspaces(out string) ([]coderWorkspace, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || strings.EqualFold(trimmed, "No workspaces found!") {
		return nil, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, exit(5, "coder list returned invalid JSON: %v", err)
	}
	switch value := raw.(type) {
	case []any:
		return parseCoderWorkspaceArray(value)
	case map[string]any:
		for _, key := range []string{"workspaces", "items", "data"} {
			if items, ok := value[key].([]any); ok {
				return parseCoderWorkspaceArray(items)
			}
		}
		return parseCoderWorkspaceArray([]any{value})
	default:
		return nil, exit(5, "coder list returned unsupported JSON shape")
	}
}

func parseCoderWorkspaceArray(items []any) ([]coderWorkspace, error) {
	workspaces := make([]coderWorkspace, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, exit(5, "coder list workspace entry is not an object")
		}
		workspaces = append(workspaces, parseCoderWorkspaceObject(obj))
	}
	return workspaces, nil
}

func parseCoderWorkspaceObject(obj map[string]any) coderWorkspace {
	workspace := coderWorkspace{
		ID:         stringField(obj, "id"),
		Name:       firstStringField(obj, "name", "workspace_name"),
		Status:     firstStringField(obj, "status", "latest_build.status"),
		Transition: firstStringField(obj, "transition", "latest_build.transition"),
		Template:   firstStringField(obj, "template_display_name", "template_name", "template.name"),
		Labels:     map[string]string{},
	}
	if owner := objectField(obj, "owner"); owner != nil {
		workspace.Owner = firstStringField(owner, "name", "username")
	}
	if workspace.Owner == "" {
		workspace.Owner = firstStringField(obj, "owner_name", "owner")
	}
	if build := objectField(obj, "latest_build"); build != nil {
		if workspace.Status == "" {
			workspace.Status = firstStringField(build, "status")
		}
		if workspace.Transition == "" {
			workspace.Transition = firstStringField(build, "transition")
		}
		workspace.Agents = append(workspace.Agents, parseCoderAgents(build)...)
	}
	workspace.Agents = append(workspace.Agents, parseCoderAgents(obj)...)
	for k, v := range mapField(obj, "labels") {
		workspace.Labels[k] = v
	}
	return workspace
}

func parseCoderAgents(obj map[string]any) []coderAgent {
	var out []coderAgent
	for _, key := range []string{"agents", "workspace_agents"} {
		if items, ok := obj[key].([]any); ok {
			for _, item := range items {
				agentObj, ok := item.(map[string]any)
				if !ok {
					continue
				}
				out = append(out, coderAgent{
					Name:      firstStringField(agentObj, "name"),
					OS:        firstStringField(agentObj, "operating_system", "os"),
					Status:    firstStringField(agentObj, "status"),
					Lifecycle: firstStringField(agentObj, "lifecycle_state", "lifecycle"),
				})
			}
		}
	}
	for _, key := range []string{"resources"} {
		if items, ok := obj[key].([]any); ok {
			for _, item := range items {
				resource, ok := item.(map[string]any)
				if !ok {
					continue
				}
				out = append(out, parseCoderAgents(resource)...)
			}
		}
	}
	return out
}

func objectField(obj map[string]any, path string) map[string]any {
	current := obj
	parts := strings.Split(path, ".")
	for i, part := range parts {
		next, ok := current[part]
		if !ok {
			return nil
		}
		value, ok := next.(map[string]any)
		if !ok {
			return nil
		}
		if i == len(parts)-1 {
			return value
		}
		current = value
	}
	return nil
}

func stringField(obj map[string]any, key string) string {
	if value, ok := obj[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func firstStringField(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if strings.Contains(key, ".") {
			if value, ok := nestedStringField(obj, key); ok {
				return value
			}
			continue
		}
		if value := stringField(obj, key); value != "" {
			return value
		}
	}
	return ""
}

func nestedStringField(obj map[string]any, path string) (string, bool) {
	parts := strings.Split(path, ".")
	current := obj
	for i, part := range parts {
		value, ok := current[part]
		if !ok {
			return "", false
		}
		if i == len(parts)-1 {
			str, ok := value.(string)
			return strings.TrimSpace(str), ok && strings.TrimSpace(str) != ""
		}
		next, ok := value.(map[string]any)
		if !ok {
			return "", false
		}
		current = next
	}
	return "", false
}

func mapField(obj map[string]any, key string) map[string]string {
	out := map[string]string{}
	raw, ok := obj[key].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
