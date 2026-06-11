package opencomputer

import (
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
	"path/filepath"
	"strings"
	"time"
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
	Tags     map[string]string `json:"tags,omitempty"`
	CPUCount int               `json:"cpuCount,omitempty"`
	MemoryMB int               `json:"memoryMB,omitempty"`
}

// createSandboxRequest is the POST /api/sandboxes body. When only CPU or memory
// is set, OpenComputer infers the matching sizing value.
type createSandboxRequest struct {
	Metadata map[string]string `json:"metadata,omitempty"`
	Timeout  int               `json:"timeout,omitempty"`
	CPUCount int               `json:"cpuCount,omitempty"`
	MemoryMB int               `json:"memoryMB,omitempty"`
	DiskMB   int               `json:"diskMB,omitempty"`
	Burst    bool              `json:"burst,omitempty"`
}

type sandboxTagsResponse struct {
	Tags map[string]string `json:"tags"`
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

type ocAPIError struct {
	StatusCode int
	err        error
}

const (
	defaultOCControlRequestTimeout = 2 * time.Minute
	defaultOCExecRequestTimeout    = time.Hour
	ocExecRequestGrace             = 30 * time.Second
)

func (e *ocAPIError) Error() string { return e.err.Error() }
func (e *ocAPIError) Unwrap() error { return e.err }

// newOCAPIClient resolves the API URL and key. Key precedence:
// CRABBOX_OPENCOMPUTER_API_KEY, OPENCOMPUTER_API_KEY, then the `oc` CLI config
// (~/.oc/config.json). Returns an error when no key can be found.
func newOCAPIClient(cfg Config, rt Runtime) (*ocAPIClient, error) {
	fileCfg := readOCFileConfig()
	// API URL precedence: an explicit trusted Crabbox setting, then the `oc` CLI
	// config file's api_url, then the built-in default. Repository YAML cannot
	// populate cfg.OpenComputer.APIURL.
	baseURL, err := validateOCAPIURL(blank(strings.TrimSpace(cfg.OpenComputer.APIURL), blank(strings.TrimSpace(fileCfg.APIURL), defaultAPIURL)))
	if err != nil {
		return nil, err
	}
	apiKey := firstNonEmpty(
		os.Getenv("CRABBOX_OPENCOMPUTER_API_KEY"),
		os.Getenv("OPENCOMPUTER_API_KEY"),
		fileCfg.APIKey,
	)
	if apiKey == "" {
		return nil, exit(2, "provider=opencomputer needs an API key; load CRABBOX_OPENCOMPUTER_API_KEY from a secret manager or configure an existing oc CLI credential")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &ocAPIClient{http: secureOCAPIClient(httpClient, baseURL), baseURL: baseURL, apiKey: apiKey}, nil
}

func validateOCAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=opencomputer API URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=opencomputer API URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "provider=opencomputer API URL must use HTTPS except for loopback development endpoints")
	}
	host := canonicalOCHostname(parsed.Hostname())
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
	cleanPath := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(cleanPath, "/api") {
		cleanPath = strings.TrimSuffix(cleanPath, "/api")
	}
	parsed.Path = cleanPath
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func canonicalOCHostname(host string) string {
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

func secureOCAPIClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOCOrigin(trusted, req.URL) {
			return fmt.Errorf("opencomputer refused cross-origin redirect to %s", req.URL.Redacted())
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

func sameOCOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveOCPort(a) == effectiveOCPort(b)
}

func effectiveOCPort(value *url.URL) string {
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
	return c.doJSONWithTimeout(ctx, defaultOCControlRequestTimeout, method, path, body, out)
}

func (c *ocAPIClient) doJSONWithTimeout(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opencomputer marshal %s: %w", path, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := c.newRequest(requestCtx, method, path, reader)
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

func (c *ocAPIClient) probeSandboxes(ctx context.Context) error {
	var raw json.RawMessage
	return c.doJSON(ctx, http.MethodGet, "/api/sandboxes", nil, &raw)
}

func (c *ocAPIClient) killSandbox(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/sandboxes/"+url.PathEscape(id), nil, nil)
}

func (c *ocAPIClient) replaceSandboxTags(ctx context.Context, id string, tags map[string]string) error {
	return c.doJSON(ctx, http.MethodPut, "/api/sandboxes/"+url.PathEscape(id)+"/tags", tags, nil)
}

func (c *ocAPIClient) getSandboxTags(ctx context.Context, id string) (map[string]string, error) {
	var response sandboxTagsResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/sandboxes/"+url.PathEscape(id)+"/tags", nil, &response); err != nil {
		return nil, err
	}
	return response.Tags, nil
}

func (c *ocAPIClient) getSandboxWithTags(ctx context.Context, id string) (sandbox, error) {
	sb, err := c.getSandbox(ctx, id)
	if err != nil {
		return sandbox{}, err
	}
	sb.Tags, err = c.getSandboxTags(ctx, id)
	if err != nil {
		return sandbox{}, err
	}
	return sb, nil
}

func (c *ocAPIClient) execRun(ctx context.Context, id string, req execRunRequest) (execRunResult, error) {
	var res execRunResult
	timeout := defaultOCExecRequestTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	if err := c.doJSONWithTimeout(ctx, timeout+ocExecRequestGrace, http.MethodPost, "/api/sandboxes/"+url.PathEscape(id)+"/exec/run", req, &res); err != nil {
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
	return &ocAPIError{
		StatusCode: resp.StatusCode,
		err:        exit(5, "opencomputer %s %s failed: %s: %s", method, path, resp.Status, msg),
	}
}

func isOCNotFound(err error) bool {
	var apiErr *ocAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
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
