package lambda

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Client struct {
	token   string
	client  *http.Client
	baseURL string
}

type APIError struct {
	Operation  string
	Status     int
	Code       string
	Message    string
	Suggestion string
	Body       string
}

func (e *APIError) Error() string {
	if e.Code != "" || e.Message != "" {
		return fmt.Sprintf("lambda %s: http %d: code=%s message=%s", e.Operation, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("lambda %s: http %d: %s", e.Operation, e.Status, e.Body)
}

func newClient(rt core.Runtime) (*Client, error) {
	token, err := requireToken()
	if err != nil {
		return nil, err
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{token: token, client: httpClient, baseURL: defaultAPIBaseURL}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		reader = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	operation := method + " " + path
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.decodeAPIError(operation, resp.StatusCode, data, readErr)
	}
	if readErr != nil {
		return fmt.Errorf("lambda %s response body: %w", operation, readErr)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("lambda %s decode: %w", operation, err)
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("lambda %s decode: missing data envelope", operation)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("lambda %s decode data: %w", operation, err)
	}
	return nil
}

func (c *Client) decodeAPIError(operation string, status int, data []byte, readErr error) error {
	body := strings.TrimSpace(string(data))
	var envelope apiErrorEnvelope
	if err := json.Unmarshal(data, &envelope); err == nil && (envelope.Error.Code != "" || envelope.Error.Message != "") {
		apiErr := &APIError{
			Operation:  operation,
			Status:     status,
			Code:       envelope.Error.Code,
			Message:    c.redact(envelope.Error.Message),
			Suggestion: c.redact(envelope.Error.Suggestion),
			Body:       c.redact(body),
		}
		if readErr != nil {
			apiErr.Body = strings.TrimSpace(apiErr.Body + "; response body read failed: " + readErr.Error())
		}
		return apiErr
	}
	if len(body) > 400 {
		body = body[:400]
	}
	body = c.redact(body)
	if readErr != nil {
		if body != "" {
			body += "; "
		}
		body += "response body read failed: " + readErr.Error()
	}
	return &APIError{Operation: operation, Status: status, Body: body}
}

func (c *Client) redact(value string) string {
	out := value
	if c.token != "" {
		out = strings.ReplaceAll(out, c.token, "<redacted>")
	}
	for _, field := range []string{"token", "api_key", "user_data", "private_key", "privateKey", "jupyter_token", "jupyterToken", "jupyter_url", "jupyterUrl"} {
		out = redactJSONishField(out, field)
		out = redactInlineField(out, field)
	}
	out = redactPrivateKeyBlock(out)
	out = redactJupyterURLs(out)
	return out
}

func redactJSONishField(body, field string) string {
	pattern := regexp.MustCompile(`("` + regexp.QuoteMeta(field) + `"\s*:\s*")[^"]*(")`)
	return pattern.ReplaceAllString(body, `${1}<redacted>${2}`)
}

func redactInlineField(body, field string) string {
	pattern := regexp.MustCompile(`(?i)(` + regexp.QuoteMeta(field) + `\s*[=: ]\s*)[^",\s]+`)
	return pattern.ReplaceAllString(body, `${1}<redacted>`)
}

func redactPrivateKeyBlock(body string) string {
	pattern := regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
	return pattern.ReplaceAllString(body, "<redacted>")
}

func redactJupyterURLs(body string) string {
	pattern := regexp.MustCompile(`https?://[^\s"']*(?i:token=)[^\s"']+`)
	return pattern.ReplaceAllString(body, "<redacted>")
}

func (c *Client) ListInstanceTypes(ctx context.Context) ([]InstanceType, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/instance-types", nil, &raw); err != nil {
		return nil, err
	}
	return decodeInstanceTypes(raw)
}

func decodeInstanceTypes(raw json.RawMessage) ([]InstanceType, error) {
	var list []InstanceType
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}

	var keyed map[string]struct {
		InstanceType                 InstanceType `json:"instance_type"`
		Name                         string       `json:"name,omitempty"`
		Description                  string       `json:"description,omitempty"`
		RegionsWithCapacityAvailable []string     `json:"regions_with_capacity_available,omitempty"`
	}
	if err := json.Unmarshal(raw, &keyed); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(keyed))
	for key := range keyed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]InstanceType, 0, len(keys))
	for _, key := range keys {
		item := keyed[key]
		instanceType := item.InstanceType
		if instanceType.Name == "" {
			instanceType.Name = firstNonBlank(item.Name, key)
		}
		if instanceType.Description == "" {
			instanceType.Description = item.Description
		}
		if len(instanceType.RegionsWithCapacityAvailable) == 0 {
			instanceType.RegionsWithCapacityAvailable = item.RegionsWithCapacityAvailable
		}
		out = append(out, instanceType)
	}
	return out, nil
}

func (c *Client) ListRegions(ctx context.Context) ([]Region, error) {
	var out []Region
	err := c.do(ctx, http.MethodGet, "/regions", nil, &out)
	return out, err
}

func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	var out []Image
	err := c.do(ctx, http.MethodGet, "/images", nil, &out)
	return out, err
}

func (c *Client) ListInstances(ctx context.Context) ([]Instance, error) {
	var out []Instance
	err := c.do(ctx, http.MethodGet, "/instances", nil, &out)
	return out, err
}

func (c *Client) GetInstance(ctx context.Context, id string) (Instance, error) {
	var out Instance
	err := c.do(ctx, http.MethodGet, "/instances/"+id, nil, &out)
	return out, err
}

func (c *Client) LaunchInstance(ctx context.Context, req LaunchInstanceRequest) (LaunchInstanceResponse, error) {
	var out LaunchInstanceResponse
	err := c.do(ctx, http.MethodPost, "/instance-operations/launch", req, &out)
	return out, err
}

func (c *Client) TerminateInstances(ctx context.Context, ids []string) error {
	return c.do(ctx, http.MethodPost, "/instance-operations/terminate", TerminateInstanceRequest{InstanceIDs: ids}, nil)
}

func (c *Client) ListSSHKeys(ctx context.Context) ([]SSHKey, error) {
	var out []SSHKey
	err := c.do(ctx, http.MethodGet, "/ssh-keys", nil, &out)
	return out, err
}

func (c *Client) AddSSHKey(ctx context.Context, req AddSSHKeyRequest) (SSHKey, error) {
	var out SSHKey
	err := c.do(ctx, http.MethodPost, "/ssh-keys", req, &out)
	return out, err
}

func (c *Client) DeleteSSHKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/ssh-keys/"+id, nil, nil)
}

func (c *Client) ListFilesystems(ctx context.Context) ([]Filesystem, error) {
	var out []Filesystem
	err := c.do(ctx, http.MethodGet, "/file-systems", nil, &out)
	return out, err
}

func (c *Client) ListFirewallRulesets(ctx context.Context) ([]FirewallRuleset, error) {
	var out []FirewallRuleset
	err := c.do(ctx, http.MethodGet, "/firewall-rulesets", nil, &out)
	return out, err
}
