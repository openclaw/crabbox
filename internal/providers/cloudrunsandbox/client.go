package cloudrunsandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// sandboxTransport is the lifecycle surface for either a remote ComputeSDK
// gateway or the in-container `sandbox` CLI.
type sandboxTransport interface {
	Mode() string
	Health(ctx context.Context) error
	Create(ctx context.Context, sandboxID string, opts runOptions) error
	Exec(ctx context.Context, sandboxID, command string, opts execOptions, stdout, stderr io.Writer) (int, error)
	Destroy(ctx context.Context, sandboxID string) error
	WriteFile(ctx context.Context, sandboxID, path, content string) error
}

type runOptions struct {
	AllowEgress bool
	Write       bool
	Rootfs      string
	Workdir     string
	Env         map[string]string
}

type execOptions struct {
	Workdir string
	Env     map[string]string
	Timeout time.Duration
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var newTransport = func(cfg Config, rt Runtime) (sandboxTransport, error) {
	gatewayURL := strings.TrimSpace(cfg.CloudRunSandbox.GatewayURL)
	secret := firstNonEmpty(
		os.Getenv("CRABBOX_CLOUD_RUN_SANDBOX_SECRET"),
		os.Getenv("CLOUD_RUN_SANDBOX_SECRET"),
	)
	if gatewayURL != "" {
		if secret == "" {
			return nil, exit(2, "provider=cloud-run-sandbox remote mode requires CLOUD_RUN_SANDBOX_SECRET or CRABBOX_CLOUD_RUN_SANDBOX_SECRET")
		}
		validated, err := validateGatewayURL(gatewayURL)
		if err != nil {
			return nil, err
		}
		httpClient := rt.HTTP
		if httpClient == nil {
			httpClient = http.DefaultClient
		}
		return &remoteTransport{
			baseURL:   validated,
			secret:    secret,
			authToken: firstNonEmpty(os.Getenv("CRABBOX_CLOUD_RUN_SANDBOX_AUTH_TOKEN"), os.Getenv("CLOUD_RUN_AUTH_TOKEN")),
			cfg:       cfg,
			http:      secureHTTPClient(httpClient, validated),
		}, nil
	}
	if rt.Exec == nil {
		return nil, exit(2, "provider=cloud-run-sandbox direct mode requires Runtime.Exec")
	}
	return &directTransport{
		cfg: cfg,
		rt:  rt,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func validateGatewayURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=cloud-run-sandbox gateway URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=cloud-run-sandbox gateway URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "provider=cloud-run-sandbox gateway URL must use HTTPS except for loopback development endpoints")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func secureHTTPClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	original := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(trusted, req.URL) {
			return fmt.Errorf("cloud-run-sandbox refused cross-origin redirect to %s", req.URL.Redacted())
		}
		if original != nil {
			return original(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func sameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

func validateSandboxID(id string) error {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(id) {
		return exit(2, "invalid cloud-run-sandbox id %q", id)
	}
	return nil
}

func validateEnv(env map[string]string) error {
	for key := range env {
		if !envNamePattern.MatchString(key) {
			return exit(2, "invalid environment variable name %q", key)
		}
	}
	return nil
}

// --- remote gateway transport (ComputeSDK Cloud Run protocol) ---

type remoteTransport struct {
	baseURL   string
	secret    string
	authToken string
	cfg       Config
	http      *http.Client
}

func (t *remoteTransport) Mode() string { return "remote" }

type gatewayExecResponse struct {
	SandboxID string `json:"sandboxId"`
	ExitCode  int    `json:"exitCode"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Error     string `json:"error"`
	Status    string `json:"status"`
	Success   bool   `json:"success"`
}

func (t *remoteTransport) request(ctx context.Context, path string, body map[string]any) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-ComputeSDK-Cloud-Run-Secret", t.secret)
	if t.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, exit(2, "cloud-run-sandbox gateway unauthorized; check CLOUD_RUN_SANDBOX_SECRET")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &errBody)
		detail := strings.TrimSpace(errBody.Error)
		if detail == "" {
			detail = strings.TrimSpace(string(data))
		}
		if detail == "" {
			detail = resp.Status
		}
		return nil, fmt.Errorf("cloud-run-sandbox gateway %s: %s", path, detail)
	}
	return json.RawMessage(data), nil
}

func (t *remoteTransport) requestBody(sandboxID string, opts runOptions, extra map[string]any) map[string]any {
	body := map[string]any{
		"sandboxId":     sandboxID,
		"executionMode": "stateful",
		"allowEgress":   opts.AllowEgress || t.cfg.CloudRunSandbox.AllowEgress,
		"write":         opts.Write || t.cfg.CloudRunSandbox.Write,
	}
	if rootfs := blank(opts.Rootfs, t.cfg.CloudRunSandbox.Rootfs); rootfs != "" {
		body["rootfs"] = rootfs
	}
	if workdir := blank(opts.Workdir, t.cfg.CloudRunSandbox.Workdir); workdir != "" {
		body["workdir"] = workdir
		body["cwd"] = workdir
	}
	if len(opts.Env) > 0 {
		body["env"] = opts.Env
	}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

func (t *remoteTransport) Health(ctx context.Context) error {
	// Prefer the documented health endpoint; fall through to info if absent.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloud-run-sandbox gateway unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Some gateways may not expose GET /v1/health with the secret header path.
	// A successful unauthorized-or-not-found is still reachability evidence.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	return fmt.Errorf("cloud-run-sandbox gateway health returned %s", resp.Status)
}

func (t *remoteTransport) Create(ctx context.Context, sandboxID string, opts runOptions) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	if err := validateEnv(opts.Env); err != nil {
		return err
	}
	_, err := t.request(ctx, "/v1/sandbox/create", t.requestBody(sandboxID, opts, nil))
	return err
}

func (t *remoteTransport) Exec(ctx context.Context, sandboxID, command string, opts execOptions, stdout, stderr io.Writer) (int, error) {
	if err := validateSandboxID(sandboxID); err != nil {
		return 2, err
	}
	if err := validateEnv(opts.Env); err != nil {
		return 2, err
	}
	body := t.requestBody(sandboxID, runOptions{Workdir: opts.Workdir, Env: opts.Env}, map[string]any{
		"command": command,
	})
	if opts.Timeout > 0 {
		body["timeout"] = int(opts.Timeout.Milliseconds())
	}
	raw, err := t.request(ctx, "/v1/sandbox/exec", body)
	if err != nil {
		return 1, err
	}
	var result gatewayExecResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return 1, fmt.Errorf("cloud-run-sandbox decode exec response: %w", err)
	}
	if stdout != nil && result.Stdout != "" {
		_, _ = io.WriteString(stdout, result.Stdout)
	}
	if stderr != nil && result.Stderr != "" {
		_, _ = io.WriteString(stderr, result.Stderr)
	}
	return result.ExitCode, nil
}

func (t *remoteTransport) Destroy(ctx context.Context, sandboxID string) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	_, err := t.request(ctx, "/v1/sandbox/destroy", map[string]any{"sandboxId": sandboxID})
	return err
}

func (t *remoteTransport) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	_, err := t.request(ctx, "/v1/sandbox/writeFile", t.requestBody(sandboxID, runOptions{Write: true}, map[string]any{
		"path":    path,
		"content": content,
	}))
	return err
}

// --- direct in-container sandbox CLI transport ---

type directTransport struct {
	cfg Config
	rt  Runtime
}

func (t *directTransport) Mode() string { return "direct" }

func (t *directTransport) binary() string {
	return blank(strings.TrimSpace(t.cfg.CloudRunSandbox.CLIPath), defaultCLIPath)
}

func (t *directTransport) baseArgs() []string {
	args := []string{}
	if mode := strings.TrimSpace(t.cfg.CloudRunSandbox.Mode); mode != "" {
		args = append(args, "--mode", mode)
	}
	return args
}

func (t *directTransport) pushRunArgs(args []string, opts runOptions) []string {
	if opts.AllowEgress || t.cfg.CloudRunSandbox.AllowEgress {
		args = append(args, "--allow-egress")
	}
	if rootfs := blank(opts.Rootfs, t.cfg.CloudRunSandbox.Rootfs); rootfs != "" {
		args = append(args, "--rootfs", rootfs)
	}
	if workdir := blank(opts.Workdir, t.cfg.CloudRunSandbox.Workdir); workdir != "" {
		args = append(args, "--workdir", workdir)
	}
	if opts.Write || t.cfg.CloudRunSandbox.Write {
		args = append(args, "--write")
	}
	for key, value := range opts.Env {
		args = append(args, "-e", key+"="+value)
	}
	return args
}

func (t *directTransport) pushExecArgs(args []string, opts execOptions) []string {
	if workdir := blank(opts.Workdir, t.cfg.CloudRunSandbox.Workdir); workdir != "" {
		args = append(args, "--workdir", workdir)
	}
	for key, value := range opts.Env {
		args = append(args, "-e", key+"="+value)
	}
	return args
}

func (t *directTransport) runCLI(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	result, err := t.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   t.binary(),
		Args:   args,
		Stdout: stdout,
		Stderr: stderr,
	})
	return result, err
}

func (t *directTransport) Health(ctx context.Context) error {
	var stdout, stderr bytes.Buffer
	result, err := t.runCLI(ctx, append(t.baseArgs(), "--help"), &stdout, &stderr)
	if err != nil {
		return exit(2, "cloud-run-sandbox CLI %q not usable: %v (%s)", t.binary(), err, strings.TrimSpace(stderr.String()))
	}
	if result.ExitCode != 0 {
		return exit(2, "cloud-run-sandbox CLI %q --help exited %d: %s", t.binary(), result.ExitCode, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return nil
}

func (t *directTransport) Create(ctx context.Context, sandboxID string, opts runOptions) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	if err := validateEnv(opts.Env); err != nil {
		return err
	}
	args := append(t.baseArgs(), "run", sandboxID, "--detach")
	args = t.pushRunArgs(args, opts)
	var stdout, stderr bytes.Buffer
	result, err := t.runCLI(ctx, args, &stdout, &stderr)
	if err != nil {
		return fmt.Errorf("sandbox run %s failed: %w (%s)", sandboxID, err, strings.TrimSpace(stderr.String()))
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sandbox run %s exited %d: %s", sandboxID, result.ExitCode, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return nil
}

func (t *directTransport) Exec(ctx context.Context, sandboxID, command string, opts execOptions, stdout, stderr io.Writer) (int, error) {
	if err := validateSandboxID(sandboxID); err != nil {
		return 2, err
	}
	if err := validateEnv(opts.Env); err != nil {
		return 2, err
	}
	args := append(t.baseArgs(), "exec", sandboxID)
	args = t.pushExecArgs(args, opts)
	args = append(args, "--", "/bin/sh", "-c", command)
	result, err := t.runCLI(ctx, args, stdout, stderr)
	if err != nil {
		return result.ExitCode, fmt.Errorf("sandbox exec failed: %w", err)
	}
	return result.ExitCode, nil
}

func (t *directTransport) Destroy(ctx context.Context, sandboxID string) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	args := append(t.baseArgs(), "delete", sandboxID, "--force")
	var stdout, stderr bytes.Buffer
	result, err := t.runCLI(ctx, args, &stdout, &stderr)
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		if isNotFoundDetail(detail) {
			return nil
		}
		return fmt.Errorf("sandbox delete %s failed: %w (%s)", sandboxID, err, detail)
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(stderr.String() + stdout.String())
		if isNotFoundDetail(detail) {
			return nil
		}
		return fmt.Errorf("sandbox delete %s exited %d: %s", sandboxID, result.ExitCode, detail)
	}
	return nil
}

func (t *directTransport) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	// Encode content as base64 so binary archives stay intact inside shell.
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	command := fmt.Sprintf("mkdir -p \"$(dirname %s)\" && printf '%%s' %s | base64 -d > %s",
		shellQuote(path), shellQuote(encoded), shellQuote(path))
	code, err := t.Exec(ctx, sandboxID, command, execOptions{Workdir: "/"}, nil, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("sandbox writeFile %s exited %d", path, code)
	}
	return nil
}

func isNotFoundDetail(detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist") || strings.Contains(lower, "no such")
}
