package azuredynamicsessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type azureDynamicSessionsAPI interface {
	CheckRunner(context.Context, string) error
	UploadFile(context.Context, string, string, string) error
	ExecStream(context.Context, string, azureDynamicSessionsExecRequest, io.Writer, io.Writer) (int, error)
	GetSession(context.Context, string) (azureDynamicSessionsSession, error)
	ListSessions(context.Context) ([]azureDynamicSessionsSession, error)
	DeleteSession(context.Context, string) error
}

type azureDynamicSessionsClient struct {
	endpoint             string
	managementAPIVersion string
	token                string
	httpClient           *http.Client
}

type azureDynamicSessionsExecRequest struct {
	Command   string            `json:"command"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMS int64             `json:"timeoutMs,omitempty"`
}

type azureDynamicSessionsExecEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

type azureDynamicSessionsSession struct {
	Identifier     string `json:"identifier"`
	ETag           string `json:"etag"`
	ExpireAt       string `json:"expireAt"`
	ExpiresAt      string `json:"expiresAt"`
	CreatedAt      string `json:"createdAt"`
	LastAccessedAt string `json:"lastAccessedAt"`
	Status         string `json:"status"`
	Name           string `json:"name"`
	ID             string `json:"id"`
	Properties     struct {
		Identifier     string `json:"identifier"`
		Status         string `json:"status"`
		ExpireAt       string `json:"expireAt"`
		ExpiresAt      string `json:"expiresAt"`
		CreatedAt      string `json:"createdAt"`
		LastAccessedAt string `json:"lastAccessedAt"`
	} `json:"properties"`
}

type azureDynamicSessionsErrorResponse struct {
	Error *azureDynamicSessionsError `json:"error,omitempty"`
}

type azureDynamicSessionsError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Target  string `json:"target,omitempty"`
}

type azureDynamicSessionsAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *azureDynamicSessionsAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

var newAzureDynamicSessionsClient = func(ctx context.Context, cfg Config, rt Runtime) (azureDynamicSessionsAPI, error) {
	if err := validateNativeCredentialDestination(cfg); err != nil {
		return nil, err
	}
	endpoint, err := azureDynamicSessionsEndpoint(cfg)
	if err != nil {
		return nil, err
	}
	token, err := azureDynamicSessionsAccessToken(ctx, cfg, rt)
	if err != nil {
		return nil, err
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &azureDynamicSessionsClient{
		endpoint:             endpoint,
		managementAPIVersion: blank(strings.TrimSpace(cfg.AzureDynamicSessions.APIVersion), "2025-02-02-preview"),
		token:                token,
		httpClient:           httpClient,
	}, nil
}

func azureDynamicSessionsEndpoint(cfg Config) (string, error) {
	if strings.TrimSpace(cfg.AzureDynamicSessions.Pool) != "" {
		return "", exit(2, "azureDynamicSessions.pool is not supported; set azureDynamicSessions.endpoint to the custom container poolManagementEndpoint")
	}
	endpoint := strings.TrimSpace(cfg.AzureDynamicSessions.Endpoint)
	if endpoint == "" {
		return "", exit(2, "provider=%s requires azureDynamicSessions.endpoint set to the custom container poolManagementEndpoint", providerName)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", exit(2, "%s endpoint %q is invalid", providerName, endpoint)
	}
	if parsed.User != nil {
		return "", exit(2, "%s endpoint must not include userinfo", providerName)
	}
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return "", exit(2, "%s endpoint %q must use https unless it targets localhost", providerName, endpoint)
	}
	if !isAzureDynamicSessionsTrustedEndpointURL(parsed) {
		return "", exit(2, "%s endpoint %q must target an Azure Container Apps Dynamic Sessions host", providerName, endpoint)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "%s endpoint %q must not include query or fragment components", providerName, endpoint)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isAzureDynamicSessionsTrustedEndpointURL(parsed *url.URL) bool {
	if isLoopbackHTTPURL(parsed) {
		return true
	}
	if parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "azurecontainerapps.io" || strings.HasSuffix(host, ".azurecontainerapps.io")
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func azureDynamicSessionsAccessToken(ctx context.Context, cfg Config, rt Runtime) (string, error) {
	if token := strings.TrimSpace(os.Getenv(tokenEnvName)); token != "" {
		return token, nil
	}
	if rt.Exec == nil {
		return "", exit(2, "provider=%s requires %s or Azure CLI authentication", providerName, tokenEnvName)
	}
	args := []string{
		"account", "get-access-token",
		"--resource", dynamicSessionsAudience,
		"--query", "accessToken",
		"-o", "tsv",
	}
	if tenant := strings.TrimSpace(cfg.AzureTenant); tenant != "" {
		args = append(args, "--tenant", tenant)
	}
	if subscription := strings.TrimSpace(cfg.AzureSubscription); subscription != "" {
		args = append(args, "--subscription", subscription)
	}
	result, err := rt.Exec.Run(ctx, LocalCommandRequest{Name: "az", Args: args})
	if err != nil || result.ExitCode != 0 {
		stderr := strings.TrimSpace(result.Stderr)
		if stderr != "" {
			return "", exit(2, "Azure CLI token request failed: %s", stderr)
		}
		if err != nil {
			return "", exit(2, "Azure CLI token request failed: %v", err)
		}
		return "", exit(2, "Azure CLI token request failed with exit %d", result.ExitCode)
	}
	token := strings.TrimSpace(result.Stdout)
	if token == "" {
		return "", exit(2, "Azure CLI token request returned an empty token")
	}
	return token, nil
}

func (c *azureDynamicSessionsClient) CheckRunner(ctx context.Context, identifier string) error {
	var readiness struct {
		OK bool `json:"ok"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/health", c.sessionQuery(identifier), nil, &readiness); err != nil {
		return err
	}
	if !readiness.OK {
		return fmt.Errorf("%s runner health response is invalid", providerName)
	}
	return nil
}

