package unikraftcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Unikraft Cloud REST API, documented at
// https://unikraft.com/docs/api/ (formerly https://docs.kraft.cloud/).
// Every metro exposes its own endpoint (https://api.<metro>.unikraft.cloud)
// and authenticates with `Authorization: Bearer <token>`. Responses share one
// envelope: {"status": "success"|"error", "message"?, "data": {"instances":
// [...]}, "errors"? }.

const (
	unikraftCloudMaxResponseBytes = 16 << 20
	unikraftCloudRequestTimeout   = 2 * time.Minute
)

type unikraftCloudAPI interface {
	BaseURL() string
	CreateInstance(ctx context.Context, req createInstanceRequest) (ukcInstance, error)
	GetInstance(ctx context.Context, id string) (ukcInstance, error)
	ListInstances(ctx context.Context) ([]ukcInstance, error)
	StopInstance(ctx context.Context, id string) (ukcInstance, error)
	DeleteInstance(ctx context.Context, id string) error
}

type unikraftCloudClient struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type createInstanceRequest struct {
	Name      string            `json:"name,omitempty"`
	Image     ukcImage          `json:"image"`
	MemoryMB  int               `json:"memory_mb,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Autostart bool              `json:"autostart"`
}

type ukcImage struct {
	URL string `json:"url"`
}

type ukcInstance struct {
	UUID              string                 `json:"uuid"`
	Name              string                 `json:"name"`
	State             string                 `json:"state"`
	CreatedAt         string                 `json:"created_at"`
	PrivateFQDN       string                 `json:"private_fqdn"`
	MemoryMB          int                    `json:"memory_mb"`
	ServiceGroup      *ukcServiceGroup       `json:"service_group,omitempty"`
	NetworkInterfaces []ukcNetworkInterface  `json:"network_interfaces,omitempty"`
	ItemStatus        string                `json:"status,omitempty"`
	ItemMessage       string                `json:"message,omitempty"`
	ItemError         int                   `json:"error,omitempty"`
}

type ukcServiceGroup struct {
	Domains []ukcDomain `json:"domains,omitempty"`
}

type ukcDomain struct {
	FQDN string `json:"fqdn"`
}

type ukcNetworkInterface struct {
	PrivateIP string `json:"private_ip"`
}

type ukcResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Data    struct {
		Instances []ukcInstance `json:"instances"`
	} `json:"data"`
	Errors []ukcResponseError `json:"errors,omitempty"`
}

type ukcResponseError struct {
	Status  int    `json:"status"`
	Message string `json:"message,omitempty"`
}

type unikraftCloudAPIError struct {
	StatusCode int
	Message    string
}

func (e *unikraftCloudAPIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s API error status=%d", providerName, e.StatusCode)
	}
	return fmt.Sprintf("%s API error status=%d: %s", providerName, e.StatusCode, e.Message)
}

func isNotFound(err error) bool {
	var apiErr *unikraftCloudAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func isUnauthorized(err error) bool {
	var apiErr *unikraftCloudAPIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
}

var unikraftCloudMetroPattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}$`)

func newUnikraftCloudClient(cfg Config, rt Runtime) (unikraftCloudAPI, error) {
	apiKey := strings.TrimSpace(cfg.UnikraftCloud.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=%s requires an API key; set UKC_TOKEN, UNIKRAFT_CLOUD_API_KEY, or unikraftCloud.apiKey", providerName)
	}
	baseURL, err := unikraftCloudBaseURL(cfg)
	if err != nil {
		return nil, err
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &unikraftCloudClient{
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: secureUnikraftCloudHTTPClient(httpClient, baseURL),
	}, nil
}

// unikraftCloudBaseURL derives the metro endpoint unless an explicit API URL
// override is configured (tests, self-hosted gateways).
func unikraftCloudBaseURL(cfg Config) (string, error) {
	if raw := strings.TrimSpace(cfg.UnikraftCloud.APIURL); raw != "" {
		return validateUnikraftCloudAPIURL(raw)
	}
	metro := strings.ToLower(strings.TrimSpace(cfg.UnikraftCloud.Metro))
	if metro == "" {
		return "", exit(2, "provider=%s requires a metro (for example fra, dal, sin, was, sfo) or an explicit API URL", providerName)
	}
	if !unikraftCloudMetroPattern.MatchString(metro) {
		return "", exit(2, "provider=%s metro %q is invalid; use a short lowercase identifier such as fra", providerName, metro)
	}
	return "https://api." + metro + ".unikraft.cloud", nil
}

func validateUnikraftCloudAPIURL(raw string) (string, error) {
	apiURL := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=%s API URL must be an absolute HTTPS URL", providerName)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=%s API URL must not contain userinfo, query parameters, or a fragment", providerName)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return "", exit(2, "provider=%s API URL must use HTTPS except for loopback development endpoints", providerName)
	}
	return apiURL, nil
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func secureUnikraftCloudHTTPClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameUnikraftCloudOrigin(trusted, req.URL) {
			return &unikraftCloudRedirectError{origin: unikraftCloudRedirectOrigin(req.URL)}
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

func sameUnikraftCloudOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveUnikraftCloudPort(a) == effectiveUnikraftCloudPort(b)
}

