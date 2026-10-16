package dockersandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const cloudLeasePrefix = "dsbxc_"
const cloudScope = "docker-sandbox:cloud"
const cloudBaseline = "v0.48.0"

var cloudIDPattern = regexp.MustCompile(`^sbx_[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var cloudVersionPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

func (c *sbxCLI) cloudQuiet(ctx context.Context, args []string) (string, string, error) {
	args = append([]string{"--cloud"}, args...)
	result, err := c.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name: c.binary(), Args: args, Env: c.env(), MaxCapturedOutputBytes: 4 << 20,
	})
	if err != nil || result.ExitCode != 0 {
		return strings.TrimSpace(result.Stdout), "", cloudCommandError(args, result.ExitCode, err)
	}
	return strings.TrimSpace(result.Stdout), "", nil
}

// Cloud diagnostics are untrusted and can include account data. Only expose our
// own stage and guidance, never arbitrary diagnostic text or echoed arguments.
func cloudCommandError(args []string, code int, cause error) error {
	verb := "command"
	if len(args) > 1 {
		verb = args[1]
	}
	if isMissingExecutableError(cause) {
		return core.Exit(2, "Docker cloud requires sbx: install with brew install docker/tap/sbx or set --docker-sandbox-cli")
	}
	if cause != nil {
		return fmt.Errorf("Docker cloud %s failed (exit=%d): %w", verb, code, cause)
	}
	return core.Exit(2, "Docker cloud %s failed (exit=%d); run sbx --cloud diagnose to check sign-in, connectivity and Docker Agentic Platform access", verb, code)
}

func (c *sbxCLI) cloudReady(ctx context.Context) error {
	out, _, err := c.runQuiet(ctx, []string{"version", "--json"})
	if err != nil {
		return err
	}
	var version struct {
		Client struct {
			Version string `json:"version"`
		} `json:"client"`
	}
	if json.Unmarshal([]byte(out), &version) != nil {
		return core.Exit(2, "Docker cloud: invalid sbx version JSON; install sbx %s or newer", cloudBaseline)
	}
	parts := cloudVersionPattern.FindStringSubmatch(version.Client.Version)
	if len(parts) != 4 {
		return core.Exit(2, "Docker cloud: unsupported sbx version; install sbx %s or newer", cloudBaseline)
	}
	major, _ := strconv.Atoi(parts[1])
	minor, _ := strconv.Atoi(parts[2])
	if major == 0 && minor < 48 {
		return core.Exit(2, "Docker cloud requires sbx %s or newer; upgrade with brew upgrade docker/tap/sbx", cloudBaseline)
	}
	out, _, runErr := c.runQuiet(ctx, []string{"diagnose", "--json"})
	var report struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if json.Unmarshal([]byte(out), &report) != nil {
		if runErr != nil {
			return runErr
		}
		return core.Exit(2, "Docker cloud: invalid sbx diagnostic JSON; run sbx --cloud diagnose")
	}
	checks := map[string]string{}
	for _, check := range report.Checks {
		if _, duplicate := checks[check.Name]; duplicate {
			return core.Exit(2, "Docker cloud: ambiguous sbx diagnostic checks")
		}
		checks[check.Name] = check.Status
	}
	if checks["Authentication"] == "pass" && checks["Cloud entitlement"] == "fail" {
		return core.Exit(2, "Docker cloud readiness: account lacks Cloud Sandboxes access; activate a Docker Agentic Platform subscription, then run sbx login")
	}
	for _, check := range []struct{ name, guidance string }{
		{"CLI binary", "install sbx with brew install docker/tap/sbx"},
		{"Binary version", "upgrade sbx with brew upgrade docker/tap/sbx"},
		{"Authentication", "run sbx login with the subscribed Docker account"},
		{"Cloud API", "run sbx --cloud diagnose and check cloud connectivity"},
		{"Cloud entitlement", "activate a Docker Agentic Platform subscription with Cloud Sandboxes access, then run sbx login"},
	} {
		if checks[check.name] != "pass" {
			return core.Exit(2, "Docker cloud readiness: %s did not pass; %s", check.name, check.guidance)
		}
	}
	return runErr
}

func (c *sbxCLI) cloudReadFile(ctx context.Context, id, remotePath string, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > core.DelegatedRunDownloadMaxBytes {
		return nil, core.Exit(2, "Docker cloud artifact requires a positive bounded size")
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// The remote check avoids directories/symlinks. Runtime capture enforces the
	// boundary even if the sandbox changes the file or its head executable.
	args := []string{"--cloud", "exec", id, "sh", "-c", `test -f "$1" && test ! -L "$1" && exec head -c "$2" -- "$1"`, "crabbox-artifact", remotePath, strconv.Itoa(maxBytes + 1)}
	result, err := c.rt.Exec.Run(bounded, core.LocalCommandRequest{
		Name: c.binary(), Args: args, Env: c.env(), MaxCapturedOutputBytes: maxBytes + 1,
	})
	if len(result.Stdout) > maxBytes {
		return nil, core.Exit(7, "Docker cloud artifact exceeds %d bytes", maxBytes)
	}
	if err != nil || result.ExitCode != 0 {
		return nil, cloudCommandError(args, result.ExitCode, err)
	}
	return []byte(result.Stdout), nil
}

func (c *sbxCLI) cloudCreate(ctx context.Context, name string) (string, error) {
	args := []string{"create", "--name", name, "--ttl", cloudTTL(c.cfg).String(), "--on-timeout", "delete"}
	if template := strings.TrimSpace(c.cfg.DockerSandbox.Template); template != "" {
		args = append(args, "--template", template)
	} else {
		args = append(args, "shell")
	}
	if c.cfg.DockerSandbox.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(int(c.cfg.DockerSandbox.CPUs)))
	}
	if c.cfg.DockerSandbox.Memory != "" {
		args = append(args, "--memory", c.cfg.DockerSandbox.Memory)
	}
	for _, value := range c.cfg.DockerSandbox.MCP {
		args = append(args, "--static-mcp", value)
	}
	for _, value := range c.cfg.DockerSandbox.Kit {
		args = append(args, "--kit", value)
	}
	out, _, err := c.runQuiet(ctx, args)
	if err != nil {
		return "", fmt.Errorf("%w; allocation may have reached Docker; inspect sbx --cloud ls for name %s before retrying", err, name)
	}
	var id string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if cloudIDPattern.MatchString(line) {
			if id != "" {
				return "", core.Exit(2, "Docker cloud create returned ambiguous IDs; inspect sbx --cloud ls for name %s", name)
			}
			id = line
		}
	}
	if id == "" {
		return "", core.Exit(2, "Docker cloud create returned no supported sandbox ID; inspect sbx --cloud ls for name %s", name)
	}
	return id, nil
}

// Require distinct immutable IDs in cloud inventory. Names are never authority.
func (c *sbxCLI) cloudList(ctx context.Context) ([]sandboxRecord, error) {
	out, _, err := c.runQuiet(ctx, []string{"ls", "--json"})
	if err != nil {
		return nil, err
	}
	return parseCloudSandboxList(out)
}

func parseCloudSandboxList(out string) ([]sandboxRecord, error) {
	var items []json.RawMessage
	array := strings.TrimSpace(out)
	if strings.HasPrefix(array, "{") {
		var wrapper map[string]json.RawMessage
		if json.Unmarshal([]byte(array), &wrapper) != nil {
			return nil, core.Exit(2, "Docker cloud inventory is invalid JSON")
		}
		array = ""
		for _, key := range []string{"sandboxes", "items", "data", "results"} {
			if raw, ok := wrapper[key]; ok {
				if array != "" {
					return nil, core.Exit(2, "Docker cloud inventory has ambiguous arrays")
				}
				array = strings.TrimSpace(string(raw))
			}
		}
	}
	if !strings.HasPrefix(array, "[") || json.Unmarshal([]byte(array), &items) != nil {
		return nil, core.Exit(2, "Docker cloud inventory requires a sandbox array")
	}
	records, err := parseSandboxList(array)
	if err != nil || len(records) != len(items) {
		return nil, core.Exit(2, "Docker cloud inventory contains invalid records")
	}
	seen := map[string]bool{}
	for _, record := range records {
		if !cloudIDPattern.MatchString(record.ID) || record.ID == record.Name || seen[record.ID] {
			return nil, core.Exit(2, "Docker cloud inventory has a missing, invalid or duplicate immutable ID")
		}
		seen[record.ID] = true
	}
	return records, nil
}
