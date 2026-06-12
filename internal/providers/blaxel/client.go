package blaxel

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
	"path"
	"strings"
)

type Client interface {
	BaseURL() string
	Probe(context.Context) error
	CreateSandbox(context.Context, CreateSandboxRequest) (Sandbox, error)
	GetSandbox(context.Context, string) (Sandbox, error)
	ListSandboxes(context.Context, ListSandboxesRequest) (ListSandboxesResult, error)
	UpdateSandboxLabels(context.Context, string, map[string]string) (Sandbox, error)
	DeleteSandbox(context.Context, string) error
	ExecuteProcess(context.Context, string, ExecuteProcessRequest) (Process, error)
	GetProcess(context.Context, string, string) (Process, error)
	GetProcessLogs(context.Context, string, string) (ProcessLogs, error)
	StopProcess(context.Context, string, string) error
	WriteFile(context.Context, string, WriteFileRequest) error
	UploadFile(context.Context, string, string, io.Reader) error
	GetDirectoryTree(context.Context, string, string) (DirectoryTree, error)
}

type CreateSandboxRequest struct {
	Name       string            `json:"name,omitempty"`
	Image      string            `json:"image,omitempty"`
	Region     string            `json:"region,omitempty"`
	MemoryMB   int               `json:"memoryMb,omitempty"`
	TTL        string            `json:"ttl,omitempty"`
	IdleTTL    string            `json:"idleTtl,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type ListSandboxesRequest struct {
	Cursor         string
	Limit          int
	ShowTerminated bool
	Labels         map[string]string
}

type ListSandboxesResult struct {
	Sandboxes []Sandbox
	Next      string
}

type Sandbox struct {
	ID        string            `json:"id,omitempty"`
	Name      string            `json:"name,omitempty"`
	Status    string            `json:"status,omitempty"`
	Region    string            `json:"region,omitempty"`
	Image     string            `json:"image,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Endpoint  string            `json:"endpoint,omitempty"`
	CreatedAt string            `json:"createdAt,omitempty"`
	UpdatedAt string            `json:"updatedAt,omitempty"`
}

type ExecuteProcessRequest struct {
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	WorkingDir  string            `json:"workingDir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	TimeoutSecs int               `json:"timeoutSecs,omitempty"`
}

type Process struct {
	ID       string `json:"id,omitempty"`
	Status   string `json:"status,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

type ProcessLogs struct {
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
}

type WriteFileRequest struct {
	Path      string `json:"path,omitempty"`
	Content   string `json:"content,omitempty"`
	Encoding  string `json:"encoding,omitempty"`
	Directory bool   `json:"directory,omitempty"`
}

type DirectoryTree struct {
	Path    string          `json:"path,omitempty"`
	Entries []DirectoryItem `json:"entries,omitempty"`
}

type DirectoryItem struct {
	Path string `json:"path,omitempty"`
	Type string `json:"type,omitempty"`
	Size int64  `json:"size,omitempty"`
}

type restClient struct {
	base      string
	apiKey    string
	workspace string
	version   string
	http      *http.Client
}

func newBlaxelClient(cfg Config, rt Runtime) (Client, error) {
	baseURL := strings.TrimSpace(cfg.Blaxel.APIURL)
	if baseURL == "" {
		baseURL = defaultAPIURL
	}
	baseURL, err := ValidateAPIURL(baseURL)
	if err != nil {
		return nil, err
	}
	apiKey := BlaxelAPIKey(cfg)
	if apiKey == "" {
		return nil, exit(2, "provider=blaxel needs an API key; load CRABBOX_BLAXEL_API_KEY or BL_API_KEY from a secret manager")
	}
	workspace := strings.TrimSpace(cfg.Blaxel.Workspace)
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &restClient{
		base:      baseURL,
		apiKey:    apiKey,
		workspace: workspace,
		version:   defaultAPIVersion,
		http:      secureHTTPClient(httpClient),
	}, nil
}

func BlaxelAPIKey(cfg Config) string {
	return strings.TrimSpace(cfg.Blaxel.APIKey)
}

func ValidateAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=blaxel API URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=blaxel API URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "provider=blaxel API URL must use HTTPS except for loopback development endpoints")
	}
	host := canonicalHostname(parsed.Hostname())
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

