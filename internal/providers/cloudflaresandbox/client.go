package cloudflaresandbox

import (
	"bytes"
	"context"
	"encoding/json"
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
		http:    httpClient,
	}, nil
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
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes", true, req, &out); err != nil {
		return sandboxSummary{}, err
	}
	return out, nil
}

func (c *client) GetSandbox(ctx context.Context, id string) (sandboxSummary, error) {
	var out sandboxSummary
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), true, nil, &out); err != nil {
		return sandboxSummary{}, err
	}
	if out.ID == "" {
		out.ID = id
	}
	return out, nil
}

func (c *client) ListSandboxes(ctx context.Context) ([]sandboxSummary, error) {
	var out []sandboxSummary
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes", true, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) ListRunning(ctx context.Context) ([]sandboxSummary, error) {
	var out []sandboxSummary
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/running", true, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) DeleteSandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), true, nil, nil)
}

func (c *client) Exec(ctx context.Context, id string, req execRequest, stdout, stderr io.Writer) (execResult, error) {
	var out execResult
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", true, req, &out); err != nil {
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

func (c *client) Persist(ctx context.Context, id string, req persistRequest) (persistResponse, error) {
	var out persistResponse
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/persist", true, req, &out); err != nil {
		return persistResponse{}, err
	}
	return out, nil
}

func (c *client) Hydrate(ctx context.Context, id string, req hydrateRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/hydrate", true, req, nil)
}

func (c *client) WarmPool(ctx context.Context) (warmPoolResponse, error) {
	var out warmPoolResponse
	if err := c.do(ctx, http.MethodGet, "/v1/warm-pool", true, nil, &out); err != nil {
		return warmPoolResponse{}, err
	}
	return out, nil
}

func (c *client) do(ctx context.Context, method, route string, authenticated bool, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s encode %s: %w", providerName, route, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, joinURLPath(c.baseURL, route), reader)
	if err != nil {
		return fmt.Errorf("%s request %s: %w", providerName, route, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authenticated && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s %s: %s", providerName, method, route, c.redact(err.Error()))
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("%s read %s %s: %w", providerName, method, route, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s %s failed: %s: %s", providerName, method, route, resp.Status, c.redact(string(data)))
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s decode %s %s: %w", providerName, method, route, err)
	}
	return nil
}

func joinURLPath(base, route string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return base + route
	}
	parsed.Path = path.Join(parsed.Path, route)
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
