package superserve

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	defaultSuperserveRequestTimeout = 2 * time.Minute
	maxExecStreamCaptureBytes       = 16 * 1024
)

type superserveClient interface {
	BaseURL() string
	CreateSandbox(context.Context, createSandboxRequest) (superserveSandbox, error)
	ListSandboxes(context.Context, map[string]string) ([]superserveSandbox, error)
	GetSandbox(context.Context, string) (superserveSandbox, error)
	ActivateSandbox(context.Context, string) (sandboxAccess, error)
	UpdateSandboxMetadata(context.Context, string, map[string]string) (superserveSandbox, error)
	PauseSandbox(context.Context, string) (superserveSandbox, error)
	ResumeSandbox(context.Context, string) (sandboxAccess, error)
	DeleteSandbox(context.Context, string) error
	UploadFile(context.Context, *sandboxAccess, string, io.Reader) error
	Exec(context.Context, *sandboxAccess, execRequest, io.Writer, io.Writer) (execResult, error)
	Probe(context.Context) error
}

type httpSuperserveClient struct {
	http    *http.Client
	baseURL string
	apiKey  string
}

type createSandboxRequest struct {
	Name           string                   `json:"name,omitempty"`
	FromTemplate   string                   `json:"from_template,omitempty"`
	FromSnapshot   string                   `json:"from_snapshot,omitempty"`
	TimeoutSeconds int                      `json:"timeout_seconds,omitempty"`
	Metadata       map[string]string        `json:"metadata,omitempty"`
	Network        *createSandboxNetworkCfg `json:"network,omitempty"`
}

type createSandboxNetworkCfg struct {
	AllowOut []string `json:"allow_out,omitempty"`
	DenyOut  []string `json:"deny_out,omitempty"`
}

type updateSandboxRequest struct {
	Metadata map[string]string `json:"metadata"`
}

type superserveSandbox struct {
	ID       string            `json:"id"`
	Status   string            `json:"status,omitempty"`
	State    string            `json:"state,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type sandboxAccess struct {
	Sandbox     superserveSandbox
	AccessToken string
}

type execRequest struct {
	Command    string            `json:"command"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	// Superserve data-plane exec uses timeout_s; sandbox lifetime uses timeout_secs.
	TimeoutSecs int `json:"timeout_s,omitempty"`
}

type execResult struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
}

type execStreamEvent struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Finished bool   `json:"finished,omitempty"`
	Error    string `json:"error,omitempty"`
}

type superserveAPIError struct {
	StatusCode int
	err        error
}

func (e *superserveAPIError) Error() string { return e.err.Error() }
func (e *superserveAPIError) Unwrap() error { return e.err }

