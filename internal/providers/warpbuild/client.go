package warpbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const apiScope = "https://api.warpbuild.com/api/v1"

var errSandboxNotFound = errors.New("WarpBuild sandbox not found")

type sandbox struct {
	Name    string `json:"sandbox"`
	State   string `json:"state"`
	Size    string `json:"size"`
	ID      string `json:"id"`
	Created string `json:"created"`
	Expires string `json:"expires"`
}

type client struct {
	cfg core.WarpBuildConfig
	rt  core.Runtime
}

func newClient(cfg core.Config, rt core.Runtime) (*client, error) {
	if strings.TrimSpace(cfg.WarpBuild.Token) == "" {
		return nil, core.Exit(2, "provider=warpbuild requires WB_TOKEN or CRABBOX_WARPBUILD_TOKEN with CI scope")
	}
	if rt.Exec == nil {
		return nil, core.Exit(2, "provider=warpbuild requires Runtime.Exec")
	}
	return &client{cfg: cfg.WarpBuild, rt: rt}, nil
}

func (c *client) env() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "WB_TOKEN", "CRABBOX_WARPBUILD_TOKEN", "WB_API_URL", "WB_SBX_RUNNER_LABEL", "WB_SBX_USER", "WB_NO_UPDATE_CHECK":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "WB_TOKEN="+c.cfg.Token, "WB_API_URL="+apiScope, "WB_SBX_USER=runner", "WB_NO_UPDATE_CHECK=1")
}

func (c *client) control(ctx context.Context, args ...string) (core.LocalCommandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.call(ctx, nil, nil, nil, args...)
}

func (c *client) call(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (core.LocalCommandResult, error) {
	result, err := c.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name: c.cfg.CLIPath, Args: append([]string{"-f", "json"}, args...), Env: c.env(),
		Stdin: stdin, Stdout: stdout, Stderr: stderr, MaxCapturedOutputBytes: 1024 * 1024, CancelGracePeriod: time.Second,
	})
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil || result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" && err != nil {
			detail = err.Error()
		}
		detail = shared.RedactErrorSecrets(detail, c.cfg.Token)
		return result, core.Exit(5, "WarpBuild %s failed: %s", strings.Join(args[:min(2, len(args))], " "), detail)
	}
	return result, nil
}

func (c *client) info(ctx context.Context, name string) (sandbox, error) {
	result, err := c.control(ctx, "sbx", "info", name)
	// wb maps HTTP 404 / FVE_004 to exit 4, separately from auth and transport failures.
	if ctx.Err() == nil && result.ExitCode == 4 {
		return sandbox{}, fmt.Errorf("%w: %s", errSandboxNotFound, name)
	}
	if err != nil {
		return sandbox{}, err
	}
	var item sandbox
	if json.Unmarshal([]byte(result.Stdout), &item) != nil || item.Name != name || item.ID == "" || item.Created == "" || item.State == "" {
		return sandbox{}, core.Exit(5, "WarpBuild info omitted the exact sandbox identity; check wb CLI compatibility")
	}
	return item, nil
}

func (c *client) list(ctx context.Context) ([]sandbox, error) {
	result, err := c.control(ctx, "sbx", "list")
	if err != nil {
		return nil, err
	}
	var items []sandbox
	if json.Unmarshal([]byte(result.Stdout), &items) != nil || items == nil {
		return nil, core.Exit(5, "WarpBuild list did not return a sandbox array")
	}
	for _, item := range items {
		if item.Name == "" || item.State == "" {
			return nil, core.Exit(5, "WarpBuild list omitted sandbox names or states")
		}
	}
	return items, nil
}

func (c *client) create(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	_, err := c.call(ctx, nil, nil, nil, "sbx", "create", name, "--runner-label", c.cfg.RunnerLabel, "--wait", "150")
	return err
}

func (c *client) run(ctx context.Context, name, workdir string, command []string, environment map[string]string, stdout, stderr io.Writer) (int, error) {
	args := []string{"-f", "json", "sbx", "exec"}
	if workdir != "" {
		args = append(args, "-c", workdir)
	}
	var stdin io.Reader
	if len(environment) > 0 {
		keys := make([]string, 0, len(environment))
		for key := range environment {
			if !core.ValidShellEnvName(key) || key == "WB_TOKEN" || key == "CRABBOX_WARPBUILD_TOKEN" {
				return 2, core.Exit(2, "cannot forward environment variable %q to WarpBuild", key)
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var script strings.Builder
		for _, key := range keys {
			fmt.Fprintf(&script, "export %s=%s\n", key, core.ShellQuote(environment[key]))
		}
		script.WriteString("exec \"$@\"\n")
		stdin = strings.NewReader(script.String())
		args = append(args, "-i", name, "/bin/bash", "-s", "--")
	} else {
		args = append(args, name)
	}
	args = append(args, command...)
	result, err := c.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: c.cfg.CLIPath, Args: args, Env: c.env(), Stdin: stdin, Stdout: stdout, Stderr: stderr, DisableOutputCapture: true, CancelGracePeriod: time.Second})
	if ctx.Err() != nil {
		return result.ExitCode, ctx.Err()
	}
	if err != nil && !core.IsPlainLocalCommandExit(result, err) {
		return result.ExitCode, fmt.Errorf("WarpBuild command transport failed: %w", err)
	}
	return result.ExitCode, nil
}

func (c *client) shell(ctx context.Context, name, command string) error {
	code, err := c.run(ctx, name, "", []string{"/bin/bash", "-c", command}, nil, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if code != 0 {
		return core.Exit(5, "WarpBuild workspace command exited %d", code)
	}
	return nil
}

func (c *client) upload(ctx context.Context, name, remote string, source io.Reader) error {
	if file, ok := source.(*os.File); ok {
		_, err := c.call(ctx, nil, nil, nil, "sbx", "fs", "put", name, file.Name(), remote)
		return err
	}
	// Other borrowed streams need a private file for wb's filename-only upload.
	file, err := os.CreateTemp("", "crabbox-warpbuild-upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := io.Copy(file, source); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	_, err = c.call(ctx, nil, nil, nil, "sbx", "fs", "put", name, file.Name(), remote)
	return err
}