func (c *azureDynamicSessionsClient) UploadFile(ctx context.Context, identifier, localPath, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open upload file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat upload file: %w", err)
	}
	query := c.sessionQuery(identifier)
	query.Set("path", remotePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/v1/files", query), file)
	if err != nil {
		return err
	}
	req.GetBody = func() (io.ReadCloser, error) {
		redirectFile, err := os.Open(localPath)
		if err != nil {
			return nil, fmt.Errorf("reopen upload file for redirect: %w", err)
		}
		return redirectFile, nil
	}
	c.setHeaders(req)
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.secureHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.responseError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *azureDynamicSessionsClient) ExecStream(ctx context.Context, identifier string, execReq azureDynamicSessionsExecRequest, stdout, stderr io.Writer) (int, error) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(execReq); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/v1/exec", c.sessionQuery(identifier)), &body)
	if err != nil {
		return 0, err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.secureHTTPClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, c.responseError(resp)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "" && mediaType != "application/x-ndjson" && mediaType != "application/jsonl" {
		return 0, fmt.Errorf("unexpected %s stream content-type %q", providerName, resp.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	exitCode := 0
	completed := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event azureDynamicSessionsExecEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return exitCode, fmt.Errorf("decode %s stream event: %w", providerName, err)
		}
		switch event.Type {
		case "stdout":
			if stdout != nil {
				if _, err := io.WriteString(stdout, event.Data); err != nil {
					return exitCode, fmt.Errorf("write %s stdout: %w", providerName, err)
				}
			}
		case "stderr":
			if stderr != nil {
				if _, err := io.WriteString(stderr, event.Data); err != nil {
					return exitCode, fmt.Errorf("write %s stderr: %w", providerName, err)
				}
			}
		case "complete":
			completed = true
			if event.ExitCode != nil {
				exitCode = *event.ExitCode
			}
			return exitCode, nil
		case "error":
			if event.Error == "" {
				event.Error = "stream error"
			}
			return exitCode, errors.New(event.Error)
		case "start", "heartbeat":
		default:
			return exitCode, fmt.Errorf("unknown %s stream event %q", providerName, event.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return exitCode, err
	}
	if !completed {
		return exitCode, fmt.Errorf("%s stream ended before completion", providerName)
	}
	return exitCode, nil
}

func (c *azureDynamicSessionsClient) GetSession(ctx context.Context, identifier string) (azureDynamicSessionsSession, error) {
	var session azureDynamicSessionsSession
	if err := c.doJSON(ctx, http.MethodPost, "/.management/getSession", c.managementQuery(identifier), nil, &session); err != nil {
		return azureDynamicSessionsSession{}, err
	}
	return session.normalized(identifier), nil
}

func (c *azureDynamicSessionsClient) ListSessions(ctx context.Context) ([]azureDynamicSessionsSession, error) {
	query := url.Values{}
	query.Set("api-version", c.managementAPIVersion)
	query.Set("skip", "0")
	next := c.url("/.management/listSessions", query)
	var out []azureDynamicSessionsSession
	var err error
	for next != "" {
		var page struct {
			Sessions []azureDynamicSessionsSession `json:"sessions"`
			Value    []azureDynamicSessionsSession `json:"value"`
			Count    int                           `json:"count"`
			NextLink string                        `json:"nextLink"`
		}
		if err := c.doJSONURL(ctx, http.MethodPost, next, nil, &page); err != nil {
			return nil, err
		}
		sessions := page.Value
		if len(sessions) == 0 && len(page.Sessions) > 0 {
			sessions = page.Sessions
		}
		for _, session := range sessions {
			out = append(out, session.normalized(""))
		}
		next, err = c.nextURL(page.NextLink)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *azureDynamicSessionsClient) DeleteSession(ctx context.Context, identifier string) error {
	return c.doJSON(ctx, http.MethodPost, "/.management/stopSession", c.managementQuery(identifier), nil, nil)
}

func (c *azureDynamicSessionsClient) sessionQuery(identifier string) url.Values {
	query := url.Values{}
	query.Set("identifier", identifier)
	return query
}

func (c *azureDynamicSessionsClient) managementQuery(identifier string) url.Values {
	query := url.Values{}
	query.Set("api-version", c.managementAPIVersion)
	if identifier != "" {
		query.Set("identifier", identifier)
	}
	return query
}

func (c *azureDynamicSessionsClient) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	return c.doJSONURL(ctx, method, c.url(path, query), body, out)
}

func (c *azureDynamicSessionsClient) doJSONURL(ctx context.Context, method, endpoint string, body any, out any) error {
	var r io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		r = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.secureHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &azureDynamicSessionsAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: summarizeJSON(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return err
		}
	}
	return nil
}

func (c *azureDynamicSessionsClient) responseError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &azureDynamicSessionsAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: summarizeJSON(data)}
}

