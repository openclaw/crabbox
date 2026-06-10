package opencomputer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ocAPIClient is a thin REST client for the OpenComputer control plane. The
// provider talks to the API directly (no `oc` CLI dependency): the API key
// travels only in the X-API-Key header and all payloads (command env, file
// content) travel in request bodies, so nothing sensitive ever reaches argv.
type ocAPIClient struct {
	http    *http.Client
	baseURL string
	apiKey  string
}

// ocFileConfig mirrors the subset of ~/.oc/config.json that `oc config set`
// persists. Crabbox reuses that credential rather than introducing a new
// secret in its own config.
type ocFileConfig struct {
	APIURL string `json:"api_url"`
	APIKey string `json:"api_key"`
}

// sandbox is the API representation of a sandbox (subset we use).
type sandbox struct {
	ID       string            `json:"sandboxID"`
	Status   string            `json:"status"`
	Metadata map[string]string `json:"metadata,omitempty"`
	CPUCount int               `json:"cpuCount,omitempty"`
	MemoryMB int               `json:"memoryMB,omitempty"`
}

type sandboxListResponse struct {
	Sandboxes []sandbox `json:"sandboxes"`
}

// createSandboxRequest is the POST /api/sandboxes body. CPU/memory must form an
// allowed tier (e.g. 1/1024, 2/8192); they are sent only when both are set, so
// an unset sizing falls back to the service default tier.
type createSandboxRequest struct {
	Metadata map[string]string `json:"metadata,omitempty"`
	Timeout  int               `json:"timeout,omitempty"`
	CPUCount int               `json:"cpuCount,omitempty"`
	MemoryMB int               `json:"memoryMB,omitempty"`
	DiskMB   int               `json:"diskMB,omitempty"`
}

// execRunRequest is the POST /api/sandboxes/:id/exec/run body. Env travels in
// the body (`envs`), never argv.
type execRunRequest struct {
	Cmd     string            `json:"cmd"`
	Args    []string          `json:"args,omitempty"`
	Envs    map[string]string `json:"envs,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

type execRunResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// newOCAPIClient resolves the API URL and key. Key precedence:
// CRABBOX_OPENCOMPUTER_API_KEY, OPENCOMPUTER_API_KEY, then the `oc` CLI config
// (~/.oc/config.json). Returns an error when no key can be found.
func newOCAPIClient(cfg Config, rt Runtime) (*ocAPIClient, error) {
	fileCfg := readOCFileConfig()
	// API URL precedence: an explicit Crabbox setting (config/flag/env), then the
	// `oc` CLI config file's api_url, then the built-in default. This honors
	// `oc config set api-url <custom>` for users who never set it in Crabbox.
	baseURL := strings.TrimRight(blank(strings.TrimSpace(cfg.OpenComputer.APIURL), blank(strings.TrimSpace(fileCfg.APIURL), defaultAPIURL)), "/")
	apiKey := firstNonEmpty(
		os.Getenv("CRABBOX_OPENCOMPUTER_API_KEY"),
		os.Getenv("OPENCOMPUTER_API_KEY"),
		fileCfg.APIKey,
	)
	if apiKey == "" {
		return nil, exit(2, "provider=opencomputer needs an API key; set it with `oc config set api-key <key>` or CRABBOX_OPENCOMPUTER_API_KEY")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &ocAPIClient{http: httpClient, baseURL: baseURL, apiKey: apiKey}, nil
}

func (c *ocAPIClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	return req, nil
}

// doJSON sends an optional JSON body and decodes a JSON response into out
// (out may be nil). Non-2xx responses become errors carrying the response body.
func (c *ocAPIClient) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opencomputer marshal %s: %w", path, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := c.newRequest(ctx, method, path, reader)
	if err != nil {
		return fmt.Errorf("opencomputer request %s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opencomputer %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(method, path, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("opencomputer decode %s: %w", path, err)
	}
	return nil
}

func (c *ocAPIClient) createSandbox(ctx context.Context, req createSandboxRequest) (sandbox, error) {
	var sb sandbox
	if err := c.doJSON(ctx, http.MethodPost, "/api/sandboxes", req, &sb); err != nil {
		return sandbox{}, err
	}
	if sb.ID == "" {
		return sandbox{}, exit(5, "opencomputer create returned no sandbox id")
	}
	return sb, nil
}

func (c *ocAPIClient) getSandbox(ctx context.Context, id string) (sandbox, error) {
	var sb sandbox
	if err := c.doJSON(ctx, http.MethodGet, "/api/sandboxes/"+url.PathEscape(id), nil, &sb); err != nil {
		return sandbox{}, err
	}
	return sb, nil
}

func (c *ocAPIClient) listSandboxes(ctx context.Context) ([]sandbox, error) {
	// The list endpoint may return either a bare array of sandboxes or an object
	// wrapping them under "sandboxes"; accept both.
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/api/sandboxes", nil, &raw); err != nil {
		return nil, err
	}
	var arr []sandbox
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var wrapped sandboxListResponse
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		return wrapped.Sandboxes, nil
	}
	return nil, exit(5, "opencomputer list: unexpected response shape")
}

func (c *ocAPIClient) killSandbox(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/sandboxes/"+url.PathEscape(id), nil, nil)
}

func (c *ocAPIClient) execRun(ctx context.Context, id string, req execRunRequest) (execRunResult, error) {
	var res execRunResult
	if err := c.doJSON(ctx, http.MethodPost, "/api/sandboxes/"+url.PathEscape(id)+"/exec/run", req, &res); err != nil {
		return execRunResult{}, err
	}
	return res, nil
}

// uploadFile writes content to remotePath inside the sandbox via
// `PUT /api/sandboxes/{id}/files?path=...`; the payload is the raw request body.
func (c *ocAPIClient) uploadFile(ctx context.Context, id, remotePath string, content io.Reader) error {
	path := fmt.Sprintf("/api/sandboxes/%s/files?path=%s", url.PathEscape(id), url.QueryEscape(remotePath))
	req, err := c.newRequest(ctx, http.MethodPut, path, content)
	if err != nil {
		return fmt.Errorf("opencomputer file upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opencomputer file upload %s: %w", remotePath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(http.MethodPut, "files?path="+remotePath, resp)
	}
	return nil
}

func apiError(method, path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	// Surface the API's {"error": "..."} message when present.
	var wrapped struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &wrapped) == nil && wrapped.Error != "" {
		msg = wrapped.Error
	}
	return exit(5, "opencomputer %s %s failed: %s: %s", method, path, resp.Status, msg)
}

func readOCFileConfig() ocFileConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		return ocFileConfig{}
	}
	data, err := os.ReadFile(filepath.Join(home, ".oc", "config.json"))
	if err != nil {
		return ocFileConfig{}
	}
	var cfg ocFileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ocFileConfig{}
	}
	return cfg
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
