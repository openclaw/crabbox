package cloudflaresandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

type bridgeClient interface {
	Health(context.Context) (healthResponse, error)
	OpenAPI(context.Context) (openAPIResponse, error)
	CreateSandbox(context.Context, createSandboxRequest) (sandboxSummary, error)
	GetSandbox(context.Context, string) (sandboxSummary, error)
	ListSandboxes(context.Context) ([]sandboxSummary, error)
	ListRunning(context.Context) ([]sandboxSummary, error)
	DeleteSandbox(context.Context, string) error
	Exec(context.Context, string, execRequest, io.Writer, io.Writer) (execResult, error)
	UploadFile(context.Context, string, string, io.Reader) error
	Persist(context.Context, string, persistRequest) (persistResponse, error)
	Hydrate(context.Context, string, hydrateRequest) error
	WarmPool(context.Context) (warmPoolResponse, error)
}

type client struct {
	baseURL string
	token   string
	http    *http.Client
}

type healthResponse struct {
	OK      bool   `json:"ok"`
	Status  string `json:"status,omitempty"`
	Version string `json:"version,omitempty"`
}

type openAPIResponse struct {
	OpenAPI string `json:"openapi,omitempty"`
	Info    struct {
		Title   string `json:"title,omitempty"`
		Version string `json:"version,omitempty"`
	} `json:"info,omitempty"`
}

type sandboxSummary struct {
	ID       string            `json:"id"`
	Name     string            `json:"name,omitempty"`
	Status   string            `json:"status,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type createSandboxRequest struct {
	Name     string            `json:"name,omitempty"`
	Workdir  string            `json:"workdir,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type execRequest struct {
	Command     string            `json:"command"`
	WorkingDir  string            `json:"workingDir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	TimeoutSecs int               `json:"timeoutSecs,omitempty"`
}

type execResult struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode"`
}

type writeFileResponse struct {
	OK bool `json:"ok,omitempty"`
}

type persistRequest struct {
	Path string `json:"path,omitempty"`
}

type persistResponse struct {
	ID  string `json:"id,omitempty"`
	URL string `json:"url,omitempty"`
}

