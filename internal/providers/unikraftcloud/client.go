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
	"path"
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
	// Unikraft Cloud reports a missing resource as application error 8 inside
	// an HTTP 200 per-instance result.
	unikraftCloudErrorNotFound = 8
)

type unikraftCloudAPI interface {
	BaseURL() string
	UserUUID(ctx context.Context) (string, error)
	CreateInstance(ctx context.Context, req createInstanceRequest) (ukcInstance, error)
	GetInstance(ctx context.Context, id string) (ukcInstance, error)
	ListInstances(ctx context.Context) ([]ukcInstance, error)
	DeleteInstance(ctx context.Context, id string) (ukcInstance, error)
}

type unikraftCloudClient struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type createInstanceRequest struct {
	Name      string            `json:"name,omitempty"`
	Image     string            `json:"image"`
	MemoryMB  int               `json:"memory_mb,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Autostart bool              `json:"autostart"`
}

type ukcInstance struct {
	UUID              string                `json:"uuid"`
	Name              string                `json:"name"`
	State             string                `json:"state"`
	CreatedAt         string                `json:"created_at"`
	PrivateFQDN       string                `json:"private_fqdn"`
	MemoryMB          int                   `json:"memory_mb"`
	ServiceGroup      *ukcServiceGroup      `json:"service_group,omitempty"`
	NetworkInterfaces []ukcNetworkInterface `json:"network_interfaces,omitempty"`
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

type ukcQuotasResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Data    struct {
		Quotas []ukcQuota `json:"quotas"`
	} `json:"data"`
	Errors []ukcResponseError `json:"errors,omitempty"`
}

type ukcQuota struct {
	UUID        string `json:"uuid"`
	ItemStatus  string `json:"status,omitempty"`
	ItemMessage string `json:"message,omitempty"`
	ItemError   int    `json:"error,omitempty"`
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

var (
	unikraftCloudMetroPattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}$`)
	unikraftCloudUUIDPattern  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

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
	parsed, err := url.Parse(strings.TrimSpace(raw))
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
	if escapedPath := parsed.EscapedPath(); escapedPath != "" && escapedPath != "/" {
		return "", exit(2, "provider=%s API URL must identify the endpoint root without a path", providerName)
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed.String(), nil
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
		if !withinUnikraftCloudAPIPath(trusted, req.URL) {
			return &unikraftCloudRedirectPathError{}
		}
		if len(via) > 0 && isUnikraftCloudMutation(via[0].Method) && req.Method != via[0].Method {
			return &unikraftCloudRedirectMethodError{from: via[0].Method, to: req.Method}
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

func withinUnikraftCloudAPIPath(baseURL, target *url.URL) bool {
	if baseURL == nil || target == nil {
		return false
	}
	// RawPath is populated when the redirect uses a non-canonical escape such
	// as an encoded slash or dot segment. Refuse it rather than letting a
	// downstream router reinterpret the authenticated request outside /v1/.
	if target.RawPath != "" {
		return false
	}
	cleanPath := path.Clean(target.Path)
	if target.Path != cleanPath && target.Path != cleanPath+"/" {
		return false
	}
	basePath := strings.TrimSuffix(baseURL.Path, "/") + "/v1/"
	return strings.HasPrefix(target.Path, basePath)
}

func isUnikraftCloudMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
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

type unikraftCloudRedirectPathError struct{}

func (e *unikraftCloudRedirectPathError) Error() string {
	return fmt.Sprintf("%s refused redirect outside the trusted API path", providerName)
}

type unikraftCloudRedirectMethodError struct {
	from string
	to   string
}

func (e *unikraftCloudRedirectMethodError) Error() string {
	return fmt.Sprintf("%s refused redirect that changed mutation method from %s to %s", providerName, e.from, e.to)
}

func unikraftCloudRedirectOrigin(value *url.URL) string {
	if value == nil || value.Scheme == "" || value.Host == "" {
		return "<redacted>"
	}
	return value.Scheme + "://" + value.Host
}

func (c *unikraftCloudClient) BaseURL() string { return c.baseURL }

func (c *unikraftCloudClient) UserUUID(ctx context.Context) (string, error) {
	var envelope ukcQuotasResponse
	statusCode, err := c.doJSON(ctx, http.MethodGet, "/v1/users/quotas", nil, &envelope)
	if err != nil {
		return "", err
	}
	if statusCode < 200 || statusCode >= 300 {
		return "", &unikraftCloudAPIError{StatusCode: statusCode, Message: redactSecret(unikraftCloudQuotasEnvelopeMessage(envelope), c.apiKey)}
	}
	if !strings.EqualFold(strings.TrimSpace(envelope.Status), "success") {
		if err := c.unikraftCloudQuotaError(envelope); err != nil {
			return "", err
		}
		if len(envelope.Errors) > 0 {
			return "", c.unikraftCloudResponseError(envelope.Errors[0], unikraftCloudQuotasEnvelopeMessage(envelope))
		}
		return "", &unikraftCloudAPIError{StatusCode: http.StatusInternalServerError, Message: redactSecret(unikraftCloudQuotasEnvelopeMessage(envelope), c.apiKey)}
	}
	if len(envelope.Errors) > 0 {
		return "", c.unikraftCloudResponseError(envelope.Errors[0], unikraftCloudQuotasEnvelopeMessage(envelope))
	}
	if err := c.unikraftCloudQuotaError(envelope); err != nil {
		return "", err
	}
	var userUUID string
	for _, quota := range envelope.Data.Quotas {
		candidate := strings.TrimSpace(quota.UUID)
		if quota.UUID == "" {
			continue
		}
		if candidate != quota.UUID || !unikraftCloudUUIDPattern.MatchString(candidate) {
			return "", fmt.Errorf("%s quotas response contains an invalid user UUID", providerName)
		}
		if userUUID != "" && candidate != userUUID {
			return "", fmt.Errorf("%s quotas response contains conflicting user UUIDs", providerName)
		}
		userUUID = candidate
	}
	if userUUID == "" {
		return "", fmt.Errorf("%s quotas response contains no user UUID", providerName)
	}
	return userUUID, nil
}

func (c *unikraftCloudClient) CreateInstance(ctx context.Context, req createInstanceRequest) (ukcInstance, error) {
	instances, err := c.doInstances(ctx, http.MethodPost, "/v1/instances", req)
	if err != nil {
		return ukcInstance{}, err
	}
	instance, err := requireExactUnikraftCloudInstance("create instance", "", instances)
	if err != nil {
		return ukcInstance{}, err
	}
	if req.Name != "" && instance.Name != req.Name {
		return ukcInstance{}, exit(5, "%s create instance returned an unexpected instance name", providerName)
	}
	return instance, nil
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
	return requireExactUnikraftCloudInstance("get instance", id, instances)
}

func (c *unikraftCloudClient) ListInstances(ctx context.Context) ([]ukcInstance, error) {
	instances, err := c.doInstances(ctx, http.MethodGet, "/v1/instances", nil)
	if err != nil {
		return nil, err
	}
	if _, err := indexUnikraftCloudInventory(instances); err != nil {
		return nil, err
	}
	return instances, nil
}

func (c *unikraftCloudClient) DeleteInstance(ctx context.Context, id string) (ukcInstance, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ukcInstance{}, fmt.Errorf("delete instance: instance uuid or name is required")
	}
	instances, err := c.doInstances(ctx, http.MethodDelete, "/v1/instances/"+url.PathEscape(id), map[string]any{
		"timeout_s":   -1,
		"dont_retain": true,
	})
	if err != nil {
		return ukcInstance{}, err
	}
	instance, err := requireExactUnikraftCloudInstance("delete instance", id, instances)
	if err != nil {
		return ukcInstance{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(instance.ItemStatus), "success") {
		return ukcInstance{}, exit(5, "%s delete instance returned an item without explicit success", providerName)
	}
	return instance, nil
}

func (c *unikraftCloudClient) doInstances(ctx context.Context, method, apiPath string, body any) ([]ukcInstance, error) {
	var envelope ukcResponse
	statusCode, err := c.doJSON(ctx, method, apiPath, body, &envelope)
	if err != nil {
		return nil, err
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, &unikraftCloudAPIError{StatusCode: statusCode, Message: redactSecret(unikraftCloudEnvelopeMessage(envelope), c.apiKey)}
	}
	status := strings.ToLower(strings.TrimSpace(envelope.Status))
	if status == "error" {
		if len(envelope.Errors) > 0 {
			return nil, c.unikraftCloudResponseError(envelope.Errors[0], unikraftCloudEnvelopeMessage(envelope))
		}
		if err := c.unikraftCloudInstanceError(envelope); err != nil {
			return nil, err
		}
		return nil, &unikraftCloudAPIError{StatusCode: http.StatusInternalServerError, Message: redactSecret(unikraftCloudEnvelopeMessage(envelope), c.apiKey)}
	}
	if status != "success" && status != "partial_success" {
		redactedStatus := redactSecret(strings.TrimSpace(envelope.Status), c.apiKey)
		return nil, fmt.Errorf("%s response has invalid status %q", providerName, redactedStatus)
	}
	// Partial and batch envelopes report failures inline even when the top-level
	// status is not "error". Never return the successful subset silently.
	if err := c.unikraftCloudInstanceError(envelope); err != nil {
		return nil, err
	}
	if len(envelope.Errors) > 0 {
		return nil, c.unikraftCloudResponseError(envelope.Errors[0], unikraftCloudEnvelopeMessage(envelope))
	}
	if status == "partial_success" {
		message := blank(strings.TrimSpace(envelope.Message), "instance operation only partially succeeded")
		return nil, &unikraftCloudAPIError{StatusCode: http.StatusInternalServerError, Message: redactSecret(message, c.apiKey)}
	}
	return envelope.Data.Instances, nil
}

func (c *unikraftCloudClient) doJSON(ctx context.Context, method, apiPath string, body, destination any) (int, error) {
	requestCtx, cancel := context.WithTimeout(ctx, unikraftCloudRequestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("%s marshal %s: %w", providerName, apiPath, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(requestCtx, method, c.baseURL+apiPath, reader)
	if err != nil {
		return 0, fmt.Errorf("%s request %s: %w", providerName, apiPath, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		var redirectErr *unikraftCloudRedirectError
		var redirectPathErr *unikraftCloudRedirectPathError
		var redirectMethodErr *unikraftCloudRedirectMethodError
		if errors.As(err, &redirectErr) {
			return 0, redirectErr
		}
		if errors.As(err, &redirectPathErr) {
			return 0, redirectPathErr
		}
		if errors.As(err, &redirectMethodErr) {
			return 0, redirectMethodErr
		}
		return 0, fmt.Errorf("%s %s %s: %s", providerName, method, apiPath, redactSecret(err.Error(), c.apiKey))
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, unikraftCloudMaxResponseBytes+1))
	if readErr != nil {
		return 0, readErr
	}
	if len(data) > unikraftCloudMaxResponseBytes {
		return 0, fmt.Errorf("%s response exceeds %d bytes", providerName, unikraftCloudMaxResponseBytes)
	}
	if len(data) > 0 {
		if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "" && mediaType != "application/json" {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return 0, &unikraftCloudAPIError{StatusCode: resp.StatusCode, Message: redactSecret(strings.TrimSpace(string(data)), c.apiKey)}
			}
			return 0, fmt.Errorf("%s expected application/json response, got %q", providerName, resp.Header.Get("Content-Type"))
		}
		if err := json.Unmarshal(data, destination); err != nil {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return 0, &unikraftCloudAPIError{StatusCode: resp.StatusCode, Message: redactSecret(strings.TrimSpace(string(data)), c.apiKey)}
			}
			return 0, fmt.Errorf("decode %s response: %w", providerName, err)
		}
	}
	return resp.StatusCode, nil
}

func requireExactUnikraftCloudInstance(operation, requestedID string, instances []ukcInstance) (ukcInstance, error) {
	if len(instances) != 1 {
		return ukcInstance{}, exit(5, "%s %s returned %d instances; expected exactly one", providerName, operation, len(instances))
	}
	instance := instances[0]
	if !unikraftCloudUUIDPattern.MatchString(instance.UUID) {
		return ukcInstance{}, exit(5, "%s %s returned an invalid instance uuid", providerName, operation)
	}
	if requestedID != "" {
		matches := instance.Name == requestedID
		if unikraftCloudUUIDPattern.MatchString(requestedID) {
			matches = instance.UUID == requestedID
		}
		if !matches {
			return ukcInstance{}, exit(5, "%s %s returned an unexpected instance identity", providerName, operation)
		}
	}
	return instance, nil
}

func (c *unikraftCloudClient) unikraftCloudInstanceError(envelope ukcResponse) error {
	for _, instance := range envelope.Data.Instances {
		status := strings.ToLower(strings.TrimSpace(instance.ItemStatus))
		if status == "error" || instance.ItemError != 0 {
			statusCode := unikraftCloudHTTPStatus(instance.ItemError)
			return &unikraftCloudAPIError{StatusCode: statusCode, Message: redactSecret(blank(instance.ItemMessage, "instance operation failed"), c.apiKey)}
		}
		if status != "" && status != "success" {
			return fmt.Errorf("%s instance result has invalid status %q", providerName, redactSecret(instance.ItemStatus, c.apiKey))
		}
	}
	return nil
}

func (c *unikraftCloudClient) unikraftCloudQuotaError(envelope ukcQuotasResponse) error {
	for _, quota := range envelope.Data.Quotas {
		status := strings.ToLower(strings.TrimSpace(quota.ItemStatus))
		if status == "error" || quota.ItemError != 0 {
			statusCode := unikraftCloudHTTPStatus(quota.ItemError)
			return &unikraftCloudAPIError{StatusCode: statusCode, Message: redactSecret(blank(quota.ItemMessage, "quota lookup failed"), c.apiKey)}
		}
		if status != "" && status != "success" {
			return fmt.Errorf("%s quota result has invalid status %q", providerName, redactSecret(quota.ItemStatus, c.apiKey))
		}
	}
	return nil
}

func (c *unikraftCloudClient) unikraftCloudResponseError(responseErr ukcResponseError, fallback string) error {
	message := blank(strings.TrimSpace(responseErr.Message), fallback)
	return &unikraftCloudAPIError{
		StatusCode: unikraftCloudHTTPStatus(responseErr.Status),
		Message:    redactSecret(message, c.apiKey),
	}
}

func unikraftCloudHTTPStatus(code int) int {
	if code == unikraftCloudErrorNotFound {
		return http.StatusNotFound
	}
	if code >= 100 && code <= 599 {
		return code
	}
	return http.StatusInternalServerError
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

func unikraftCloudQuotasEnvelopeMessage(envelope ukcQuotasResponse) string {
	if message := strings.TrimSpace(envelope.Message); message != "" {
		return message
	}
	for _, item := range envelope.Errors {
		if message := strings.TrimSpace(item.Message); message != "" {
			return message
		}
	}
	for _, item := range envelope.Data.Quotas {
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
