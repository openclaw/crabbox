package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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

const cloudflareDefaultResponseHeaderTimeout = 30 * time.Second

var cloudflareCleanupTimeout = 15 * time.Second

var cloudflareBearerPattern = regexp.MustCompile(`(?i)\bbearer[ \t]+[A-Za-z0-9._~+/=-]+`)

func newCloudflareClient(cfg core.Config, rt core.Runtime) (*cloudflareClient, error) {
	apiURL := strings.TrimSpace(cfg.Cloudflare.APIURL)
	if apiURL == "" {
		return nil, core.Exit(2, "%s requires --cloudflare-url or CRABBOX_CLOUDFLARE_RUNNER_URL", providerName)
	}
	token := strings.TrimSpace(cfg.Cloudflare.Token)
	if token == "" {
		return nil, core.Exit(2, "%s requires CRABBOX_CLOUDFLARE_RUNNER_TOKEN or user-level config", providerName)
	}
	instanceType, ok := core.NormalizeCloudflareContainerInstanceType(core.Blank(cfg.ServerType, cloudflareContainerInstanceTypeForClass(cfg.Class)))
	if !ok {
		if cfg.ServerTypeExplicit {
			return nil, core.Exit(2, "%s --type must be one of %s", providerName, strings.Join(core.CloudflareContainerInstanceTypes(), ", "))
		}
		instanceType = cloudflareContainerInstanceTypeForClass(cfg.Class)
	}
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return nil, core.Exit(2, "%s url %q is invalid", providerName, apiURL)
	}
	if parsed.User != nil {
		return nil, core.Exit(2, "%s url must not include userinfo", providerName)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, core.Exit(2, "%s url %q is invalid", providerName, apiURL)
	}
	if parsed.Scheme != "https" && !shared.IsLoopbackHTTPURL(parsed) {
		return nil, core.Exit(2, "%s url %q must use https unless it targets localhost", providerName, apiURL)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, core.Exit(2, "%s url %q must not include query or fragment components", providerName, apiURL)
	}
	baseURL := strings.TrimRight(parsed.String(), "/")
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient, err = defaultCloudflareHTTPClient()
		if err != nil {
			return nil, fmt.Errorf("%s HTTP client setup: %w", providerName, err)
		}
	}
	return &cloudflareClient{
		baseURL:      baseURL,
		token:        token,
		instanceType: instanceType,
		http:         shared.SecureHTTPClient(httpClient, parsed, cloudflareRedirectError),
	}, nil
}

func (c *cloudflareClient) useInstanceType(instanceType string) {
	if normalized, ok := core.NormalizeCloudflareContainerInstanceType(instanceType); ok {
		c.instanceType = normalized
	}
}

func cloudflareCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cloudflareCleanupTimeout)
}

func defaultCloudflareHTTPClient() (*http.Client, error) {
	transport, err := core.CloneDefaultTransport()
	if err != nil {
		return nil, err
	}
	transport.ResponseHeaderTimeout = cloudflareDefaultResponseHeaderTimeout
	return &http.Client{Transport: transport}, nil
}

func cloudflareRedirectError(destination *url.URL) error {
	return fmt.Errorf("%s refused cross-origin redirect to %s", providerName, destination.Redacted())
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

func (c *cloudflareClient) execStream(ctx context.Context, sandboxID string, req shared.CommandStreamRequest, stdout, stderr io.Writer) (int, error) {
	httpReq, err := shared.NewJSONRequest(ctx, http.MethodPost, c.baseURL+c.sandboxEndpoint(sandboxID, "/exec-stream"), req)
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
	return (shared.CommandStream{
		Provider:    providerName,
		RedactError: func(message string) string { return redactCloudflareRunnerSecrets(message, c.token) },
	}).Read(ctx, resp, stdout, stderr)
}

func (c *cloudflareClient) sandboxEndpoint(sandboxID, suffix string) string {
	endpoint := "/v1/sandboxes/" + url.PathEscape(sandboxID) + suffix + "?instanceType=" + url.QueryEscape(c.instanceType)
	return endpoint
}

func (c *cloudflareClient) doJSON(ctx context.Context, method, endpoint string, input any, output any) error {
	req, err := shared.NewJSONRequest(ctx, method, c.baseURL+endpoint, input)
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

type cloudflareResponseError struct {
	statusCode int
	message    string
}

func (e *cloudflareResponseError) Error() string { return e.message }

func (c *cloudflareClient) responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var payload struct {
		Error string `json:"error"`
	}
	text := strings.TrimSpace(string(body))
	if err := json.Unmarshal(body, &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		text = payload.Error
	}
	if text == "" {
		text = resp.Status
	}
	return &cloudflareResponseError{statusCode: resp.StatusCode, message: fmt.Sprintf("%s API %s: %s", providerName, resp.Status, redactCloudflareRunnerSecrets(text, c.token))}
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
