package cloudflaredynamicworkers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type loaderAPI interface {
	Readiness(context.Context) (readinessResponse, error)
	Run(context.Context, runRequest) (runResponse, error)
	Status(context.Context, string) (runStatus, error)
	Delete(context.Context, string) error
}

type client struct {
	baseURL string
	token   string
	http    *http.Client
}

type readinessResponse struct {
	OK                bool              `json:"ok"`
	Runner            string            `json:"runner"`
	LoaderBinding     bool              `json:"loaderBinding"`
	CompatibilityDate string            `json:"compatibilityDate,omitempty"`
	Egress            string            `json:"egress,omitempty"`
	Limits            limits            `json:"limits,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

type runRequest struct {
	ID                 string            `json:"id,omitempty"`
	CacheMode          string            `json:"cacheMode"`
	Module             moduleSource      `json:"module"`
	CompatibilityDate  string            `json:"compatibilityDate,omitempty"`
	CompatibilityFlags []string          `json:"compatibilityFlags,omitempty"`
	Egress             string            `json:"egress"`
	Limits             limits            `json:"limits,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	TimeoutMS          int64             `json:"timeoutMs,omitempty"`
}

type moduleSource struct {
	Name   string `json:"name,omitempty"`
	Source string `json:"source"`
}

type limits struct {
	CPUMs       int `json:"cpuMs,omitempty"`
	Subrequests int `json:"subrequests,omitempty"`
}

type runResponse struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	ExitCode int               `json:"exitCode"`
	Stdout   string            `json:"stdout,omitempty"`
	Stderr   string            `json:"stderr,omitempty"`
	Body     string            `json:"body,omitempty"`
	Logs     string            `json:"logs,omitempty"`
	Timing   map[string]int64  `json:"timing,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type runStatus struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	CreatedAt string            `json:"createdAt,omitempty"`
	UpdatedAt string            `json:"updatedAt,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type apiError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *apiError) Error() string {
	if strings.TrimSpace(e.Body) == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

const defaultResponseHeaderTimeout = 30 * time.Second

var newLoaderAPI = func(cfg Config, rt Runtime) (loaderAPI, error) {
	baseURL, err := loaderURL(cfg)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(cfg.CloudflareDynamicWorkers.Token)
	if token == "" {
		return nil, exit(2, "%s requires cloudflareDynamicWorkers.token or CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN", providerName)
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	return &client{baseURL: baseURL, token: token, http: httpClient}, nil
}

func loaderURL(cfg Config) (string, error) {
	raw := strings.TrimSpace(cfg.CloudflareDynamicWorkers.LoaderURL)
	if raw == "" {
		return "", exit(2, "%s requires cloudflareDynamicWorkers.loaderUrl or CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL", providerName)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", exit(2, "%s loader URL %q is invalid", providerName, raw)
	}
	if parsed.User != nil {
		return "", exit(2, "%s loader URL must not include userinfo", providerName)
	}
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return "", exit(2, "%s loader URL %q must use https unless it targets localhost", providerName, raw)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "%s loader URL %q must not include query or fragment components", providerName, raw)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	return &http.Client{Transport: transport}
}

func (c *client) Readiness(ctx context.Context) (readinessResponse, error) {
	var out readinessResponse
	err := c.doJSON(ctx, http.MethodGet, "/v1/readiness", nil, &out)
	return out, err
}

func (c *client) Run(ctx context.Context, req runRequest) (runResponse, error) {
	var out runResponse
	err := c.doJSON(ctx, http.MethodPost, "/v1/runs", req, &out)
	return out, err
}

func (c *client) Status(ctx context.Context, id string) (runStatus, error) {
	var out runStatus
	err := c.doJSON(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *client) Delete(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/runs/"+url.PathEscape(id), nil, nil)
}

func (c *client) doJSON(ctx context.Context, method, endpoint string, input any, output any) error {
	var body io.Reader
	if input != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(input); err != nil {
			return err
		}
		body = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.responseError(resp)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(output)
}

func (c *client) responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	text := strings.TrimSpace(string(body))
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		switch {
		case strings.TrimSpace(payload.Error) != "":
			text = payload.Error
		case strings.TrimSpace(payload.Message) != "":
			text = payload.Message
		}
	}
	if text == "" {
		text = resp.Status
	}
	text = redactSensitive(text, c.token)
	return &apiError{StatusCode: resp.StatusCode, Status: providerName + " API " + resp.Status, Body: text}
}

func redactSensitive(text, token string) string {
	out := text
	if token = strings.TrimSpace(token); token != "" {
		out = strings.ReplaceAll(out, token, "<redacted>")
	}
	fields := strings.Fields(out)
	for i, field := range fields {
		lower := strings.ToLower(strings.Trim(field, `"'`))
		if strings.HasPrefix(lower, "bearer") && i+1 < len(fields) {
			fields[i+1] = "<redacted>"
		}
	}
	out = strings.Join(fields, " ")
	if strings.Contains(strings.ToLower(out), "authorization: bearer ") {
		out = redactBearerLine(out)
	}
	return out
}

func redactBearerLine(text string) string {
	parts := strings.Split(text, "\n")
	for i, part := range parts {
		lower := strings.ToLower(part)
		if idx := strings.Index(lower, "authorization: bearer "); idx >= 0 {
			parts[i] = part[:idx] + "Authorization: Bearer <redacted>"
		}
	}
	return strings.Join(parts, "\n")
}

func providerError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", providerName, action, err)
}
