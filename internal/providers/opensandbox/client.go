package opensandbox

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
	PingSandbox(context.Context, string) error
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
	ID        string
	State     string
	Metadata  map[string]string
	ExpiresAt *time.Time
}

type runCommandRequest struct {
	Command     string
	Workdir     string
	Env         map[string]string
	TimeoutSecs int
}

type commandStreamEvent struct {
	Event      string
	Data       string
	Structured bool
}

type execdConnection struct {
	baseURL string
	headers map[string]string
}

var errOpenSandboxNotFound = errors.New("opensandbox not found")

type sdkOpenSandboxClient struct {
	cfg                    Config
	rt                     Runtime
	base                   string
	key                    string
	client                 *http.Client
	requestTimeoutOverride time.Duration
	execTimeoutOverride    time.Duration
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
	if client.Transport == nil {
		client.Transport = sdk.DefaultTransport()
	}
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
	httpClient := *c.client
	httpClient.Timeout = c.requestTimeout()
	return sdk.NewLifecycleClient(c.base+"/v1", c.key, sdk.WithHTTPClient(&httpClient))
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
	timeoutSecs := opts.TimeoutSecs
	if timeoutSecs <= 0 {
		// The low-level request treats nil as manual cleanup. Keep the fallback
		// bounded while covering Crabbox's default command budget.
		timeoutSecs = openSandboxExecTimeoutSecs
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
		Timeout:        &timeoutSecs,
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

func (c *sdkOpenSandboxClient) PingSandbox(ctx context.Context, sandboxID string) error {
	execd, err := c.execd(ctx, sandboxID)
	if err != nil {
		return err
	}
	return execd.Ping(ctx)
}

func (c *sdkOpenSandboxClient) readyTimeout() time.Duration {
	timeout := openSandboxReadyTimeout
	if lifetime := openSandboxLifetimeForConfig(c.cfg); lifetime > 0 && lifetime < timeout {
		timeout = lifetime
	}
	return timeout
}

func (c *sdkOpenSandboxClient) requestTimeout() time.Duration {
	if c.requestTimeoutOverride > 0 {
		return c.requestTimeoutOverride
	}
	return sdk.DefaultRequestTimeout
}

func (c *sdkOpenSandboxClient) waitForRunning(ctx context.Context, sandboxID string) (*sdk.SandboxInfo, error) {
	timeout := c.readyTimeout()
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
	timeout := c.readyTimeout()
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
	conn, err := c.resolveExecd(ctx, sandboxID)
	if err != nil {
		return 1, err
	}
	if timeout := c.execRequestTimeout(req.TimeoutSecs); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	exitCode := 0
	terminal := false
	outputState := commandOutputState{}
	err = c.runCommandStream(ctx, conn, sdk.RunCommandRequest{
		Command: strings.TrimSpace(req.Command),
		Cwd:     req.Workdir,
		Timeout: int64(req.TimeoutSecs) * int64(time.Second/time.Millisecond),
		Envs:    req.Env,
	}, func(event commandStreamEvent) error {
		result, err := c.handleCommandEventWithState(event, &outputState)
		if result.exitCode != nil {
			exitCode = *result.exitCode
		} else if result.errorEvent && exitCode == 0 {
			exitCode = 1
		}
		terminal = terminal || result.terminal
		return err
	})
	if err != nil {
		return exitCodeOrDefault(exitCode), fmt.Errorf("opensandbox run command: %w", err)
	}
	if !terminal {
		return 1, errors.New("opensandbox run command: stream ended before terminal event")
	}
	return exitCode, nil
}

func (c *sdkOpenSandboxClient) runCommandStream(ctx context.Context, conn execdConnection, req sdk.RunCommandRequest, handler func(commandStreamEvent) error) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("opensandbox marshal command request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(conn.baseURL, "/")+"/command", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("opensandbox create command request: %w", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+sdk.Version)
	for key, value := range conn.headers {
		request.Header.Set(key, value)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("opensandbox send command request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		return fmt.Errorf("opensandbox command request failed status=%d body=%s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return streamOpenSandboxCommand(ctx, response.Body, handler)
}

func streamOpenSandboxCommand(ctx context.Context, body io.Reader, handler func(commandStreamEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	current := commandStreamEvent{}
	dataLines := []string(nil)
	eventCount := 0
	dispatch := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		current.Data = strings.Join(dataLines, "\n")
		if err := handler(current); err != nil {
			return err
		}
		eventCount++
		current = commandStreamEvent{}
		dataLines = nil
		return nil
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			current.Structured = true
			dataLines = append(dataLines, line)
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(line), &probe) == nil && probe.Type != "" {
				current.Event = probe.Type
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "event":
			current.Event = value
		case "id":
			// IDs are not used by command execution.
		}
	}
	if err := dispatch(); err != nil {
		return err
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("opensandbox command stream read: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if eventCount == 0 {
		return errors.New("opensandbox command stream was empty")
	}
	return nil
}

func (c *sdkOpenSandboxClient) execRequestTimeout(timeoutSecs int) time.Duration {
	if c.execTimeoutOverride > 0 {
		return c.execTimeoutOverride
	}
	if timeoutSecs <= 0 {
		return 0
	}
	return time.Duration(timeoutSecs)*time.Second + openSandboxExecGrace
}

type commandEventResult struct {
	exitCode   *int
	errorEvent bool
	terminal   bool
}

type commandOutputState struct {
	stdout bool
	stderr bool
}

func (c *sdkOpenSandboxClient) handleCommandEvent(event commandStreamEvent) (commandEventResult, error) {
	return c.handleCommandEventWithState(event, &commandOutputState{})
}

func (c *sdkOpenSandboxClient) handleCommandEventWithState(event commandStreamEvent, outputState *commandOutputState) (commandEventResult, error) {
	if strings.TrimSpace(event.Data) == "" {
		return commandEventResult{}, nil
	}
	explicitType := strings.ToLower(strings.TrimSpace(event.Event))
	if !event.Structured {
		switch explicitType {
		case "stdout":
			_, err := io.WriteString(c.rt.Stdout, event.Data)
			return commandEventResult{}, err
		case "stderr":
			_, err := io.WriteString(c.rt.Stderr, event.Data)
			return commandEventResult{}, err
		}
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
		switch explicitType {
		case "result", "error", "execution_complete":
			return commandEventResult{}, fmt.Errorf("decode opensandbox %s event: %w", event.Event, err)
		case "stderr":
			_, writeErr := io.WriteString(c.rt.Stderr, event.Data)
			return commandEventResult{}, writeErr
		}
		_, writeErr := io.WriteString(c.rt.Stdout, event.Data)
		return commandEventResult{}, writeErr
	}
	if payload.Type == "" {
		switch explicitType {
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
	if payload.ExitCode != nil && (eventType == "result" || eventType == "execution_complete") {
		code := *payload.ExitCode
		return commandEventResult{exitCode: &code, terminal: true}, nil
	}
	switch eventType {
	case "stdout":
		err := writeCommandOutput(c.rt.Stdout, commandOutput(payload.Text, payload.Data), &outputState.stdout)
		return commandEventResult{}, err
	case "result":
		_, err := io.WriteString(c.rt.Stdout, commandOutput(payload.Text, payload.Data))
		return commandEventResult{}, err
	case "stderr":
		err := writeCommandOutput(c.rt.Stderr, commandOutput(payload.Text, payload.Data), &outputState.stderr)
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
			return commandEventResult{exitCode: &code, errorEvent: true, terminal: true}, nil
		}
		code := 1
		return commandEventResult{exitCode: &code, errorEvent: true, terminal: true}, nil
	case "execution_complete":
		if payload.ExitCode != nil {
			code := *payload.ExitCode
			return commandEventResult{exitCode: &code, terminal: true}, nil
		}
		return commandEventResult{terminal: true}, nil
	}
	return commandEventResult{}, nil
}

func writeCommandOutput(w io.Writer, value string, previous *bool) error {
	if *previous {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, value); err != nil {
		return err
	}
	*previous = true
	return nil
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
	conn, err := c.resolveExecd(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	return sdk.NewExecdClient(conn.baseURL, "", sdk.WithHTTPClient(c.client), sdk.WithHeaders(conn.headers)), nil
}

func (c *sdkOpenSandboxClient) resolveExecd(ctx context.Context, sandboxID string) (execdConnection, error) {
	useProxy := c.cfg.OpenSandbox.UseServerProxy
	endpoint, err := c.lifecycle().GetEndpoint(ctx, sandboxID, sdk.DefaultExecdPort, &useProxy)
	if err != nil {
		return execdConnection{}, fmt.Errorf("opensandbox resolve execd endpoint: %w", err)
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
	return execdConnection{baseURL: endpointURL, headers: headers}, nil
}

func sdkSandboxInfo(info *sdk.SandboxInfo) sandboxInfo {
	if info == nil {
		return sandboxInfo{}
	}
	return sandboxInfo{
		ID:        info.ID,
		State:     string(info.Status.State),
		Metadata:  cloneStringMap(info.Metadata),
		ExpiresAt: info.ExpiresAt,
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
