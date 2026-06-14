package agx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// agxAPI is the AGX control-plane surface Crabbox depends on. AGX is early
// access (https://www.agx.so, shipping Summer 2026) and does not publish a
// stable control-plane contract yet, so this interface is intentionally narrow
// and provisional: create an instance with a Crabbox-registered SSH public key,
// look one up, list Crabbox-owned instances, and delete one. The SSH transport
// itself is AGX-native (`ssh <user>+<instance>@<workspace>`), so Crabbox owns
// nothing beyond instance lifecycle and key registration.
type agxAPI interface {
	CreateInstance(ctx context.Context, req agxCreateRequest) (agxInstance, error)
	GetInstance(ctx context.Context, id string) (agxInstance, error)
	ListInstances(ctx context.Context, prefix string) ([]agxInstance, error)
	DeleteInstance(ctx context.Context, id string) error
}

type agxCreateRequest struct {
	Name      string            `json:"name"`
	PublicKey string            `json:"public_key"`
	Image     string            `json:"image,omitempty"`
	Region    string            `json:"region,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type agxInstance struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	SSHUser string            `json:"ssh_user"`
	SSHHost string            `json:"ssh_host"`
	SSHPort int               `json:"ssh_port"`
	Region  string            `json:"region"`
	Labels  map[string]string `json:"labels"`
}

type agxListResponse struct {
	Instances     []agxInstance `json:"instances"`
	NextPageToken string        `json:"next_page_token"`
}

type agxClient struct {
	token      string
	apiURL     string
	httpClient *http.Client
}

type agxAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *agxAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

func newAGXClient(cfg Config, rt Runtime) agxAPI {
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	apiURL := strings.TrimRight(blank(cfg.AGX.APIURL, "https://api.agx.so"), "/")
	return &agxClient{token: strings.TrimSpace(cfg.AGX.Token), apiURL: apiURL, httpClient: httpClient}
}

func (c *agxClient) CreateInstance(ctx context.Context, req agxCreateRequest) (agxInstance, error) {
	var instance agxInstance
	if err := c.doJSON(ctx, http.MethodPost, "/v1/instances", nil, req, &instance); err != nil {
		return agxInstance{}, err
	}
	if instance.Name == "" {
		instance.Name = req.Name
	}
	if len(instance.Labels) == 0 {
		instance.Labels = req.Labels
	}
	return instance, nil
}

func (c *agxClient) GetInstance(ctx context.Context, id string) (agxInstance, error) {
	var instance agxInstance
	if err := c.doJSON(ctx, http.MethodGet, "/v1/instances/"+url.PathEscape(id), nil, nil, &instance); err != nil {
		return agxInstance{}, err
	}
	return instance, nil
}

func (c *agxClient) ListInstances(ctx context.Context, prefix string) ([]agxInstance, error) {
	var all []agxInstance
	pageToken := ""
	seen := map[string]bool{}
	for {
		query := url.Values{}
		query.Set("page_size", "50")
		if prefix != "" {
			query.Set("prefix", prefix)
		}
		if pageToken != "" {
			query.Set("page_token", pageToken)
		}
		var page agxListResponse
		if err := c.doJSON(ctx, http.MethodGet, "/v1/instances", query, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Instances...)
		next := strings.TrimSpace(page.NextPageToken)
		if next == "" {
			return all, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("agx list response repeated page token %q", next)
		}
		seen[next] = true
		pageToken = next
	}
}

func (c *agxClient) DeleteInstance(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/instances/"+url.PathEscape(id), nil, nil, nil)
}

func (c *agxClient) doJSON(ctx context.Context, method, requestPath string, query url.Values, body any, out any) error {
	var r io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		r = &buf
	}
	endpoint := c.apiURL + requestPath
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &agxAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: summarizeAGXBody(data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse agx response: %w", err)
	}
	return nil
}

func agxAPILabels(leaseID, slug string) map[string]string {
	return map[string]string{
		"crabbox":       "true",
		"created_by":    "crabbox",
		"provider":      agxProvider,
		"crabbox-lease": leaseID,
		"crabbox-slug":  normalizeLeaseSlug(slug),
	}
}

func isCrabboxInstance(instance agxInstance) bool {
	return instance.Labels["crabbox"] == "true" || instance.Labels["created_by"] == "crabbox"
}

func agxLeaseID(instance agxInstance) string {
	return strings.TrimSpace(instance.Labels["crabbox-lease"])
}

func agxSlug(leaseID string, instance agxInstance) string {
	if slug := strings.TrimSpace(instance.Labels["crabbox-slug"]); slug != "" {
		return normalizeLeaseSlug(slug)
	}
	if leaseID != "" {
		return newLeaseSlug(leaseID)
	}
	return normalizeLeaseSlug(instance.Name)
}

func isAGXNotFound(err error) bool {
	apiErr, ok := err.(*agxAPIError)
	return ok && apiErr.StatusCode == http.StatusNotFound
}

func agxError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("agx %s: %w", action, err)
}

func summarizeAGXBody(data []byte) string {
	text := strings.TrimSpace(string(data))
	if len(text) > 4096 {
		return text[:4096] + "..."
	}
	return text
}