type hydrateRequest struct {
	ID   string `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
}

type warmPoolResponse struct {
	Ready int `json:"ready,omitempty"`
	Total int `json:"total,omitempty"`
}

func newBridgeClient(cfg Config, rt Runtime) (bridgeClient, error) {
	baseURL, err := bridgeURL(cfg)
	if err != nil {
		return nil, err
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &client{
		baseURL: baseURL,
		token:   strings.TrimSpace(cfg.CloudflareSandbox.Token),
		http:    secureCloudflareSandboxHTTPClient(httpClient, baseURL),
	}, nil
}

func secureCloudflareSandboxHTTPClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameCloudflareSandboxOrigin(trusted, req.URL) {
			return fmt.Errorf("%s refused cross-origin redirect to %s", providerName, cloudflareSandboxRedirectOrigin(req.URL))
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

func sameCloudflareSandboxOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveCloudflareSandboxPort(a) == effectiveCloudflareSandboxPort(b)
}

func effectiveCloudflareSandboxPort(value *url.URL) string {
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

func cloudflareSandboxRedirectOrigin(value *url.URL) string {
	if value == nil || value.Scheme == "" || value.Host == "" {
		return "<redacted>"
	}
	return value.Scheme + "://" + value.Host
}

func (c *client) Health(ctx context.Context) (healthResponse, error) {
	var out healthResponse
	if err := c.do(ctx, http.MethodGet, "/health", false, nil, &out); err != nil {
		return healthResponse{}, err
	}
	return out, nil
}

func (c *client) OpenAPI(ctx context.Context) (openAPIResponse, error) {
	var out openAPIResponse
	if err := c.do(ctx, http.MethodGet, "/v1/openapi.json", true, nil, &out); err != nil {
		return openAPIResponse{}, err
	}
	return out, nil
}

func (c *client) CreateSandbox(ctx context.Context, req createSandboxRequest) (sandboxSummary, error) {
	var out sandboxSummary
	if err := c.do(ctx, http.MethodPost, "/v1/sandbox", true, req, &out); err != nil {
		return sandboxSummary{}, err
	}
	return out, nil
}

func (c *client) GetSandbox(ctx context.Context, id string) (sandboxSummary, error) {
	var out sandboxSummary
	if err := c.do(ctx, http.MethodGet, "/v1/sandbox/"+url.PathEscape(id), true, nil, &out); err != nil {
		return sandboxSummary{}, err
	}
	if out.ID == "" {
		out.ID = id
	}
	return out, nil
}

func (c *client) ListSandboxes(ctx context.Context) ([]sandboxSummary, error) {
	var out []sandboxSummary
	if err := c.do(ctx, http.MethodGet, "/v1/sandbox", true, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) ListRunning(ctx context.Context) ([]sandboxSummary, error) {
	var out []sandboxSummary
	if err := c.do(ctx, http.MethodGet, "/v1/sandbox/running", true, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) DeleteSandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandbox/"+url.PathEscape(id), true, nil, nil)
}

func (c *client) Exec(ctx context.Context, id string, req execRequest, stdout, stderr io.Writer) (execResult, error) {
	resp, err := c.doRequest(ctx, http.MethodPost, "/v1/sandbox/"+url.PathEscape(id)+"/exec", true, req, "application/json")
	if err != nil {
		return execResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return execResult{}, fmt.Errorf("%s POST /v1/sandbox/%s/exec failed: %s: %s", providerName, url.PathEscape(id), resp.Status, c.redact(string(data)))
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return c.parseExecSSE(resp.Body, stdout, stderr)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return execResult{}, fmt.Errorf("%s read exec response: %w", providerName, readErr)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return execResult{}, errors.New("cloudflare-sandbox exec returned empty response")
	}
	var out execResult
	if err := json.Unmarshal(data, &out); err != nil {
		return execResult{}, err
	}
	if stdout != nil && out.Stdout != "" {
		_, _ = io.WriteString(stdout, out.Stdout)
	}
	if stderr != nil && out.Stderr != "" {
		_, _ = io.WriteString(stderr, out.Stderr)
	}
	return out, nil
}

type execSSEData struct {
	Type     string `json:"type,omitempty"`
	Stream   string `json:"stream,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	Chunk    string `json:"chunk,omitempty"`
	Data     string `json:"data,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Message  string `json:"message,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (c *client) parseExecSSE(body io.Reader, stdout, stderr io.Writer) (execResult, error) {
	var result execResult
	sawExit := false
	eventType := ""
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			eventType = ""
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		ev := strings.TrimSpace(eventType)
		eventType = ""
		var frame execSSEData
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			return fmt.Errorf("decode cloudflare-sandbox exec SSE event: %w", err)
		}
		kind := strings.ToLower(blank(frame.Type, ev))
		stream := strings.ToLower(frame.Stream)
		switch kind {
		case "stdout", "stderr", "output":
			payload := firstNonEmpty(frame.Chunk, frame.Data, frame.Stdout, frame.Stderr)
			decoded, err := decodeSSEPayload(payload, frame.Encoding)
			if err != nil {
				return err
			}
			if stream == "" {
				if kind == "stderr" || frame.Stderr != "" {
					stream = "stderr"
				} else {
					stream = "stdout"
				}
			}
			if stream == "stderr" {
				result.Stderr += decoded
				if stderr != nil {
					_, err = io.WriteString(stderr, decoded)
				}
			} else {
				result.Stdout += decoded
				if stdout != nil {
					_, err = io.WriteString(stdout, decoded)
				}
			}
			if err != nil {
				return fmt.Errorf("write cloudflare-sandbox %s: %w", stream, err)
			}
		case "exit", "result", "complete", "done":
			if frame.ExitCode == nil {
				return errors.New("cloudflare-sandbox exec SSE exit event missing exitCode")
			}
			result.ExitCode = *frame.ExitCode
			sawExit = true
		case "error":
			msg := firstNonEmpty(frame.Error, frame.Message, data)
			return fmt.Errorf("cloudflare-sandbox exec error: %s", c.redact(msg))
		default:
			return fmt.Errorf("unsupported cloudflare-sandbox exec SSE event %q", kind)
		}
		return nil
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return execResult{}, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			eventType = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return execResult{}, fmt.Errorf("read cloudflare-sandbox exec SSE: %w", err)
	}
	if err := flush(); err != nil {
		return execResult{}, err
	}
	if !sawExit {
		return execResult{}, errors.New("cloudflare-sandbox exec SSE returned no exit event")
	}
	return result, nil
}

