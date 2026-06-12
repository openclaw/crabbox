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
	"strconv"
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

type ambiguousOpenSandboxCreateError struct {
	cause error
}

func (e *ambiguousOpenSandboxCreateError) Error() string { return e.cause.Error() }
func (e *ambiguousOpenSandboxCreateError) Unwrap() error { return e.cause }

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
	baseURL  string
	rawQuery string
	headers  map[string]string
}

type openSandboxQueryTransport struct {
	base     http.RoundTripper
	rawQuery string
}

type openSandboxRedirectTransport struct {
	base http.RoundTripper
}

func (t openSandboxRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return response, nil
	}
	location := response.Header.Get("Location")
	if location == "" {
		return response, nil
	}
	destination, err := req.URL.Parse(location)
	if err != nil {
		response.Body.Close()
		return nil, errors.New("opensandbox received an invalid redirect location")
	}
	if sameOpenSandboxOrigin(req.URL, destination) {
		return response, nil
	}
	response.Body.Close()
	return nil, fmt.Errorf("opensandbox refused cross-origin redirect to %s://%s", destination.Scheme, destination.Host)
}

func (t openSandboxQueryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	requestURL := *req.URL
	if requestURL.RawQuery == "" {
		requestURL.RawQuery = t.rawQuery
	} else if t.rawQuery != "" {
		requestURL.RawQuery = t.rawQuery + "&" + requestURL.RawQuery
	}
	cloned.URL = &requestURL
	return t.base.RoundTrip(cloned)
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
		client: secureOpenSandboxHTTPClient(httpClient),
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

func secureOpenSandboxHTTPClient(source *http.Client) *http.Client {
	client := *source
	if client.Transport == nil {
		client.Transport = sdk.DefaultTransport()
	}
	client.Transport = openSandboxRedirectTransport{base: client.Transport}
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
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
	platformOS, platformArch, err := openSandboxPlatform(opts.PlatformOS, opts.PlatformArch)
	if err != nil {
		return sandboxInfo{}, err
	}
	var platform *sdk.PlatformSpec
	if platformOS != "" {
		platform = &sdk.PlatformSpec{
			OS:   sdk.PlatformOS(platformOS),
			Arch: sdk.PlatformArch(platformArch),
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
	if err := ctx.Err(); err != nil {
		return sandboxInfo{}, fmt.Errorf("opensandbox create sandbox: %w", err)
	}
	info, err := c.lifecycle().CreateSandbox(ctx, req)
	if err != nil {
		createErr := fmt.Errorf("opensandbox create sandbox: %w", err)
		if isOpenSandboxAmbiguousCreateError(err) {
			return sandboxInfo{}, &ambiguousOpenSandboxCreateError{cause: createErr}
		}
		return sandboxInfo{}, createErr
	}
	if strings.TrimSpace(info.ID) == "" {
		return sandboxInfo{}, &ambiguousOpenSandboxCreateError{
			cause: errors.New("opensandbox create sandbox: successful response omitted sandbox id"),
		}
	}
	sandboxID := info.ID
	readyCtx, cancel := c.readinessContext(ctx, info.ExpiresAt)
	defer cancel()
	if info.Status.State != sdk.StateRunning {
		info, err = c.waitForRunning(readyCtx, info.ID)
		if err != nil {
			return sandboxInfo{}, c.cleanupCreateFailure(ctx, sandboxID, fmt.Errorf("opensandbox wait for running: %w", err))
		}
	}
	if info.ExpiresAt == nil || info.ExpiresAt.IsZero() {
		info, err = c.lifecycle().GetSandbox(readyCtx, sandboxID)
		if err != nil {
			return sandboxInfo{}, c.cleanupCreateFailure(ctx, sandboxID, fmt.Errorf("opensandbox refresh sandbox expiration: %w", err))
		}
		if info.Status.State != sdk.StateRunning {
			refreshedCtx, refreshedCancel := c.readinessContext(readyCtx, info.ExpiresAt)
			info, err = c.waitForRunning(refreshedCtx, sandboxID)
			refreshedCancel()
			if err != nil {
				return sandboxInfo{}, c.cleanupCreateFailure(ctx, sandboxID, fmt.Errorf("opensandbox wait for running after expiration refresh: %w", err))
			}
		}
	}
	execdCtx, execdCancel := c.readinessContext(readyCtx, info.ExpiresAt)
	defer execdCancel()
	if err := c.waitUntilReady(execdCtx, sandboxID); err != nil {
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
	readyCtx, cancel := c.readinessContext(ctx, nil)
	defer cancel()
	info, err := c.waitForRunning(readyCtx, sandboxID)
	if err != nil {
		return fmt.Errorf("opensandbox wait for resumed sandbox: %w", err)
	}
	execdCtx, execdCancel := c.readinessContext(readyCtx, info.ExpiresAt)
	defer execdCancel()
	if err := c.waitUntilReady(execdCtx, sandboxID); err != nil {
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

func (c *sdkOpenSandboxClient) readinessContext(ctx context.Context, expiresAt *time.Time) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(c.readyTimeout())
	if expiresAt != nil && !expiresAt.IsZero() && expiresAt.Before(deadline) {
		deadline = *expiresAt
	}
	if parentDeadline, ok := ctx.Deadline(); ok && !deadline.Before(parentDeadline) {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

func (c *sdkOpenSandboxClient) waitForRunning(ctx context.Context, sandboxID string) (*sdk.SandboxInfo, error) {
	start := time.Now()
	var expiresAt *time.Time
	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("sandbox %s did not reach Running state within %s", sandboxID, time.Since(start).Round(time.Millisecond))
			}
			return nil, fmt.Errorf("sandbox %s did not reach Running state: %w", sandboxID, err)
		}
		if expiresAt != nil && !expiresAt.After(time.Now()) {
			return nil, fmt.Errorf("sandbox %s expired before reaching Running state", sandboxID)
		}

		requestCtx := ctx
		cancel := func() {}
		if expiresAt != nil {
			requestCtx, cancel = context.WithDeadline(ctx, *expiresAt)
		}
		info, err := c.lifecycle().GetSandbox(requestCtx, sandboxID)
		requestContextErr := requestCtx.Err()
		cancel()
		if err != nil {
			if expiresAt != nil && requestContextErr != nil && ctx.Err() == nil {
				return nil, fmt.Errorf("sandbox %s expired before reaching Running state", sandboxID)
			}
			return nil, fmt.Errorf("get sandbox status: %w", err)
		}
		if info.ExpiresAt != nil && !info.ExpiresAt.IsZero() {
			expiresAt = info.ExpiresAt
		}
		if info.Status.State == sdk.StateRunning {
			return info, nil
		}
		if info.Status.State == sdk.StateFailed || info.Status.State == sdk.StateTerminated {
			return nil, fmt.Errorf("sandbox %s entered terminal state: %s (%s)", sandboxID, info.Status.State, info.Status.Reason)
		}
		pollDelay := 2 * time.Second
		if expiresAt != nil {
			if remaining := time.Until(*expiresAt); remaining < pollDelay {
				pollDelay = remaining
			}
		}
		if pollDelay <= 0 {
			return nil, fmt.Errorf("sandbox %s expired before reaching Running state", sandboxID)
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollDelay):
		}
	}
}

func (c *sdkOpenSandboxClient) waitUntilReady(ctx context.Context, sandboxID string) error {
	interval := sdk.DefaultHealthCheckPollingInterval
	start := time.Now()
	var lastErr error
	for {
		execd, err := c.execd(ctx, sandboxID)
		if err == nil {
			err = execd.Ping(ctx)
		}
		if err == nil {
			return nil
		}
		if !isOpenSandboxReadinessPending(err) {
			return fmt.Errorf("sandbox %s readiness failed: %w", sandboxID, err)
		}
		lastErr = err
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("sandbox %s did not become ready within %s: %w", sandboxID, time.Since(start).Round(time.Millisecond), lastErr)
			}
			return fmt.Errorf("sandbox %s did not become ready: %w", sandboxID, ctx.Err())
		}
		select {
		case <-ctx.Done():
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
	if timeout := c.execRequestTimeout(req.TimeoutSecs); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	conn, err := c.resolveExecd(ctx, sandboxID)
	if err != nil {
		return 1, err
	}
	exitCode := 0
	terminal := false
	executionID := ""
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
		if result.executionID != "" {
			executionID = result.executionID
		}
		terminal = terminal || result.terminal
		return err
	})
	if err != nil {
		runErr := fmt.Errorf("opensandbox run command: %w", err)
		return exitCodeOrDefault(exitCode), c.interruptOpenSandboxCommand(ctx, conn, executionID, runErr)
	}
	if !terminal {
		runErr := errors.New("opensandbox run command: stream ended before terminal event")
		return 1, c.interruptOpenSandboxCommand(ctx, conn, executionID, runErr)
	}
	return exitCode, nil
}

func (c *sdkOpenSandboxClient) runCommandStream(ctx context.Context, conn execdConnection, req sdk.RunCommandRequest, handler func(commandStreamEvent) error) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("opensandbox marshal command request: %w", err)
	}
	commandURL, err := appendOpenSandboxExecdPath(conn.baseURL, "/command")
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, commandURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("opensandbox create command request: %w", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+sdk.Version)
	for key, value := range conn.headers {
		request.Header.Set(key, value)
	}
	response, err := c.execdHTTPClient(conn).Do(request)
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
	executionID string
	exitCode    *int
	errorEvent  bool
	terminal    bool
}