func newSuperserveClient(cfg Config, rt Runtime) (superserveClient, error) {
	baseURL, err := validateSuperserveBaseURL(cfg.Superserve.BaseURL)
	if err != nil {
		return nil, err
	}
	apiKey := firstNonEmpty(
		os.Getenv("CRABBOX_SUPERSERVE_API_KEY"),
		os.Getenv("SUPERSERVE_API_KEY"),
	)
	if apiKey == "" {
		return nil, exit(2, "provider=superserve needs an API key; load CRABBOX_SUPERSERVE_API_KEY or SUPERSERVE_API_KEY from a secret manager")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &httpSuperserveClient{http: secureSuperserveHTTPClient(httpClient, baseURL), baseURL: baseURL, apiKey: apiKey}, nil
}

func secureSuperserveHTTPClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameSuperserveOrigin(trusted, req.URL) {
			return fmt.Errorf("superserve refused cross-origin redirect to %s", req.URL.Redacted())
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

func sameSuperserveOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveSuperservePort(a) == effectiveSuperservePort(b)
}

func effectiveSuperservePort(value *url.URL) string {
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

func (c *httpSuperserveClient) BaseURL() string { return c.baseURL }

func (c *httpSuperserveClient) doJSON(ctx context.Context, method, apiPath string, body, out any) error {
	_, err := c.doJSONMaybeEmpty(ctx, method, apiPath, body, out, false)
	return err
}

func (c *httpSuperserveClient) doJSONMaybeEmpty(ctx context.Context, method, apiPath string, body, out any, allowEmpty bool) (bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, defaultSuperserveRequestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return false, fmt.Errorf("superserve marshal %s: %w", apiPath, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(requestCtx, method, c.baseURL+apiPath, reader)
	if err != nil {
		return false, fmt.Errorf("superserve request %s: %w", apiPath, err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("superserve %s %s: %w", method, apiPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, c.apiError(method, apiPath, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, fmt.Errorf("superserve decode %s: %w", apiPath, err)
	}
	return false, nil
}

func (c *httpSuperserveClient) CreateSandbox(ctx context.Context, req createSandboxRequest) (superserveSandbox, error) {
	var sb superserveSandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes", req, &sb); err != nil {
		return superserveSandbox{}, err
	}
	if sb.ID == "" {
		return superserveSandbox{}, exit(5, "superserve create returned no sandbox id")
	}
	return sb, nil
}

func (c *httpSuperserveClient) ListSandboxes(ctx context.Context, metadata map[string]string) ([]superserveSandbox, error) {
	values := url.Values{}
	for k, v := range metadata {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		values.Set("metadata."+k, v)
	}
	apiPath := "/sandboxes"
	if encoded := values.Encode(); encoded != "" {
		apiPath += "?" + encoded
	}
	var out []superserveSandbox
	if err := c.doJSON(ctx, http.MethodGet, apiPath, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *httpSuperserveClient) GetSandbox(ctx context.Context, id string) (superserveSandbox, error) {
	var sb superserveSandbox
	if err := c.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, &sb); err != nil {
		return superserveSandbox{}, err
	}
	return sb, nil
}

func (c *httpSuperserveClient) ActivateSandbox(ctx context.Context, id string) (sandboxAccess, error) {
	return c.postAccess(ctx, "/sandboxes/"+url.PathEscape(id)+"/activate")
}

func (c *httpSuperserveClient) UpdateSandboxMetadata(ctx context.Context, id string, metadata map[string]string) (superserveSandbox, error) {
	var sb superserveSandbox
	empty, err := c.doJSONMaybeEmpty(ctx, http.MethodPatch, "/sandboxes/"+url.PathEscape(id), updateSandboxRequest{Metadata: metadata}, &sb, true)
	if err != nil {
		return superserveSandbox{}, err
	}
	if empty {
		return c.GetSandbox(ctx, id)
	}
	if sb.ID == "" {
		sb.ID = id
	}
	return sb, nil
}

func (c *httpSuperserveClient) PauseSandbox(ctx context.Context, id string) (superserveSandbox, error) {
	var sb superserveSandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/pause", nil, &sb); err != nil {
		return superserveSandbox{}, err
	}
	if sb.ID == "" {
		sb.ID = id
	}
	return sb, nil
}

func (c *httpSuperserveClient) ResumeSandbox(ctx context.Context, id string) (sandboxAccess, error) {
	return c.postAccess(ctx, "/sandboxes/"+url.PathEscape(id)+"/resume")
}

func (c *httpSuperserveClient) DeleteSandbox(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil)
}

func (c *httpSuperserveClient) UploadFile(ctx context.Context, access *sandboxAccess, remotePath string, content io.Reader) error {
	if strings.TrimSpace(remotePath) == "" || !strings.HasPrefix(remotePath, "/") || strings.Contains(remotePath, "..") {
		return exit(2, "superserve file path must be absolute and must not contain '..': %q", remotePath)
	}
	seeker, canSeek := content.(io.Seeker)
	return c.withAccessRetry(ctx, access, func(token string) error {
		if canSeek {
			if _, err := seeker.Seek(0, io.SeekStart); err != nil {
				return exit(6, "rewind sync archive: %v", err)
			}
		}
		target, err := c.dataPlaneTarget(access.Sandbox.ID)
		if err != nil {
			return err
		}
		apiPath := "/files?path=" + url.QueryEscape(remotePath)
		requestCtx := ctx
		cancel := func() {}
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			requestCtx, cancel = context.WithTimeout(ctx, defaultSuperserveRequestTimeout)
		}
		defer cancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target.baseURL+apiPath, content)
		if err != nil {
			return fmt.Errorf("superserve file upload request: %w", err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Access-Token", token)
		for k, v := range target.headers {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("superserve file upload %s: %w", remotePath, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return c.apiError(http.MethodPost, "files?path="+remotePath, resp, token)
		}
		return nil
	})
}

func (c *httpSuperserveClient) Exec(ctx context.Context, access *sandboxAccess, req execRequest, stdout, stderr io.Writer) (execResult, error) {
	var result execResult
	err := c.withAccessRetry(ctx, access, func(token string) error {
		var err error
		result, err = c.execStream(ctx, access.Sandbox.ID, token, req, stdout, stderr)
		if isSuperserveUnsupportedStream(err) {
			result, err = c.execBuffered(ctx, access.Sandbox.ID, token, req)
			if err == nil {
				err = writeExecResultOutput(result, stdout, stderr)
			}
		}
		return err
	})
	return result, err
}

func (c *httpSuperserveClient) Probe(ctx context.Context) error {
	var raw json.RawMessage
	return c.doJSON(ctx, http.MethodGet, "/sandboxes", nil, &raw)
}

func (c *httpSuperserveClient) postAccess(ctx context.Context, apiPath string) (sandboxAccess, error) {
	var raw struct {
		AccessToken string            `json:"access_token"`
		Token       string            `json:"token"`
		Sandbox     superserveSandbox `json:"sandbox"`
		ID          string            `json:"id"`
		Status      string            `json:"status"`
		State       string            `json:"state"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := c.doJSON(ctx, http.MethodPost, apiPath, nil, &raw); err != nil {
		return sandboxAccess{}, err
	}
	token := blank(raw.AccessToken, raw.Token)
	if token == "" {
		return sandboxAccess{}, exit(5, "superserve %s returned no access token", apiPath)
	}
	sb := raw.Sandbox
	if sb.ID == "" {
		sb.ID = raw.ID
	}
	if sb.Status == "" {
		sb.Status = raw.Status
	}
	if sb.State == "" {
		sb.State = raw.State
	}
	if sb.Metadata == nil {
		sb.Metadata = raw.Metadata
	}
	return sandboxAccess{Sandbox: sb, AccessToken: token}, nil
}

func (c *httpSuperserveClient) withAccessRetry(ctx context.Context, access *sandboxAccess, send func(token string) error) error {
	if access == nil || strings.TrimSpace(access.Sandbox.ID) == "" {
		return exit(5, "superserve data-plane request needs an activated sandbox")
	}
	if strings.TrimSpace(access.AccessToken) == "" {
		return exit(5, "superserve activated sandbox %s returned no access token", access.Sandbox.ID)
	}
	err := send(access.AccessToken)
	if !isSuperserveUnauthorized(err) {
		return err
	}
	fresh, refreshErr := c.ActivateSandbox(ctx, access.Sandbox.ID)
	if refreshErr != nil {
		return refreshErr
	}
	access.AccessToken = fresh.AccessToken
	if fresh.Sandbox.ID != "" {
		access.Sandbox = fresh.Sandbox
	}
	return send(access.AccessToken)
}

type dataPlaneTarget struct {
	baseURL string
	headers map[string]string
}

func (c *httpSuperserveClient) dataPlaneTarget(sandboxID string) (dataPlaneTarget, error) {
	parsed, err := url.Parse(c.baseURL)
	if err != nil {
		return dataPlaneTarget{}, err
	}
	if isLoopbackHost(parsed.Hostname()) {
		return dataPlaneTarget{
			baseURL: strings.TrimRight(parsed.String(), "/"),
			headers: map[string]string{
				"X-Superserve-Sandbox-Id": sandboxID,
			},
		}, nil
	}
	sandboxHost, ok := deriveSuperserveSandboxHost(parsed.Hostname())
	if !ok {
		return dataPlaneTarget{}, exit(2, "provider=superserve cannot derive a data-plane sandbox host from base URL %s; use the production/staging Superserve API or a loopback development endpoint", c.baseURL)
	}
	if supportsSuperserveSharedHost(sandboxHost) {
		return dataPlaneTarget{
			baseURL: "https://" + sandboxHost,
			headers: map[string]string{
				"X-Superserve-Sandbox-Id": sandboxID,
			},
		}, nil
	}
	return dataPlaneTarget{baseURL: "https://" + dataPlaneSubdomainHost(sandboxID, sandboxHost)}, nil
}

func deriveSuperserveSandboxHost(apiHost string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(apiHost)) {
	case "api-staging.superserve.ai":
		return "staging-sandbox.superserve.ai", true
	case "api.superserve.ai":
		return "sandbox.superserve.ai", true
	default:
		// Match the official SDK: custom control-plane URLs use the production
		// sandbox data plane unless the caller is using a loopback endpoint.
		return "sandbox.superserve.ai", true
	}
}

func supportsSuperserveSharedHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "sandbox.superserve.ai", "staging-sandbox.superserve.ai":
		return true
	default:
		return false
	}
}

func dataPlaneSubdomainHost(sandboxID, sandboxHost string) string {
	return "boxd-" + strings.TrimSpace(sandboxID) + "." + strings.TrimSpace(sandboxHost)
}

func (c *httpSuperserveClient) execBuffered(ctx context.Context, sandboxID, token string, body execRequest) (execResult, error) {
	target, err := c.dataPlaneTarget(sandboxID)
	if err != nil {
		return execResult{}, err
	}
	var result execResult
	requestCtx, cancel := superserveExecRequestContext(ctx, body.TimeoutSecs)
	defer cancel()
	if err := c.doDataPlaneJSON(requestCtx, http.MethodPost, target, "/exec", token, body, &result, envSecretValues(body.Env)...); err != nil {
		return execResult{}, err
	}
	return result, nil
}

func (c *httpSuperserveClient) execStream(ctx context.Context, sandboxID, token string, body execRequest, stdout, stderr io.Writer) (execResult, error) {
	target, err := c.dataPlaneTarget(sandboxID)
	if err != nil {
		return execResult{}, err
	}
	requestCtx, cancel := superserveExecRequestContext(ctx, body.TimeoutSecs)
	defer cancel()
	var reader io.Reader
	buf, err := json.Marshal(body)
	if err != nil {
		return execResult{}, fmt.Errorf("superserve marshal /exec/stream: %w", err)
	}
	reader = bytes.NewReader(buf)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target.baseURL+"/exec/stream", reader)
	if err != nil {
		return execResult{}, fmt.Errorf("superserve request /exec/stream: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Access-Token", token)
	for k, v := range target.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return execResult{}, fmt.Errorf("superserve POST /exec/stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return execResult{}, &superserveStreamUnsupportedError{err: c.apiError(http.MethodPost, "/exec/stream", resp, append([]string{token}, envSecretValues(body.Env)...)...)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return execResult{}, c.apiError(http.MethodPost, "/exec/stream", resp, append([]string{token}, envSecretValues(body.Env)...)...)
	}
	return consumeSuperserveExecStream(resp.Body, stdout, stderr, envSecretValues(body.Env)...)
}

func (c *httpSuperserveClient) doDataPlaneJSON(ctx context.Context, method string, target dataPlaneTarget, apiPath, token string, body, out any, secrets ...string) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("superserve marshal %s: %w", apiPath, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.baseURL+apiPath, reader)
	if err != nil {
		return fmt.Errorf("superserve request %s: %w", apiPath, err)
	}
	req.Header.Set("X-Access-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range target.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("superserve %s %s: %w", method, apiPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.apiError(method, apiPath, resp, append([]string{token}, secrets...)...)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("superserve decode %s: %w", apiPath, err)
	}
	return nil
}

func superserveExecRequestContext(ctx context.Context, timeoutSecs int) (context.Context, context.CancelFunc) {
	if timeoutSecs > 0 {
		return context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second+5*time.Second)
	}
	return context.WithCancel(ctx)
}

type superserveStreamUnsupportedError struct {
	err error
}

func (e *superserveStreamUnsupportedError) Error() string { return e.err.Error() }
func (e *superserveStreamUnsupportedError) Unwrap() error { return e.err }

func isSuperserveUnsupportedStream(err error) bool {
	var streamErr *superserveStreamUnsupportedError
	return errors.As(err, &streamErr)
}

func consumeSuperserveExecStream(body io.Reader, stdout, stderr io.Writer, secrets ...string) (execResult, error) {
	var result execResult
	sawFinished := false
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var event execStreamEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			continue
		}
		if event.Stdout != "" {
			result.Stdout = appendBounded(result.Stdout, event.Stdout, maxExecStreamCaptureBytes)
			if _, err := io.WriteString(stdout, event.Stdout); err != nil {
				return execResult{}, fmt.Errorf("superserve write command stdout: %w", err)
			}
		}
		if event.Stderr != "" {
			result.Stderr = appendBounded(result.Stderr, event.Stderr, maxExecStreamCaptureBytes)
			if _, err := io.WriteString(stderr, event.Stderr); err != nil {
				return execResult{}, fmt.Errorf("superserve write command stderr: %w", err)
			}
		}
		if event.Finished {
			sawFinished = true
			result.ExitCode = event.ExitCode
			if event.Error != "" {
				streamErr := redactSuperserveSecrets(event.Error, secrets...)
				result.Stderr = appendBounded(result.Stderr, streamErr, maxExecStreamCaptureBytes)
				if _, err := io.WriteString(stderr, streamErr); err != nil {
					return execResult{}, fmt.Errorf("superserve write command stderr: %w", err)
				}
				if result.ExitCode == 0 {
					return result, exit(5, "superserve command stream failed: %s", streamErr)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return execResult{}, fmt.Errorf("superserve read /exec/stream: %w", err)
	}
	if !sawFinished {
		return execResult{}, exit(5, "superserve command stream ended without a finished event")
	}
	return result, nil
}

func appendBounded(current, chunk string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(chunk) >= limit {
		return chunk[len(chunk)-limit:]
	}
	if len(current)+len(chunk) <= limit {
		return current + chunk
	}
	combined := current + chunk
	return combined[len(combined)-limit:]
}

func writeExecResultOutput(result execResult, stdout, stderr io.Writer) error {
	if result.Stdout != "" {
		if _, err := io.WriteString(stdout, result.Stdout); err != nil {
			return fmt.Errorf("superserve write command stdout: %w", err)
		}
	}
	if result.Stderr != "" {
		if _, err := io.WriteString(stderr, result.Stderr); err != nil {
			return fmt.Errorf("superserve write command stderr: %w", err)
		}
	}
	return nil
}

func envSecretValues(env map[string]string) []string {
	secrets := make([]string, 0, len(env))
	for _, value := range env {
		if strings.TrimSpace(value) != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

func (c *httpSuperserveClient) apiError(method, apiPath string, resp *http.Response, secrets ...string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	var wrapped struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &wrapped) == nil {
		switch value := wrapped.Error.(type) {
		case string:
			msg = blank(value, blank(wrapped.Message, msg))
		case map[string]any:
			if message, ok := value["message"].(string); ok {
				msg = blank(message, blank(wrapped.Message, msg))
			}
		}
	}
	msg = redactSuperserveSecrets(msg, append([]string{c.apiKey}, secrets...)...)
	return &superserveAPIError{
		StatusCode: resp.StatusCode,
		err:        exit(5, "superserve %s %s failed: %s: %s", method, apiPath, resp.Status, msg),
	}
}

func isSuperserveNotFound(err error) bool {
	var apiErr *superserveAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func isSuperserveUnauthorized(err error) bool {
	var apiErr *superserveAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized
}

func redactSuperserveSecrets(value string, secrets ...string) string {
	redacted := value
	for _, secret := range secrets {
		if strings.TrimSpace(secret) != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[redacted]")
		}
	}
	redacted = redactJSONSecretField(redacted, "access_token")
	redacted = redactJSONSecretField(redacted, "token")
	return redacted
}

func redactJSONSecretField(value, key string) string {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"[^"]*"`)
	return pattern.ReplaceAllString(value, `"`+key+`":"[redacted]"`)
}

func superserveEndpointScope(baseURL string) string {
	digest := sha256.Sum256([]byte(baseURL))
	return "endpoint-sha256:" + hex.EncodeToString(digest[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
