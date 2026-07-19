package cloudrunsandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// sandboxTransport is the lifecycle surface for either a remote ComputeSDK
// gateway or the in-container `sandbox` CLI.
type sandboxTransport interface {
	Mode() string
	Health(ctx context.Context) error
	Probe(ctx context.Context, sandboxID, ownershipToken string) error
	Create(ctx context.Context, sandboxID string, opts runOptions) error
	Exec(ctx context.Context, sandboxID, command string, opts execOptions, stdout, stderr io.Writer) (int, error)
	Destroy(ctx context.Context, sandboxID, ownershipToken string) error
	WriteFile(ctx context.Context, sandboxID, path, content string, appendContent bool) error
}

type runOptions struct {
	AllowEgress    bool
	Write          bool
	Rootfs         string
	Workdir        string
	OmitWorkdir    bool
	OwnershipToken string
	Env            map[string]string
}

type execOptions struct {
	Workdir string
	Env     map[string]string
	Timeout time.Duration
}

var errSandboxNotFound = errors.New("cloud-run-sandbox sandbox not found")
var errSandboxAlreadyExists = errors.New("cloud-run-sandbox sandbox already exists")

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var newTransport = func(cfg Config, rt Runtime) (sandboxTransport, error) {
	// GatewayURL is deliberately absent from fileCloudRunSandboxConfig. Only an
	// operator-supplied flag or environment value can select the origin that
	// receives the environment-only gateway credentials below.
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
	Stream    string `json:"stream"`
	Data      string `json:"data"`
	ExitCode  *int   `json:"exitCode"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Error     string `json:"error"`
	Status    string `json:"status"`
	Success   bool   `json:"success"`
}

type gatewayLifecycle struct {
	Routing string `json:"routing"`
	Destroy string `json:"destroy"`
	Exec    string `json:"exec"`
}

type gatewayHealthResponse struct {
	Status    string           `json:"status"`
	Lifecycle gatewayLifecycle `json:"lifecycle"`
}

type gatewayCreateResponse struct {
	SandboxID      string           `json:"sandboxId"`
	OwnershipToken string           `json:"ownershipToken"`
	Status         string           `json:"status"`
	Lifecycle      gatewayLifecycle `json:"lifecycle"`
}

type gatewayDestroyResponse struct {
	SandboxID      string `json:"sandboxId"`
	OwnershipToken string `json:"ownershipToken"`
	Status         string `json:"status"`
	Success        bool   `json:"success"`
}

type gatewayStatusResponse struct {
	SandboxID      string `json:"sandboxId"`
	OwnershipToken string `json:"ownershipToken"`
	Status         string `json:"status"`
	Success        bool   `json:"success"`
}

type gatewayWriteFileResponse struct {
	SandboxID string `json:"sandboxId"`
	Status    string `json:"status"`
	Success   bool   `json:"success"`
}

func validateGatewayLifecycle(lifecycle gatewayLifecycle) error {
	if lifecycle.Routing != "durable" || lifecycle.Destroy != "synchronous" || lifecycle.Exec != "ndjson-stream" {
		return errors.New("cloud-run-sandbox gateway must guarantee lifecycle.routing=durable, lifecycle.destroy=synchronous, and lifecycle.exec=ndjson-stream")
	}
	return nil
}

func (t *remoteTransport) authorize(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-ComputeSDK-Cloud-Run-Secret", t.secret)
	if t.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	}
}

func (t *remoteTransport) request(ctx context.Context, path string, body map[string]any) (json.RawMessage, error) {
	ctx, cancel := contextWithDefaultTimeout(ctx, defaultExecTimeout)
	defer cancel()
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	t.authorize(req)
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
			Error          string `json:"error"`
			Code           string `json:"code"`
			SandboxID      string `json:"sandboxId"`
			OwnershipToken string `json:"ownershipToken"`
		}
		_ = json.Unmarshal(data, &errBody)
		detail := strings.TrimSpace(errBody.Error)
		if detail == "" {
			detail = strings.TrimSpace(string(data))
		}
		if detail == "" {
			detail = resp.Status
		}
		requestedID, _ := body["sandboxId"].(string)
		requestedToken, _ := body["ownershipToken"].(string)
		if resp.StatusCode == http.StatusNotFound && errBody.Code == "sandbox_not_found" && requestedID != "" && errBody.SandboxID == requestedID && requestedToken != "" && errBody.OwnershipToken == requestedToken {
			return nil, fmt.Errorf("%w: %s", errSandboxNotFound, detail)
		}
		if resp.StatusCode == http.StatusConflict && path == "/v1/sandbox/create" && errBody.Code == "sandbox_already_exists" && requestedID != "" && errBody.SandboxID == requestedID && requestedToken != "" && errBody.OwnershipToken == requestedToken {
			return nil, fmt.Errorf("%w: %s", errSandboxAlreadyExists, detail)
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
	if opts.OwnershipToken != "" {
		body["ownershipToken"] = opts.OwnershipToken
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
	ctx, cancel := contextWithDefaultTimeout(ctx, defaultExecTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/v1/health", nil)
	if err != nil {
		return err
	}
	t.authorize(req)
	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloud-run-sandbox gateway unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return exit(2, "cloud-run-sandbox gateway unauthorized; check gateway secret and Cloud Run IAM token")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cloud-run-sandbox gateway health returned %s", resp.Status)
	}
	var health gatewayHealthResponse
	if err := json.Unmarshal(data, &health); err != nil {
		return fmt.Errorf("cloud-run-sandbox decode health response: %w", err)
	}
	if health.Status != "ok" {
		return fmt.Errorf("cloud-run-sandbox gateway health status=%q, want ok", health.Status)
	}
	return validateGatewayLifecycle(health.Lifecycle)
}

func (t *remoteTransport) Create(ctx context.Context, sandboxID string, opts runOptions) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	if err := validateEnv(opts.Env); err != nil {
		return err
	}
	if opts.OwnershipToken == "" {
		return errors.New("cloud-run-sandbox create requires an ownership token")
	}
	raw, err := t.request(ctx, "/v1/sandbox/create", t.requestBody(sandboxID, opts, nil))
	if err != nil {
		return err
	}
	var result gatewayCreateResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("cloud-run-sandbox decode create response: %w", err)
	}
	if result.SandboxID != sandboxID || result.OwnershipToken != opts.OwnershipToken || result.Status != "running" {
		return fmt.Errorf("cloud-run-sandbox gateway did not confirm created sandbox %q", sandboxID)
	}
	return validateGatewayLifecycle(result.Lifecycle)
}

func (t *remoteTransport) Probe(ctx context.Context, sandboxID, ownershipToken string) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	if ownershipToken == "" {
		return errors.New("cloud-run-sandbox status requires an ownership token")
	}
	raw, err := t.request(ctx, "/v1/sandbox/status", map[string]any{"sandboxId": sandboxID, "ownershipToken": ownershipToken})
	if err != nil {
		return err
	}
	var result gatewayStatusResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("cloud-run-sandbox decode status response: %w", err)
	}
	if !result.Success || result.SandboxID != sandboxID || result.OwnershipToken != ownershipToken || result.Status != "running" {
		return fmt.Errorf("cloud-run-sandbox gateway did not confirm running sandbox %q", sandboxID)
	}
	return nil
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
	ctx, cancel := contextWithDefaultTimeout(ctx, defaultExecTimeout)
	defer cancel()
	payload, err := json.Marshal(body)
	if err != nil {
		return 1, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/v1/sandbox/exec", bytes.NewReader(payload))
	if err != nil {
		return 1, err
	}
	req.Header.Set("Content-Type", "application/json")
	t.authorize(req)
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := t.http.Do(req)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return 1, exit(2, "cloud-run-sandbox gateway unauthorized; check gateway credentials")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return 1, readErr
		}
		return 1, fmt.Errorf("cloud-run-sandbox gateway exec returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])); mediaType != "application/x-ndjson" {
		return 1, fmt.Errorf("cloud-run-sandbox gateway exec content-type=%q, want application/x-ndjson", mediaType)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	terminal := false
	exitCode := 1
	for scanner.Scan() {
		var frame gatewayExecResponse
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return 1, fmt.Errorf("cloud-run-sandbox decode exec frame: %w", err)
		}
		if frame.SandboxID != sandboxID {
			return 1, fmt.Errorf("cloud-run-sandbox gateway exec frame did not match sandbox %q", sandboxID)
		}
		switch frame.Stream {
		case "stdout":
			if terminal {
				return 1, errors.New("cloud-run-sandbox gateway sent stdout after terminal exec frame")
			}
			if stdout != nil {
				if _, err := io.WriteString(stdout, frame.Data); err != nil {
					return 1, fmt.Errorf("cloud-run-sandbox write stdout: %w", err)
				}
			}
			continue
		case "stderr":
			if terminal {
				return 1, errors.New("cloud-run-sandbox gateway sent stderr after terminal exec frame")
			}
			if stderr != nil {
				if _, err := io.WriteString(stderr, frame.Data); err != nil {
					return 1, fmt.Errorf("cloud-run-sandbox write stderr: %w", err)
				}
			}
			continue
		case "":
		default:
			return 1, fmt.Errorf("cloud-run-sandbox gateway exec returned unknown stream %q", frame.Stream)
		}
		if terminal || frame.Status != "completed" || !frame.Success || frame.ExitCode == nil || strings.TrimSpace(frame.Error) != "" {
			detail := strings.TrimSpace(frame.Error)
			if detail == "" {
				detail = "gateway did not return one exact completed execution confirmation"
			}
			return 1, fmt.Errorf("cloud-run-sandbox gateway exec %q failed: %s", sandboxID, detail)
		}
		terminal = true
		exitCode = *frame.ExitCode
	}
	if err := scanner.Err(); err != nil {
		return 1, fmt.Errorf("cloud-run-sandbox read exec stream: %w", err)
	}
	if !terminal {
		return 1, errors.New("cloud-run-sandbox gateway exec stream ended without a terminal frame")
	}
	return exitCode, nil
}

func (t *remoteTransport) Destroy(ctx context.Context, sandboxID, ownershipToken string) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	if ownershipToken == "" {
		return errors.New("cloud-run-sandbox destroy requires an ownership token")
	}
	raw, err := t.request(ctx, "/v1/sandbox/destroy", map[string]any{"sandboxId": sandboxID, "ownershipToken": ownershipToken})
	if err != nil {
		return err
	}
	var result gatewayDestroyResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("cloud-run-sandbox decode destroy response: %w", err)
	}
	if !result.Success || result.SandboxID != sandboxID || result.OwnershipToken != ownershipToken || result.Status != "destroyed" {
		return fmt.Errorf("cloud-run-sandbox gateway did not synchronously confirm destroyed sandbox %q", sandboxID)
	}
	return nil
}

func (t *remoteTransport) WriteFile(ctx context.Context, sandboxID, path, content string, appendContent bool) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	raw, err := t.request(ctx, "/v1/sandbox/writeFile", t.requestBody(sandboxID, runOptions{Write: true}, map[string]any{
		"path":    path,
		"content": content,
		"append":  appendContent,
	}))
	if err != nil {
		return err
	}
	var result gatewayWriteFileResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("cloud-run-sandbox decode writeFile response: %w", err)
	}
	if !result.Success || result.SandboxID != sandboxID || result.Status != "written" {
		return fmt.Errorf("cloud-run-sandbox gateway did not confirm write to sandbox %q", sandboxID)
	}
	return nil
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

func (t *directTransport) baseArgs() []string { return nil }

func (t *directTransport) pushRunArgs(args []string, opts runOptions) []string {
	if opts.AllowEgress || t.cfg.CloudRunSandbox.AllowEgress {
		args = append(args, "--allow-egress")
	}
	if rootfs := blank(opts.Rootfs, t.cfg.CloudRunSandbox.Rootfs); rootfs != "" {
		args = append(args, "--rootfs", rootfs)
	}
	if !opts.OmitWorkdir {
		if workdir := blank(opts.Workdir, t.cfg.CloudRunSandbox.Workdir); workdir != "" {
			args = append(args, "--workdir", workdir)
		}
	}
	if opts.Write || t.cfg.CloudRunSandbox.Write {
		args = append(args, "--write")
	}
	return args
}

func (t *directTransport) pushExecArgs(args []string, opts execOptions) []string {
	if workdir := blank(opts.Workdir, t.cfg.CloudRunSandbox.Workdir); workdir != "" {
		args = append(args, "--workdir", workdir)
	}
	return args
}

func (t *directTransport) runCLI(ctx context.Context, args []string, stdout, stderr io.Writer) (LocalCommandResult, error) {
	return t.runCLIWithStdin(ctx, args, nil, stdout, stderr)
}

func (t *directTransport) runCLIWithStdin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (LocalCommandResult, error) {
	ctx, cancel := contextWithDefaultTimeout(ctx, defaultExecTimeout)
	defer cancel()
	result, err := t.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:                 t.binary(),
		Args:                 args,
		Stdin:                stdin,
		Stdout:               stdout,
		Stderr:               stderr,
		DisableOutputCapture: true,
	})
	return result, err
}

func contextWithDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, timeout)
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

func (t *directTransport) Probe(ctx context.Context, sandboxID, ownershipToken string) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	if ownershipToken != sandboxID {
		return errors.New("cloud-run-sandbox direct status ownership token does not match the sandbox ID")
	}
	args := append(t.baseArgs(), "exec", sandboxID, "--", "/bin/true")
	var stdout, stderr bytes.Buffer
	result, err := t.runCLI(ctx, args, &stdout, &stderr)
	if err != nil {
		return fmt.Errorf("sandbox status %s failed: %w (%s)", sandboxID, err, strings.TrimSpace(stderr.String()))
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(stderr.String() + stdout.String())
		if isDirectSandboxNotFound(sandboxID, detail) {
			return fmt.Errorf("%w: %s", errSandboxNotFound, detail)
		}
		return fmt.Errorf("sandbox status %s exited %d: %s", sandboxID, result.ExitCode, detail)
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
	if opts.OwnershipToken != sandboxID {
		return errors.New("cloud-run-sandbox direct create requires the sandbox ID as its ownership token")
	}
	if len(opts.Env) > 0 {
		return exit(2, "cloud-run-sandbox direct create does not accept environment values; pass them to exec so they stay off argv")
	}
	args := append(t.baseArgs(), "run", sandboxID, "--detach")
	createOpts := opts
	// The configured workdir may not exist until archive setup. Launch the
	// keeper from the image's default directory, then create the workdir via exec.
	createOpts.Workdir = ""
	createOpts.OmitWorkdir = true
	args = t.pushRunArgs(args, createOpts)
	keeper := "PATH=" + shellQuote(defaultSandboxPath) + "; export PATH; while :; do /bin/sleep 3600; done"
	args = append(args, "--", "/bin/sh", "-c", keeper)
	var stdout, stderr bytes.Buffer
	result, err := t.runCLI(ctx, args, &stdout, &stderr)
	if err != nil {
		return fmt.Errorf("sandbox run %s failed: %w (%s)", sandboxID, err, strings.TrimSpace(stderr.String()))
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(stderr.String() + stdout.String())
		if isDirectSandboxAlreadyExists(sandboxID, detail) {
			return fmt.Errorf("%w: %s", errSandboxAlreadyExists, detail)
		}
		return fmt.Errorf("sandbox run %s exited %d: %s", sandboxID, result.ExitCode, detail)
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
	args = append(args, "--", "/bin/sh", "-s")
	keys := make([]string, 0, len(opts.Env))
	for key := range opts.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var script strings.Builder
	if _, hasPath := opts.Env["PATH"]; !hasPath {
		fmt.Fprintf(&script, "export PATH=%s\n", shellQuote(defaultSandboxPath))
	}
	for _, key := range keys {
		fmt.Fprintf(&script, "export %s=%s\n", key, shellQuote(opts.Env[key]))
	}
	fmt.Fprintf(&script, "exec /bin/sh -c %s\n", shellQuote(command))
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	result, err := t.runCLIWithStdin(ctx, args, strings.NewReader(script.String()), stdout, stderr)
	if err != nil {
		return result.ExitCode, fmt.Errorf("sandbox exec failed: %w", err)
	}
	return result.ExitCode, nil
}

func (t *directTransport) Destroy(ctx context.Context, sandboxID, ownershipToken string) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	if ownershipToken != sandboxID {
		return errors.New("cloud-run-sandbox direct destroy ownership token does not match the sandbox ID")
	}
	args := append(t.baseArgs(), "delete", sandboxID, "--force")
	var stdout, stderr bytes.Buffer
	result, err := t.runCLI(ctx, args, &stdout, &stderr)
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("sandbox delete %s failed: %w (%s)", sandboxID, err, detail)
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(stderr.String() + stdout.String())
		if isDirectSandboxNotFound(sandboxID, detail) {
			return fmt.Errorf("%w: %s", errSandboxNotFound, detail)
		}
		return fmt.Errorf("sandbox delete %s exited %d: %s", sandboxID, result.ExitCode, detail)
	}
	return nil
}

func (t *directTransport) WriteFile(ctx context.Context, sandboxID, path, content string, appendContent bool) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	// Stream content over stdin so workspace data never appears in host process
	// arguments. The first chunk is atomically published; later chunks append to
	// that unique per-sync temporary archive.
	command := fmt.Sprintf("mkdir -p \"$(dirname %s)\" && cat >> %s", shellQuote(path), shellQuote(path))
	if !appendContent {
		tempPath := path + ".crabbox-upload"
		command = fmt.Sprintf("mkdir -p \"$(dirname %s)\" && cat > %s && mv -f %s %s", shellQuote(path), shellQuote(tempPath), shellQuote(tempPath), shellQuote(path))
	}
	command = "PATH=" + shellQuote(defaultSandboxPath) + "; export PATH; " + command
	args := append(t.baseArgs(), "exec", sandboxID, "--workdir", "/", "--", "/bin/sh", "-c", command)
	var stdout, stderr bytes.Buffer
	result, err := t.runCLIWithStdin(ctx, args, strings.NewReader(content), &stdout, &stderr)
	if err != nil {
		return fmt.Errorf("sandbox writeFile %s failed: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sandbox writeFile %s exited %d: %s", path, result.ExitCode, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return nil
}

func isDirectSandboxAlreadyExists(sandboxID, detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, strings.ToLower(sandboxID)) &&
		strings.Contains(lower, "sandbox") &&
		strings.Contains(lower, "already exists")
}

func isDirectSandboxNotFound(sandboxID, detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, strings.ToLower(sandboxID)) &&
		strings.Contains(lower, "sandbox") &&
		(strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist"))
}
