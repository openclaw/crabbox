package opensandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

type openSandboxClient interface {
	BaseURL() string
	CreateSandbox(context.Context, createSandboxOptions) (sandboxInfo, error)
	ListSandboxes(context.Context, map[string]string) ([]sandboxInfo, error)
	GetSandbox(context.Context, string) (sandboxInfo, error)
	DeleteSandbox(context.Context, string) error
	ResumeSandbox(context.Context, string) error
	UploadFile(context.Context, string, string, io.Reader) error
	RunCommand(context.Context, string, runCommandRequest) (int, error)
	Probe(context.Context) error
}

type createSandboxOptions struct {
	Image          string
	TimeoutSecs    int
	CPU            string
	Memory         string
	Metadata       map[string]string
	SecureAccess   bool
	PlatformOS     string
	PlatformArch   string
	UseServerProxy bool
}

type sandboxInfo struct {
	ID       string
	State    string
	Metadata map[string]string
}

type runCommandRequest struct {
	Command     string
	Workdir     string
	Env         map[string]string
	TimeoutSecs int
}

var errOpenSandboxNotFound = errors.New("opensandbox not found")

type sdkOpenSandboxClient struct {
	cfg    Config
	rt     Runtime
	base   string
	key    string
	client *http.Client
}

func newOpenSandboxClient(cfg Config, rt Runtime) (openSandboxClient, error) {
	rawURL := strings.TrimSpace(cfg.OpenSandbox.APIURL)
	if rawURL == "" {
		return nil, exit(2, "provider=opensandbox needs a trusted API URL; set --opensandbox-api-url, CRABBOX_OPENSANDBOX_API_URL, or OPEN_SANDBOX_API_URL")
	}
	baseURL, err := validateOpenSandboxAPIURL(rawURL)
	if err != nil {
		return nil, err
	}
	apiKey := firstNonEmpty(
		os.Getenv("CRABBOX_OPENSANDBOX_API_KEY"),
		os.Getenv("OPEN_SANDBOX_API_KEY"),
	)
	if apiKey == "" {
		return nil, exit(2, "provider=opensandbox needs an API key; load CRABBOX_OPENSANDBOX_API_KEY or OPEN_SANDBOX_API_KEY from a secret manager")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &sdkOpenSandboxClient{
		cfg:    cfg,
		rt:     rt,
		base:   baseURL,
		key:    apiKey,
		client: secureOpenSandboxHTTPClient(httpClient, baseURL),
	}, nil
}

func validateOpenSandboxAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=opensandbox API URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=opensandbox API URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "provider=opensandbox API URL must use HTTPS except for loopback development endpoints")
	}
	host := canonicalOpenSandboxHostname(parsed.Hostname())
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
	if strings.HasSuffix(parsed.Path, "/v1") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/v1")
	}
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func canonicalOpenSandboxHostname(host string) string {
	if zoneAt := strings.Index(host, "%"); zoneAt > 0 && strings.Contains(host[:zoneAt], ":") {
		return strings.ToLower(host[:zoneAt]) + host[zoneAt:]
	}
	return strings.ToLower(host)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func secureOpenSandboxHTTPClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOpenSandboxOrigin(trusted, req.URL) {
			return fmt.Errorf("opensandbox refused cross-origin redirect to %s", req.URL.Redacted())
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func sameOpenSandboxOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveOpenSandboxPort(a) == effectiveOpenSandboxPort(b)
}

func effectiveOpenSandboxPort(value *url.URL) string {
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

func (c *sdkOpenSandboxClient) BaseURL() string { return c.base }

func (c *sdkOpenSandboxClient) lifecycle() *sdk.LifecycleClient {
	return sdk.NewLifecycleClient(c.base+"/v1", c.key, sdk.WithHTTPClient(c.client))
}

func (c *sdkOpenSandboxClient) config() sdk.ConnectionConfig {
	protocol := ""
	if parsed, err := url.Parse(c.base); err == nil {
		protocol = parsed.Scheme
	}
	return sdk.ConnectionConfig{
		Domain:         c.base,
		Protocol:       protocol,
		APIKey:         c.key,
		UseServerProxy: c.cfg.OpenSandbox.UseServerProxy,
		HTTPClient:     c.client,
		RequestTimeout: 30 * time.Second,
	}
}

func (c *sdkOpenSandboxClient) CreateSandbox(ctx context.Context, opts createSandboxOptions) (sandboxInfo, error) {
	limits := sdk.ResourceLimits{}
	if strings.TrimSpace(opts.CPU) != "" {
		limits["cpu"] = strings.TrimSpace(opts.CPU)
	}
	if strings.TrimSpace(opts.Memory) != "" {
		limits["memory"] = strings.TrimSpace(opts.Memory)
	}
	var timeout *int
	if opts.TimeoutSecs > 0 {
		timeout = &opts.TimeoutSecs
	}
	var platform *sdk.PlatformSpec
	if strings.TrimSpace(opts.PlatformOS) != "" || strings.TrimSpace(opts.PlatformArch) != "" {
		platform = &sdk.PlatformSpec{
			OS:   sdk.PlatformOS(strings.TrimSpace(opts.PlatformOS)),
			Arch: sdk.PlatformArch(strings.TrimSpace(opts.PlatformArch)),
		}
	}
	req := sdk.CreateSandboxRequest{
		Image:          &sdk.ImageSpec{URI: opts.Image},
		Entrypoint:     sdk.DefaultEntrypoint,
		Timeout:        timeout,
		ResourceLimits: limits,
		Metadata:       opts.Metadata,
		SecureAccess:   opts.SecureAccess,
		Platform:       platform,
	}
	info, err := c.lifecycle().CreateSandbox(ctx, req)
	if err != nil {
		return sandboxInfo{}, fmt.Errorf("opensandbox create sandbox: %w", err)
	}
	sandboxID := info.ID
	if info.Status.State != sdk.StateRunning {
		info, err = c.waitForRunning(ctx, info.ID)
		if err != nil {
			return sandboxInfo{}, c.cleanupCreateFailure(ctx, sandboxID, fmt.Errorf("opensandbox wait for running: %w", err))
		}
	}
	if err := c.waitUntilReady(ctx, sandboxID); err != nil {
		return sandboxInfo{}, c.cleanupCreateFailure(ctx, sandboxID, fmt.Errorf("opensandbox wait until ready: %w", err))
	}
	return sdkSandboxInfo(info), nil
}

func (c *sdkOpenSandboxClient) cleanupCreateFailure(ctx context.Context, sandboxID string, cause error) error {
	if strings.TrimSpace(sandboxID) == "" {
		return cause
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openSandboxCleanupTimeout)
	defer cancel()
	if err := c.lifecycle().DeleteSandbox(cleanupCtx, sandboxID); err != nil && !isOpenSandboxNotFound(err) {
		return fmt.Errorf("%w; cleanup opensandbox sandbox %s failed: %v", cause, sandboxID, err)
	}
	return cause
}

func (c *sdkOpenSandboxClient) ResumeSandbox(ctx context.Context, sandboxID string) error {
	if err := c.lifecycle().ResumeSandbox(ctx, sandboxID); err != nil {
		return fmt.Errorf("opensandbox resume sandbox: %w", err)
	}
	if _, err := c.waitForRunning(ctx, sandboxID); err != nil {
		return fmt.Errorf("opensandbox wait for resumed sandbox: %w", err)
	}
	if err := c.waitUntilReady(ctx, sandboxID); err != nil {
		return fmt.Errorf("opensandbox wait until resumed sandbox ready: %w", err)
	}
	return nil
}

func (c *sdkOpenSandboxClient) waitForRunning(ctx context.Context, sandboxID string) (*sdk.SandboxInfo, error) {
	timeout := time.Duration(sdk.DefaultReadyTimeoutSeconds) * time.Second
	waitCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	start := time.Now()
	for {
		if err := waitCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("sandbox %s did not reach Running state within %s", sandboxID, time.Since(start).Round(time.Millisecond))
			}
			return nil, fmt.Errorf("sandbox %s did not reach Running state: %w", sandboxID, err)
		}

		info, err := c.lifecycle().GetSandbox(waitCtx, sandboxID)
		if err != nil {
			return nil, fmt.Errorf("get sandbox status: %w", err)
		}
		if info.Status.State == sdk.StateRunning {
			return info, nil
		}
		if info.Status.State == sdk.StateFailed || info.Status.State == sdk.StateTerminated {
			return nil, fmt.Errorf("sandbox %s entered terminal state: %s (%s)", sandboxID, info.Status.State, info.Status.Reason)
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *sdkOpenSandboxClient) waitUntilReady(ctx context.Context, sandboxID string) error {
	timeout := time.Duration(sdk.DefaultReadyTimeoutSeconds) * time.Second
	interval := sdk.DefaultHealthCheckPollingInterval
	waitCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	start := time.Now()
	var lastErr error
	for {
		execd, err := c.execd(waitCtx, sandboxID)
		if err == nil {
			err = execd.Ping(waitCtx)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if waitCtx.Err() != nil {
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("sandbox %s did not become ready within %s: %w", sandboxID, time.Since(start).Round(time.Millisecond), lastErr)
			}
			return fmt.Errorf("sandbox %s did not become ready: %w", sandboxID, waitCtx.Err())
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(interval):
		}
	}
}

func (c *sdkOpenSandboxClient) ListSandboxes(ctx context.Context, metadata map[string]string) ([]sandboxInfo, error) {
	resp, err := c.lifecycle().ListSandboxes(ctx, sdk.ListOptions{Metadata: metadata, PageSize: 100})
	if err != nil {
		return nil, fmt.Errorf("opensandbox list sandboxes: %w", err)
	}
	out := make([]sandboxInfo, 0, len(resp.Items))
	for i := range resp.Items {
		out = append(out, sdkSandboxInfo(&resp.Items[i]))
	}
	return out, nil
}

func (c *sdkOpenSandboxClient) GetSandbox(ctx context.Context, sandboxID string) (sandboxInfo, error) {
	info, err := c.lifecycle().GetSandbox(ctx, sandboxID)
	if err != nil {
		return sandboxInfo{}, fmt.Errorf("opensandbox get sandbox: %w", err)
	}
	return sdkSandboxInfo(info), nil
}

func (c *sdkOpenSandboxClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	if err := c.lifecycle().DeleteSandbox(ctx, sandboxID); err != nil {
		return fmt.Errorf("opensandbox delete sandbox: %w", err)
	}
	return nil
}

func (c *sdkOpenSandboxClient) UploadFile(ctx context.Context, sandboxID, remotePath string, body io.Reader) error {
	execd, err := c.execd(ctx, sandboxID)
	if err != nil {
		return err
	}
	if err := execd.UploadFile(ctx, body, sdk.UploadFileOptions{Metadata: sdk.FileMetadata{Path: remotePath}}); err != nil {
		return fmt.Errorf("opensandbox upload %s: %w", remotePath, err)
	}
	return nil
}

func (c *sdkOpenSandboxClient) RunCommand(ctx context.Context, sandboxID string, req runCommandRequest) (int, error) {
	execd, err := c.execd(ctx, sandboxID)
	if err != nil {
		return 1, err
	}
	exitCode := 0
	err = execd.RunCommand(ctx, sdk.RunCommandRequest{
		Command: strings.TrimSpace(req.Command),
		Cwd:     req.Workdir,
		Timeout: int64(req.TimeoutSecs) * int64(time.Second/time.Millisecond),
		Envs:    req.Env,
	}, func(event sdk.StreamEvent) error {
		result, err := c.handleCommandEvent(event)
		if result.exitCode != nil {
			exitCode = *result.exitCode
		} else if result.errorEvent && exitCode == 0 {
			exitCode = 1
		}
		return err
	})
	if err != nil {
		return exitCodeOrDefault(exitCode), fmt.Errorf("opensandbox run command: %w", err)
	}
	return exitCode, nil
}

type commandEventResult struct {
	exitCode   *int
	errorEvent bool
}

func (c *sdkOpenSandboxClient) handleCommandEvent(event sdk.StreamEvent) (commandEventResult, error) {
	if strings.TrimSpace(event.Data) == "" {
		return commandEventResult{}, nil
	}
	var payload struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		ExitCode *int   `json:"exit_code,omitempty"`
		Error    *struct {
			EValue string `json:"evalue,omitempty"`
		} `json:"error,omitempty"`
		EValue string `json:"evalue,omitempty"`
	}
	if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
		if strings.EqualFold(event.Event, "stderr") {
			_, writeErr := io.WriteString(c.rt.Stderr, event.Data)
			return commandEventResult{}, writeErr
		}
		_, writeErr := io.WriteString(c.rt.Stdout, event.Data)
		return commandEventResult{}, writeErr
	}
	if payload.Type == "" {
		switch strings.ToLower(event.Event) {
		case "stdout":
			_, err := io.WriteString(c.rt.Stdout, event.Data)
			return commandEventResult{}, err
		case "stderr":
			_, err := io.WriteString(c.rt.Stderr, event.Data)
			return commandEventResult{}, err
		}
	}
	eventType := payload.Type
	if eventType == "" {
		eventType = event.Event
	}
	if payload.ExitCode != nil && eventType != "error" {
		code := *payload.ExitCode
		return commandEventResult{exitCode: &code}, nil
	}
	switch eventType {
	case "stdout", "result":
		_, err := io.WriteString(c.rt.Stdout, commandOutput(payload.Text, payload.Data))
		return commandEventResult{}, err
	case "stderr":
		_, err := io.WriteString(c.rt.Stderr, commandOutput(payload.Text, payload.Data))
		return commandEventResult{}, err
	case "error":
		value := payload.EValue
		if payload.Error != nil && payload.Error.EValue != "" {
			value = payload.Error.EValue
		}
		if value != "" {
			_, _ = io.WriteString(c.rt.Stderr, value)
		}
		if code, ok := parseExitCode(value); ok {
			return commandEventResult{exitCode: &code, errorEvent: true}, nil
		}
		code := 1
		return commandEventResult{exitCode: &code, errorEvent: true}, nil
	case "execution_complete":
		if payload.ExitCode != nil {
			code := *payload.ExitCode
			return commandEventResult{exitCode: &code}, nil
		}
	}
	return commandEventResult{}, nil
}

