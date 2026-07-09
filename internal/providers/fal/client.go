package fal

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
	"strings"
)

type computeAPI interface {
	ListInstances(ctx context.Context, limit int, cursor string) (ListInstancesResponse, error)
	GetInstance(ctx context.Context, id string) (ComputeInstance, error)
	CreateInstance(ctx context.Context, input CreateInstanceRequest, idempotencyKey string) (ComputeInstance, error)
	DeleteInstance(ctx context.Context, id string) error
}

type client struct {
	apiKey     string
	apiURL     string
	httpClient *http.Client
}

const maxResponseBytes = 16 << 20

var errFalCrossOriginRedirect = errors.New("fal refused cross-origin redirect")

// APIError mirrors fal's documented standard error envelope. Request IDs are
// taken from the error body when present; the OpenAPI schema does not document a
// request-id response header for Compute as of 2026-06-25.
type APIError struct {
	StatusCode int
	Status     string
	Type       string
	Message    string
	DocsURL    string
	RequestID  string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = e.Status
	}
	if e.Type != "" {
		return fmt.Sprintf("fal API %s: %s", e.Type, message)
	}
	return fmt.Sprintf("fal API %s", message)
}

func newClient(cfg Config, rt Runtime) (computeAPI, error) {
	apiKey := strings.TrimSpace(cfg.Fal.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=%s requires fal credentials in environment", providerName)
	}
	apiURL := strings.TrimRight(strings.TrimSpace(blank(cfg.Fal.APIURL, defaultAPIURL)), "/")
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return nil, exit(2, "%s api url is invalid", providerName)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, exit(2, "%s api url contains unsupported sensitive components", providerName)
	}
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return nil, exit(2, "%s api url must use https unless it targets localhost", providerName)
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &client{apiKey: apiKey, apiURL: apiURL, httpClient: secureHTTPClient(httpClient, apiURL)}, nil
}

func secureHTTPClient(source *http.Client, apiURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(apiURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(trusted, req.URL) {
			return errFalCrossOriginRedirect
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

func isLoopbackHTTPURL(value *url.URL) bool {
	if !strings.EqualFold(value.Scheme, "http") {
		return false
	}
	host := value.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *client) ListInstances(ctx context.Context, limit int, cursor string) (ListInstancesResponse, error) {
	path := "/compute/instances"
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", limit))
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out ListInstancesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return ListInstancesResponse{}, err
	}
	return out, nil
}

func (c *client) GetInstance(ctx context.Context, id string) (ComputeInstance, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ComputeInstance{}, fmt.Errorf("fal instance id is required")
	}
	var out ComputeInstance
	if err := c.do(ctx, http.MethodGet, "/compute/instances/"+url.PathEscape(id), nil, "", &out); err != nil {
		return ComputeInstance{}, err
	}
	return out, nil
}

func (c *client) CreateInstance(ctx context.Context, input CreateInstanceRequest, idempotencyKey string) (ComputeInstance, error) {
	var out ComputeInstance
	if err := c.do(ctx, http.MethodPost, "/compute/instances", input, strings.TrimSpace(idempotencyKey), &out); err != nil {
		return ComputeInstance{}, err
	}
	return out, nil
}

func (c *client) DeleteInstance(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("fal instance id is required")
	}
	return c.do(ctx, http.MethodDelete, "/compute/instances/"+url.PathEscape(id), nil, "", nil)
}

func (c *client) do(ctx context.Context, method, path string, body any, idempotencyKey string, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Key "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, errFalCrossOriginRedirect) {
			return errFalCrossOriginRedirect
		}
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return readErr
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("fal response exceeds %d bytes", maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp, bytes.ReplaceAll(data, []byte(c.apiKey), []byte("<redacted>")))
	}
	if out != nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode fal data: %w", err)
		}
	}
	return nil
}

func decodeAPIError(resp *http.Response, data []byte) error {
	apiErr := &APIError{StatusCode: resp.StatusCode, Status: resp.Status}
	var body APIErrorBody
	if len(strings.TrimSpace(string(data))) > 0 && json.Unmarshal(data, &body) == nil {
		apiErr.Type = strings.TrimSpace(body.Error.Type)
		apiErr.Message = strings.TrimSpace(body.Error.Message)
		apiErr.DocsURL = strings.TrimSpace(body.Error.DocsURL)
		apiErr.RequestID = strings.TrimSpace(body.Error.RequestID)
	}
	if apiErr.Message == "" {
		apiErr.Message = strings.TrimSpace(string(data))
	}
	return apiErr
}