func (c *azureDynamicSessionsClient) secureHTTPClient() *http.Client {
	source := c.httpClient
	if source == nil {
		source = http.DefaultClient
	}
	client := *source
	trusted, _ := url.Parse(c.endpoint)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOriginURL(trusted, req.URL) {
			return fmt.Errorf("%s refused cross-origin redirect to %s", providerName, req.URL.Redacted())
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

func (c *azureDynamicSessionsClient) nextURL(next string) (string, error) {
	next = strings.TrimSpace(next)
	if next == "" {
		return "", nil
	}
	parsed, err := url.Parse(next)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" && parsed.Host == "" {
		parsed, err = url.Parse(c.endpoint + "/" + strings.TrimLeft(next, "/"))
		if err != nil {
			return "", err
		}
	}
	base, err := url.Parse(c.endpoint)
	if err != nil {
		return "", err
	}
	if !sameOriginURL(base, parsed) {
		return "", fmt.Errorf("%s nextLink points outside configured endpoint origin", providerName)
	}
	query := parsed.Query()
	if query.Get("api-version") == "" {
		query.Set("api-version", c.managementAPIVersion)
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

func sameOriginURL(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveURLPort(a) == effectiveURLPort(b)
}

func effectiveURLPort(value *url.URL) string {
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

func (c *azureDynamicSessionsClient) url(path string, query url.Values) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	endpoint := c.endpoint + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	return endpoint
}

func (c *azureDynamicSessionsClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "crabbox/"+providerName)
}

func (s azureDynamicSessionsSession) normalized(fallback string) azureDynamicSessionsSession {
	if s.Identifier == "" {
		s.Identifier = s.Properties.Identifier
	}
	if s.Identifier == "" {
		s.Identifier = fallback
	}
	if s.Status == "" {
		s.Status = s.Properties.Status
	}
	if s.ExpireAt == "" {
		s.ExpireAt = s.Properties.ExpireAt
	}
	if s.ExpiresAt == "" {
		s.ExpiresAt = s.Properties.ExpiresAt
	}
	if s.CreatedAt == "" {
		s.CreatedAt = s.Properties.CreatedAt
	}
	if s.LastAccessedAt == "" {
		s.LastAccessedAt = s.Properties.LastAccessedAt
	}
	return s
}

func azureDynamicSessionsTimeout(cfg Config) time.Duration {
	return time.Duration(azureDynamicSessionsTimeoutSeconds(cfg)) * time.Second
}

func azureDynamicSessionsTimeoutSeconds(cfg Config) int {
	timeout := cfg.AzureDynamicSessions.TimeoutSecs
	if timeout <= 0 {
		if cfg.TTL > 0 {
			return durationSecondsCeil(cfg.TTL)
		}
		return 1800
	}
	return timeout
}