func decodeSSEPayload(value, encoding string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.EqualFold(strings.TrimSpace(encoding), "base64") {
		return value, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("decode cloudflare-sandbox exec SSE base64 payload: %w", err)
	}
	return string(decoded), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (c *client) UploadFile(ctx context.Context, id, remotePath string, content io.Reader) error {
	route := "/v1/sandbox/" + url.PathEscape(id) + "/files/write?path=" + url.QueryEscape(remotePath) + "&encoding=raw"
	resp, err := c.doRequest(ctx, http.MethodPost, route, true, content, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("%s read %s: %w", providerName, route, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s POST %s failed: %s: %s", providerName, route, resp.Status, c.redact(string(data)))
	}
	return nil
}

func (c *client) Persist(ctx context.Context, id string, req persistRequest) (persistResponse, error) {
	var out persistResponse
	if err := c.do(ctx, http.MethodPost, "/v1/sandbox/"+url.PathEscape(id)+"/persist", true, req, &out); err != nil {
		return persistResponse{}, err
	}
	return out, nil
}

func (c *client) Hydrate(ctx context.Context, id string, req hydrateRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/sandbox/"+url.PathEscape(id)+"/hydrate", true, req, nil)
}

func (c *client) WarmPool(ctx context.Context) (warmPoolResponse, error) {
	var out warmPoolResponse
	if err := c.do(ctx, http.MethodGet, "/v1/warm-pool", true, nil, &out); err != nil {
		return warmPoolResponse{}, err
	}
	return out, nil
}

func (c *client) do(ctx context.Context, method, route string, authenticated bool, body any, out any) error {
	resp, err := c.doRequest(ctx, method, route, authenticated, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("%s read %s %s: %w", providerName, method, route, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.responseError(method, route, resp, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s decode %s %s: %w", providerName, method, route, err)
	}
	return nil
}

func (c *client) responseError(method, route string, resp *http.Response, data []byte) error {
	err := fmt.Errorf("%s %s %s failed: %s: %s", providerName, method, route, resp.Status, c.redact(string(data)))
	if resp.StatusCode == http.StatusNotFound {
		return &cloudflareSandboxNotFoundError{err: err}
	}
	return err
}

func (c *client) doRequest(ctx context.Context, method, route string, authenticated bool, body any, contentType string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		if existing, ok := body.(io.Reader); ok {
			reader = existing
		} else {
			data, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("%s encode %s: %w", providerName, route, err)
			}
			reader = bytes.NewReader(data)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, joinURLPath(c.baseURL, route), reader)
	if err != nil {
		return nil, fmt.Errorf("%s request %s: %w", providerName, route, err)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if body != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if authenticated && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		requestErr := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			requestErr = urlErr.Err
		}
		return nil, fmt.Errorf("%s %s %s: %s", providerName, method, route, c.redact(requestErr.Error()))
	}
	return resp, nil
}

func joinURLPath(base, route string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return base + route
	}
	routeURL, err := url.Parse(route)
	if err == nil {
		parsed.Path = path.Join(parsed.Path, routeURL.Path)
		parsed.RawQuery = routeURL.RawQuery
	} else {
		parsed.Path = path.Join(parsed.Path, route)
	}
	return parsed.String()
}

func redactSecrets(value string) string {
	out := value
	for _, secret := range []string{
		"CRABBOX_CLOUDFLARE_SANDBOX_TOKEN",
		"Authorization",
		"Bearer",
	} {
		out = strings.ReplaceAll(out, secret, "[redacted]")
	}
	return out
}

func (c *client) redact(value string) string {
	out := redactSecrets(value)
	if c != nil && strings.TrimSpace(c.token) != "" {
		out = strings.ReplaceAll(out, c.token, "[redacted]")
	}
	return out
}