func commandOutput(text, data string) string {
	if text != "" {
		return text
	}
	return data
}

func (c *sdkOpenSandboxClient) Probe(ctx context.Context) error {
	if _, err := c.lifecycle().ListSandboxes(ctx, sdk.ListOptions{PageSize: 1}); err != nil {
		return fmt.Errorf("opensandbox probe: %w", err)
	}
	return nil
}

func (c *sdkOpenSandboxClient) execd(ctx context.Context, sandboxID string) (*sdk.ExecdClient, error) {
	useProxy := c.cfg.OpenSandbox.UseServerProxy
	endpoint, err := c.lifecycle().GetEndpoint(ctx, sandboxID, sdk.DefaultExecdPort, &useProxy)
	if err != nil {
		return nil, fmt.Errorf("opensandbox resolve execd endpoint: %w", err)
	}
	conn := c.config()
	endpointURL := conn.RewriteEndpointURL(endpoint.Endpoint)
	if !strings.HasPrefix(endpointURL, "http") {
		endpointURL = conn.GetProtocol() + "://" + endpointURL
	}
	headers := cloneStringMap(endpoint.Headers)
	if c.cfg.OpenSandbox.UseServerProxy && headers["X-EXECD-ACCESS-TOKEN"] == "" && c.key != "" {
		headers["X-EXECD-ACCESS-TOKEN"] = c.key
	}
	return sdk.NewExecdClient(endpointURL, "", sdk.WithHTTPClient(c.client), sdk.WithHeaders(headers)), nil
}

func sdkSandboxInfo(info *sdk.SandboxInfo) sandboxInfo {
	if info == nil {
		return sandboxInfo{}
	}
	return sandboxInfo{
		ID:       info.ID,
		State:    string(info.Status.State),
		Metadata: cloneStringMap(info.Metadata),
	}
}

func isOpenSandboxNotFound(err error) bool {
	var apiErr *sdk.APIError
	return errors.Is(err, errOpenSandboxNotFound) || (errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func parseExitCode(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	var code int
	if _, err := fmt.Sscanf(value, "%d", &code); err == nil {
		return code, true
	}
	return 0, false
}

func exitCodeOrDefault(code int) int {
	if code == 0 {
		return 1
	}
	return code
}