func effectiveUnikraftCloudPort(value *url.URL) string {
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

type unikraftCloudRedirectError struct {
	origin string
}

func (e *unikraftCloudRedirectError) Error() string {
	return fmt.Sprintf("%s refused cross-origin redirect to %s", providerName, e.origin)
}

func unikraftCloudRedirectOrigin(value *url.URL) string {
	if value == nil || value.Scheme == "" || value.Host == "" {
		return "<redacted>"
	}
	return value.Scheme + "://" + value.Host
}

func (c *unikraftCloudClient) BaseURL() string { return c.baseURL }

func (c *unikraftCloudClient) CreateInstance(ctx context.Context, req createInstanceRequest) (ukcInstance, error) {
	instances, err := c.doInstances(ctx, http.MethodPost, "/v1/instances", req)
	if err != nil {
		return ukcInstance{}, err
	}
	if len(instances) == 0 || strings.TrimSpace(instances[0].UUID) == "" {
		return ukcInstance{}, exit(5, "%s create instance returned no instance uuid", providerName)
	}
	return instances[0], nil
}

func (c *unikraftCloudClient) GetInstance(ctx context.Context, id string) (ukcInstance, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ukcInstance{}, fmt.Errorf("get instance: instance uuid or name is required")
	}
	instances, err := c.doInstances(ctx, http.MethodGet, "/v1/instances/"+url.PathEscape(id), nil)
	if err != nil {
		return ukcInstance{}, err
	}
	if len(instances) == 0 {
		return ukcInstance{}, &unikraftCloudAPIError{StatusCode: http.StatusNotFound, Message: fmt.Sprintf("instance %s not found", id)}
	}
	return instances[0], nil
}

func (c *unikraftCloudClient) ListInstances(ctx context.Context) ([]ukcInstance, error) {
	return c.doInstances(ctx, http.MethodGet, "/v1/instances", nil)
}

func (c *unikraftCloudClient) StopInstance(ctx context.Context, id string) (ukcInstance, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ukcInstance{}, fmt.Errorf("stop instance: instance uuid or name is required")
	}
	instances, err := c.doInstances(ctx, http.MethodPut, "/v1/instances/"+url.PathEscape(id)+"/stop", map[string]any{})
	if err != nil {
		return ukcInstance{}, err
	}
	if len(instances) == 0 {
		return ukcInstance{}, nil
	}
	return instances[0], nil
}

func (c *unikraftCloudClient) DeleteInstance(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("delete instance: instance uuid or name is required")
	}
	_, err := c.doInstances(ctx, http.MethodDelete, "/v1/instances/"+url.PathEscape(id), nil)
	return err
}

func (c *unikraftCloudClient) doInstances(ctx context.Context, method, apiPath string, body any) ([]ukcInstance, error) {
	requestCtx, cancel := context.WithTimeout(ctx, unikraftCloudRequestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%s marshal %s: %w", providerName, apiPath, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(requestCtx, method, c.baseURL+apiPath, reader)
	if err != nil {
		return nil, fmt.Errorf("%s request %s: %w", providerName, apiPath, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		var redirectErr *unikraftCloudRedirectError
		if errors.As(err, &redirectErr) {
			return nil, redirectErr
		}
		return nil, fmt.Errorf("%s %s %s: %s", providerName, method, apiPath, redactSecret(err.Error(), c.apiKey))
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, unikraftCloudMaxResponseBytes+1))
	if readErr != nil {
		return nil, readErr
	}
	if len(data) > unikraftCloudMaxResponseBytes {
		return nil, fmt.Errorf("%s response exceeds %d bytes", providerName, unikraftCloudMaxResponseBytes)
	}
	var envelope ukcResponse
	if len(data) > 0 {
		if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "" && mediaType != "application/json" {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, &unikraftCloudAPIError{StatusCode: resp.StatusCode, Message: redactSecret(strings.TrimSpace(string(data)), c.apiKey)}
			}
			return nil, fmt.Errorf("%s expected application/json response, got %q", providerName, resp.Header.Get("Content-Type"))
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, &unikraftCloudAPIError{StatusCode: resp.StatusCode, Message: redactSecret(strings.TrimSpace(string(data)), c.apiKey)}
			}
			return nil, fmt.Errorf("decode %s response: %w", providerName, err)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &unikraftCloudAPIError{StatusCode: resp.StatusCode, Message: redactSecret(unikraftCloudEnvelopeMessage(envelope), c.apiKey)}
	}
	if strings.EqualFold(strings.TrimSpace(envelope.Status), "error") {
		statusCode := resp.StatusCode
		if len(envelope.Errors) > 0 && envelope.Errors[0].Status > 0 {
			statusCode = envelope.Errors[0].Status
		}
		return nil, &unikraftCloudAPIError{StatusCode: statusCode, Message: redactSecret(unikraftCloudEnvelopeMessage(envelope), c.apiKey)}
	}
	// Batch envelopes report per-item failures inline; surface the first one.
	for _, instance := range envelope.Data.Instances {
		if strings.EqualFold(strings.TrimSpace(instance.ItemStatus), "error") {
			statusCode := instance.ItemError
			if statusCode <= 0 {
				statusCode = http.StatusInternalServerError
			}
			return nil, &unikraftCloudAPIError{StatusCode: statusCode, Message: redactSecret(blank(instance.ItemMessage, "instance operation failed"), c.apiKey)}
		}
	}
	return envelope.Data.Instances, nil
}

func unikraftCloudEnvelopeMessage(envelope ukcResponse) string {
	if message := strings.TrimSpace(envelope.Message); message != "" {
		return message
	}
	for _, item := range envelope.Errors {
		if message := strings.TrimSpace(item.Message); message != "" {
			return message
		}
	}
	for _, item := range envelope.Data.Instances {
		if message := strings.TrimSpace(item.ItemMessage); message != "" {
			return message
		}
	}
	return ""
}

func redactSecret(value, secret string) string {
	if strings.TrimSpace(secret) == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[redacted]")
}
