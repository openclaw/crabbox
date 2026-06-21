package cloudflare

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
	"path"
	"regexp"
	"strings"
	"time"
)

type cloudflareClient struct {
	baseURL      string
	token        string
	instanceType string
	http         *http.Client
}

type cloudflareContainer struct {
	ID           string            `json:"id"`
	State        string            `json:"state"`
	Workdir      string            `json:"workdir"`
	InstanceType string            `json:"instanceType,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	CreatedAt    string            `json:"createdAt,omitempty"`
}

type createSandboxRequest struct {
	ID                 string            `json:"id"`
	LeaseID            string            `json:"leaseId"`
	Slug               string            `json:"slug"`
	Repo               string            `json:"repo,omitempty"`
	Workdir            string            `json:"workdir"`
	InstanceType       string            `json:"instanceType,omitempty"`
	TTLSeconds         int               `json:"ttlSeconds,omitempty"`
	IdleTimeoutSeconds int               `json:"idleTimeoutSeconds,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
}

type execStreamRequest struct {
	Command   string            `json:"command"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMS int64             `json:"timeoutMs,omitempty"`
}

type execStreamEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

const cloudflareDefaultResponseHeaderTimeout = 30 * time.Second

var cloudflareCleanupTimeout = 15 * time.Second

var cloudflareBearerPattern = regexp.MustCompile(`(?i)\bbearer[ \t]+[A-Za-z0-9._~+/=-]+`)

func newCloudflareClient(cfg Config, rt Runtime) (*cloudflareClient, error) {
	apiURL := strings.TrimSpace(cfg.Cloudflare.APIURL)
	if apiURL == "" {
		return nil, exit(2, "%s requires --cloudflare-url or CRABBOX_CLOUDFLARE_RUNNER_URL", providerName)
	}
	token := strings.TrimSpace(cfg.Cloudflare.Token)
	if token == "" {
		return nil, exit(2, "%s requires CRABBOX_CLOUDFLARE_RUNNER_TOKEN or user-level config", providerName)
	}
	instanceType, ok := normalizeCloudflareContainerInstanceType(blank(cfg.ServerType, cloudflareContainerInstanceTypeForClass(cfg.Class)))
	if !ok {
		if cfg.ServerTypeExplicit {
			return nil, exit(2, "%s --type must be one of %s", providerName, strings.Join(cloudflareContainerInstanceTypes(), ", "))
		}
		instanceType = cloudflareContainerInstanceTypeForClass(cfg.Class)
	}
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return nil, exit(2, "%s url %q is invalid", providerName, apiURL)
	}
	if parsed.User != nil {
		return nil, exit(2, "%s url must not include userinfo", providerName)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, exit(2, "%s url %q is invalid", providerName, apiURL)
	}
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return nil, exit(2, "%s url %q must use https unless it targets localhost", providerName, apiURL)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, exit(2, "%s url %q must not include query or fragment components", providerName, apiURL)
	}
	baseURL := strings.TrimRight(parsed.String(), "/")
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = defaultCloudflareHTTPClient()
	}
	return &cloudflareClient{
		baseURL:      baseURL,
		token:        token,
		instanceType: instanceType,
		http:         secureCloudflareHTTPClient(httpClient, baseURL),
	}, nil
}

func (c *cloudflareClient) useInstanceType(instanceType string) {
	if normalized, ok := normalizeCloudflareContainerInstanceType(instanceType); ok {
		c.instanceType = normalized
	}
}

func cloudflareCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cloudflareCleanupTimeout)
}

func defaultCloudflareHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = cloudflareDefaultResponseHeaderTimeout
	return &http.Client{Transport: transport}
}

func secureCloudflareHTTPClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameCloudflareOrigin(trusted, req.URL) {
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

func sameCloudflareOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveCloudflarePort(a) == effectiveCloudflarePort(b)
}

func effectiveCloudflarePort(value *url.URL) string {
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

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *cloudflareClient) createSandbox(ctx context.Context, req createSandboxRequest) (cloudflareContainer, error) {
	var sandbox cloudflareContainer
	err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes", req, &sandbox)
	return sandbox, err
}

func (c *cloudflareClient) checkAuth(ctx context.Context) error {
	var readiness struct {
		OK     bool   `json:"ok"`
		Runner string `json:"runner"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/readiness", nil, &readiness); err != nil {
		return err
	}
	if !readiness.OK || readiness.Runner != providerName {
		return fmt.Errorf("%s API readiness response is invalid", providerName)
	}
	return nil
}

func (c *cloudflareClient) getSandbox(ctx context.Context, sandboxID string) (cloudflareContainer, error) {
	var sandbox cloudflareContainer
	err := c.doJSON(ctx, http.MethodGet, c.sandboxEndpoint(sandboxID, ""), nil, &sandbox)
	return sandbox, err
}

func (c *cloudflareClient) destroySandbox(ctx context.Context, sandboxID string) error {
	return c.doJSON(ctx, http.MethodDelete, c.sandboxEndpoint(sandboxID, ""), nil, nil)
}

func (c *cloudflareClient) uploadFile(ctx context.Context, sandboxID, localPath, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open upload file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat upload file: %w", err)
	}
	endpoint := c.sandboxEndpoint(sandboxID, "/files") + "&path=" + url.QueryEscape(remotePath)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, file)
	if err != nil {
		return err
	}
	httpReq.ContentLength = info.Size()
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.responseError(resp)
	}
	return nil
}

func (c *cloudflareClient) execStream(ctx context.Context, sandboxID string, req execStreamRequest, stdout, stderr io.Writer) (int, error) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(req); err != nil {
		return 0, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.sandboxEndpoint(sandboxID, "/exec-stream"), &body)
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
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
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event execStreamEvent
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
			if event.ExitCode != nil {
				exitCode = *event.ExitCode
			}
			return exitCode, nil
		case "error":
			if event.Error == "" {
				event.Error = "stream error"
			}
			return exitCode, errors.New(redactCloudflareRunnerSecrets(event.Error, c.token))
		case "start", "heartbeat":
		default:
			return exitCode, fmt.Errorf("unknown %s stream event %q", providerName, event.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return exitCode, err
	}
	return exitCode, fmt.Errorf("%s stream ended before completion", providerName)
}

func (c *cloudflareClient) sandboxEndpoint(sandboxID, suffix string) string {
	endpoint := "/v1/sandboxes/" + url.PathEscape(sandboxID) + suffix + "?instanceType=" + url.QueryEscape(c.instanceType)
	return endpoint
}

func (c *cloudflareClient) doJSON(ctx context.Context, method, endpoint string, input any, output any) error {
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

func (c *cloudflareClient) responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		return fmt.Errorf("%s API %s: %s", providerName, resp.Status, redactCloudflareRunnerSecrets(payload.Error, c.token))
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		text = resp.Status
	}
	return fmt.Errorf("%s API %s: %s", providerName, resp.Status, redactCloudflareRunnerSecrets(text, c.token))
}

func redactCloudflareRunnerSecrets(value string, secrets ...string) string {
	redacted := value
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[redacted]")
		}
	}
	return cloudflareBearerPattern.ReplaceAllString(redacted, "Bearer [redacted]")
}

func remoteArchivePath() string {
	return path.Join("/tmp", "crabbox-cloudflare-sync-"+time.Now().UTC().Format("20060102150405.000000000")+".tgz")
}