func validateBlaxelConfig(cfg Config) error {
	if _, err := ValidateAPIURL(blank(cfg.Blaxel.APIURL, defaultAPIURL)); err != nil {
		return err
	}
	if cfg.Blaxel.MemoryMB < 0 {
		return exit(2, "blaxel memory-mb must be >= 0")
	}
	if cfg.Blaxel.ExecTimeoutSecs < 0 {
		return exit(2, "blaxel execTimeoutSecs must be non-negative")
	}
	if strings.TrimSpace(cfg.Blaxel.Workdir) != "" && !strings.HasPrefix(strings.TrimSpace(cfg.Blaxel.Workdir), "/") {
		return exit(2, "blaxel workdir must be absolute")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func canonicalHostname(host string) string {
	if zoneAt := strings.Index(host, "%"); zoneAt > 0 && strings.Contains(host[:zoneAt], ":") {
		return strings.ToLower(host[:zoneAt]) + host[zoneAt:]
	}
	return strings.ToLower(host)
}

func secureHTTPClient(source *http.Client) *http.Client {
	client := *source
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if len(via) > 0 && !sameOrigin(via[len(via)-1].URL, req.URL) {
			return fmt.Errorf("blaxel refused cross-origin redirect to %s://%s", req.URL.Scheme, req.URL.Host)
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
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

func (c *restClient) BaseURL() string { return c.base }

func (c *restClient) Probe(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/v1/sandboxes", url.Values{"limit": []string{"1"}}, nil, nil)
	return err
}

func (c *restClient) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (Sandbox, error) {
	var out Sandbox
	_, err := c.do(ctx, http.MethodPost, "/v1/sandboxes", nil, req, &out)
	return out, err
}

func (c *restClient) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	_, err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

func (c *restClient) ListSandboxes(ctx context.Context, req ListSandboxesRequest) (ListSandboxesResult, error) {
	values := url.Values{}
	if req.Cursor != "" {
		values.Set("cursor", req.Cursor)
	}
	if req.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", req.Limit))
	}
	if req.ShowTerminated {
		values.Set("showTerminated", "true")
	}
	body, err := c.do(ctx, http.MethodGet, "/v1/sandboxes", values, nil, nil)
	if err != nil {
		return ListSandboxesResult{}, err
	}
	return parseSandboxList(body)
}

func (c *restClient) UpdateSandboxLabels(ctx context.Context, id string, labels map[string]string) (Sandbox, error) {
	var out Sandbox
	_, err := c.do(ctx, http.MethodPatch, "/v1/sandboxes/"+url.PathEscape(id), nil, map[string]any{"labels": labels}, &out)
	return out, err
}

func (c *restClient) DeleteSandbox(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil, nil)
	return err
}

func (c *restClient) ExecuteProcess(ctx context.Context, sandbox string, req ExecuteProcessRequest) (Process, error) {
	var out Process
	_, err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(sandbox)+"/processes", nil, req, &out)
	return out, err
}

func (c *restClient) GetProcess(ctx context.Context, sandbox, process string) (Process, error) {
	var out Process
	_, err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(sandbox)+"/processes/"+url.PathEscape(process), nil, nil, &out)
	return out, err
}

func (c *restClient) GetProcessLogs(ctx context.Context, sandbox, process string) (ProcessLogs, error) {
	var out ProcessLogs
	_, err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(sandbox)+"/processes/"+url.PathEscape(process)+"/logs", nil, nil, &out)
	return out, err
}

func (c *restClient) StopProcess(ctx context.Context, sandbox, process string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(sandbox)+"/processes/"+url.PathEscape(process)+"/stop", nil, nil, nil)
	return err
}

func (c *restClient) WriteFile(ctx context.Context, sandbox string, req WriteFileRequest) error {
	_, err := c.do(ctx, http.MethodPut, "/v1/sandboxes/"+url.PathEscape(sandbox)+"/filesystem", nil, req, nil)
	return err
}

func (c *restClient) UploadFile(ctx context.Context, sandbox, remotePath string, reader io.Reader) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	req := WriteFileRequest{Path: remotePath, Content: base64.StdEncoding.EncodeToString(body), Encoding: "base64"}
	return c.WriteFile(ctx, sandbox, req)
}

func (c *restClient) GetDirectoryTree(ctx context.Context, sandbox, remotePath string) (DirectoryTree, error) {
	var out DirectoryTree
	values := url.Values{}
	if remotePath != "" {
		values.Set("path", remotePath)
	}
	_, err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(sandbox)+"/filesystem/tree", values, nil, &out)
	return out, err
}

func (c *restClient) do(ctx context.Context, method, endpoint string, values url.Values, request any, response any) ([]byte, error) {
	var body io.Reader
	if request != nil {
		data, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	reqURL, err := url.Parse(c.base)
	if err != nil {
		return nil, err
	}
	reqURL.Path = path.Join(reqURL.Path, endpoint)
	if strings.HasSuffix(endpoint, "/") && !strings.HasSuffix(reqURL.Path, "/") {
		reqURL.Path += "/"
	}
	reqURL.RawQuery = values.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, method, reqURL.String(), body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Blaxel-Version", c.version)
	if c.workspace != "" {
		httpReq.Header.Set("X-Blaxel-Workspace", c.workspace)
	}
	if request != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, redactError(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apiError{StatusCode: resp.StatusCode, Body: redactString(string(data))}
	}
	if response != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, response); err != nil {
			return nil, err
		}
	}
	return data, nil
}

type listEnvelope struct {
	Data []Sandbox `json:"data"`
	Meta struct {
		NextCursor string `json:"nextCursor"`
		Cursor     string `json:"cursor"`
		Next       string `json:"next"`
	} `json:"meta"`
}

func parseSandboxList(data []byte) (ListSandboxesResult, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return ListSandboxesResult{}, nil
	}
	if trimmed[0] == '[' {
		var sandboxes []Sandbox
		if err := json.Unmarshal(trimmed, &sandboxes); err != nil {
			return ListSandboxesResult{}, err
		}
		return ListSandboxesResult{Sandboxes: sandboxes}, nil
	}
	var envelope listEnvelope
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return ListSandboxesResult{}, err
	}
	return ListSandboxesResult{
		Sandboxes: envelope.Data,
		Next:      firstNonEmpty(envelope.Meta.NextCursor, envelope.Meta.Next, envelope.Meta.Cursor),
	}, nil
}

type apiError struct {
	StatusCode int
	Body       string
}

func (e apiError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("blaxel API request failed status=%d", e.StatusCode)
	}
	return fmt.Sprintf("blaxel API request failed status=%d body=%s", e.StatusCode, e.Body)
}

func redactError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(redactString(err.Error()))
}

func redactString(value string) string {
	out := value
	for _, secret := range []string{
		os.Getenv("CRABBOX_BLAXEL_API_KEY"),
		os.Getenv("BL_API_KEY"),
	} {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			out = strings.ReplaceAll(out, secret, "<redacted>")
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