type commandOutputState struct {
	stdout commandLineState
	stderr commandLineState
}

type commandLineState struct {
	seen       bool
	terminated bool
}

func (c *sdkOpenSandboxClient) handleCommandEvent(event commandStreamEvent) (commandEventResult, error) {
	return c.handleCommandEventWithState(event, &commandOutputState{})
}

func (c *sdkOpenSandboxClient) handleCommandEventWithState(event commandStreamEvent, outputState *commandOutputState) (commandEventResult, error) {
	explicitType := strings.ToLower(strings.TrimSpace(event.Event))
	if !event.Structured {
		switch explicitType {
		case "stdout":
			err := writeCommandLine(c.rt.Stdout, event.Data, &outputState.stdout)
			return commandEventResult{}, err
		case "stderr":
			err := writeCommandLine(c.rt.Stderr, event.Data, &outputState.stderr)
			return commandEventResult{}, err
		}
	}
	if event.Data == "" {
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
		switch explicitType {
		case "result", "error", "execution_complete":
			return commandEventResult{}, fmt.Errorf("decode opensandbox %s event: %w", event.Event, err)
		case "stderr":
			writeErr := writeCommandLine(c.rt.Stderr, event.Data, &outputState.stderr)
			return commandEventResult{}, writeErr
		}
		writeErr := writeCommandLine(c.rt.Stdout, event.Data, &outputState.stdout)
		return commandEventResult{}, writeErr
	}
	if payload.Type == "" {
		switch explicitType {
		case "stdout":
			err := writeCommandLine(c.rt.Stdout, event.Data, &outputState.stdout)
			return commandEventResult{}, err
		case "stderr":
			err := writeCommandLine(c.rt.Stderr, event.Data, &outputState.stderr)
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
	case "init":
		return commandEventResult{executionID: payload.Text}, nil
	case "stdout":
		err := writeCommandLine(c.rt.Stdout, commandOutput(payload.Text, payload.Data), &outputState.stdout)
		return commandEventResult{}, err
	case "result":
		err := writeCommandLine(c.rt.Stdout, commandOutput(payload.Text, payload.Data), &outputState.stdout)
		return commandEventResult{}, err
	case "stderr":
		err := writeCommandLine(c.rt.Stderr, commandOutput(payload.Text, payload.Data), &outputState.stderr)
		return commandEventResult{}, err
	case "error":
		value := payload.EValue
		if payload.Error != nil && payload.Error.EValue != "" {
			value = payload.Error.EValue
		}
		if code, ok := parseExitCode(value); ok {
			return commandEventResult{exitCode: &code, errorEvent: true, terminal: true}, nil
		}
		if value != "" {
			_, _ = io.WriteString(c.rt.Stderr, value)
			if !strings.HasSuffix(value, "\n") {
				_, _ = io.WriteString(c.rt.Stderr, "\n")
			}
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

func writeCommandLine(w io.Writer, value string, state *commandLineState) error {
	// OpenSandbox stdout/stderr events are line records without required
	// terminators. The stream parser removes SSE framing before this point.
	if value == "" {
		if state.seen && !state.terminated {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		state.seen = true
		state.terminated = true
		return nil
	}
	if state.seen && !state.terminated {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, value); err != nil {
		return err
	}
	state.seen = true
	state.terminated = strings.HasSuffix(value, "\n")
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
	return c.execdForConnection(conn), nil
}

func (c *sdkOpenSandboxClient) execdForConnection(conn execdConnection) *sdk.ExecdClient {
	return sdk.NewExecdClient(conn.baseURL, "", sdk.WithHTTPClient(c.execdHTTPClient(conn)), sdk.WithHeaders(conn.headers))
}

func (c *sdkOpenSandboxClient) interruptOpenSandboxCommand(ctx context.Context, conn execdConnection, executionID string, cause error) error {
	if strings.TrimSpace(executionID) == "" {
		return cause
	}
	interruptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openSandboxInterruptTimeout)
	defer cancel()
	if err := c.execdForConnection(conn).InterruptCommand(interruptCtx, executionID); err != nil {
		return fmt.Errorf("%w; interrupt opensandbox command %s failed: %v", cause, executionID, err)
	}
	return cause
}

func (c *sdkOpenSandboxClient) resolveExecd(ctx context.Context, sandboxID string) (execdConnection, error) {
	useProxy := c.cfg.OpenSandbox.UseServerProxy
	endpoint, err := c.lifecycle().GetEndpoint(ctx, sandboxID, sdk.DefaultExecdPort, &useProxy)
	if err != nil {
		return execdConnection{}, fmt.Errorf("opensandbox resolve execd endpoint: %w", err)
	}
	conn := c.config()
	endpointURL := conn.RewriteEndpointURL(endpoint.Endpoint)
	endpointURL, rawQuery, err := validateOpenSandboxExecdURL(endpointURL, conn.GetProtocol())
	if err != nil {
		return execdConnection{}, err
	}
	headers, err := normalizeOpenSandboxEndpointHeaders(endpoint.Headers)
	if err != nil {
		return execdConnection{}, err
	}
	execdAuthHeader := http.CanonicalHeaderKey("X-EXECD-ACCESS-TOKEN")
	if c.cfg.OpenSandbox.UseServerProxy && headers[execdAuthHeader] == "" && c.key != "" {
		if headers == nil {
			headers = make(map[string]string, 1)
		}
		headers[execdAuthHeader] = c.key
	}
	return execdConnection{baseURL: endpointURL, rawQuery: rawQuery, headers: headers}, nil
}

func normalizeOpenSandboxEndpointHeaders(values map[string]string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	headers := make(map[string]string, len(values))
	for key, value := range values {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if canonical == "" {
			return nil, exit(5, "opensandbox execd endpoint returned an invalid empty header name")
		}
		if existing, ok := headers[canonical]; ok && existing != value {
			return nil, exit(5, "opensandbox execd endpoint returned conflicting values for header %s", canonical)
		}
		headers[canonical] = value
	}
	return headers, nil
}

func validateOpenSandboxExecdURL(raw, defaultProtocol string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = strings.TrimSpace(defaultProtocol) + "://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", "", exit(5, "opensandbox execd endpoint must be an absolute HTTP(S) URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", exit(5, "opensandbox execd endpoint must use HTTP(S)")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return "", "", exit(5, "opensandbox execd endpoint must not contain userinfo or a fragment")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", "", exit(5, "opensandbox execd endpoint host %q must use HTTPS unless it is loopback", parsed.Host)
	}
	rawQuery := parsed.RawQuery
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	return strings.TrimRight(parsed.String(), "/"), rawQuery, nil
}

func (c *sdkOpenSandboxClient) execdHTTPClient(conn execdConnection) *http.Client {
	client := *c.client
	client.Transport = openSandboxQueryTransport{
		base:     client.Transport,
		rawQuery: conn.rawQuery,
	}
	return &client
}

func appendOpenSandboxExecdPath(baseURL, suffix string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("opensandbox parse execd base URL: %w", err)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(suffix, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
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
	code, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return code, true
}

func exitCodeOrDefault(code int) int {
	if code == 0 {
		return 1
	}
	return code
}
