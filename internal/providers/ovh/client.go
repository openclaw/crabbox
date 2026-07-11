package ovh

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const defaultEndpoint = "https://api.us.ovhcloud.com/1.0"

var (
	errOVHCrossOriginRedirect = errors.New("ovh refused cross-origin redirect")
	errOVHInvalidRedirect     = errors.New("ovh refused invalid redirect")
	errOVHRedirectLimit       = errors.New("ovh redirect stopped after 10 redirects")
)

type clientConfig struct {
	Endpoint          string
	ApplicationKey    string
	ApplicationSecret string
	ConsumerKey       string
	HTTP              *http.Client
	AllowTestEndpoint bool
}

type Client struct {
	endpoint          *url.URL
	applicationKey    string
	applicationSecret string
	consumerKey       string
	http              *http.Client
	now               func() int64
	timeMu            sync.Mutex
	timeSyncMu        sync.Mutex
	serverTimeDelta   int64
	hasServerTime     bool
}

type APIError struct {
	Operation string
	Status    int
	Body      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ovh %s: http %d: %s", e.Operation, e.Status, e.Body)
}

type Project struct {
	ID          string `json:"project_id,omitempty"`
	ProjectID   string `json:"projectId,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"`
}

type projectList []Project

func (p *projectList) UnmarshalJSON(data []byte) error {
	var ids []string
	if err := json.Unmarshal(data, &ids); err == nil {
		projects := make([]Project, 0, len(ids))
		for _, id := range ids {
			projects = append(projects, Project{ID: id})
		}
		*p = projects
		return nil
	}
	var projects []Project
	if err := json.Unmarshal(data, &projects); err != nil {
		return err
	}
	*p = projects
	return nil
}

func (p Project) Key() string {
	if p.ID != "" {
		return p.ID
	}
	return p.ProjectID
}

type Region struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
}

func (r *Region) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		r.Name = name
		r.Status = ""
		return nil
	}
	type regionAlias Region
	var out regionAlias
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*r = Region(out)
	return nil
}

type Flavor struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Region    string `json:"region,omitempty"`
	OSType    string `json:"osType,omitempty"`
	Available *bool  `json:"available,omitempty"`
	Quota     *int64 `json:"quota,omitempty"`
}

func (f Flavor) Matches(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && (f.ID == value || f.Name == value)
}

type Image struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Region     string `json:"region,omitempty"`
	Type       string `json:"type,omitempty"`
	Status     string `json:"status,omitempty"`
	Visibility string `json:"visibility,omitempty"`
}

func (i Image) Matches(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && (i.ID == value || i.Name == value)
}

type SSHKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PublicKey   string `json:"publicKey,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type IPAddress struct {
	IP      string `json:"ip,omitempty"`
	Version int    `json:"version,omitempty"`
	Type    string `json:"type,omitempty"`
}

type Instance struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Status      string            `json:"status,omitempty"`
	Region      string            `json:"region,omitempty"`
	FlavorID    string            `json:"flavorId,omitempty"`
	Flavor      Flavor            `json:"flavor,omitempty"`
	Image       Image             `json:"image,omitempty"`
	SSHKeyID    string            `json:"sshKeyId,omitempty"`
	IPAddresses []IPAddress       `json:"ipAddresses,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type API interface {
	AuthTime(context.Context) (int64, error)
	ListProjects(context.Context) ([]Project, error)
	ListRegions(context.Context, string) ([]Region, error)
	ListFlavors(context.Context, string, string) ([]Flavor, error)
	GetFlavor(context.Context, string, string) (Flavor, error)
	ListImages(context.Context, string, string) ([]Image, error)
	GetImage(context.Context, string, string) (Image, error)
	ListSSHKeys(context.Context, string) ([]SSHKey, error)
	GetSSHKey(context.Context, string, string) (SSHKey, error)
	CreateSSHKey(context.Context, string, string, string) (SSHKey, error)
	DeleteSSHKey(context.Context, string, string) error
	ListInstances(context.Context, string) ([]Instance, error)
	GetInstance(context.Context, string, string) (Instance, error)
	CreateInstance(context.Context, string, InstanceCreateRequest) (Instance, error)
	DeleteInstance(context.Context, string, string) error
}

type InstanceCreateRequest struct {
	Name     string `json:"name"`
	Region   string `json:"region"`
	FlavorID string `json:"flavorId"`
	ImageID  string `json:"imageId"`
	SSHKeyID string `json:"sshKeyId,omitempty"`
	UserData string `json:"userData,omitempty"`
}

func newClient(cfg core.Config, rt core.Runtime) (*Client, error) {
	endpoint := strings.TrimSpace(cfg.OVH.Endpoint)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return newClientWithConfig(clientConfig{
		Endpoint:          endpoint,
		ApplicationKey:    os.Getenv("OVH_APPLICATION_KEY"),
		ApplicationSecret: os.Getenv("OVH_APPLICATION_SECRET"),
		ConsumerKey:       os.Getenv("OVH_CONSUMER_KEY"),
		HTTP:              rt.HTTP,
	})
}

func newClientWithConfig(cfg clientConfig) (*Client, error) {
	applicationKey := strings.TrimSpace(cfg.ApplicationKey)
	applicationSecret := strings.TrimSpace(cfg.ApplicationSecret)
	consumerKey := strings.TrimSpace(cfg.ConsumerKey)
	switch {
	case applicationKey == "":
		return nil, core.Exit(3, "OVH_APPLICATION_KEY is required")
	case applicationSecret == "":
		return nil, core.Exit(3, "OVH_APPLICATION_SECRET is required")
	case consumerKey == "":
		return nil, core.Exit(3, "OVH_CONSUMER_KEY is required")
	}
	rawEndpoint := normalizeEndpointAlias(cfg.Endpoint)
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, core.Exit(2, "ovh endpoint must be an absolute URL")
	}
	if !cfg.AllowTestEndpoint && (endpoint.Scheme != "https" || !isOVHEndpointHost(endpoint.Hostname())) {
		return nil, core.Exit(2, "ovh endpoint must use https and an ovhcloud.com host")
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	httpClient = secureOVHHTTPClient(httpClient, endpoint)
	return &Client{
		endpoint:          endpoint,
		applicationKey:    applicationKey,
		applicationSecret: applicationSecret,
		consumerKey:       consumerKey,
		http:              httpClient,
		now: func() int64 {
			return time.Now().Unix()
		},
	}, nil
}

func secureOVHHTTPClient(source *http.Client, trusted *url.URL) *http.Client {
	client := *source
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOVHOrigin(trusted, req.URL) {
			return errOVHCrossOriginRedirect
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errOVHRedirectLimit
		}
		return nil
	}
	return &client
}

func sameOVHOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveOVHPort(a) == effectiveOVHPort(b)
}

func effectiveOVHPort(value *url.URL) string {
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

func sanitizeOVHClientError(err error) error {
	// net/http wraps redirect failures with the untrusted Location URL. Return
	// only stable sentinels so provider diagnostics cannot retain it.
	if errors.Is(err, errOVHCrossOriginRedirect) {
		return errOVHCrossOriginRedirect
	}
	if errors.Is(err, errOVHRedirectLimit) {
		return errOVHRedirectLimit
	}
	var requestErr *url.Error
	if errors.As(err, &requestErr) && strings.Contains(requestErr.Err.Error(), "failed to parse Location header") {
		return errOVHInvalidRedirect
	}
	if err != nil && strings.Contains(err.Error(), "invalid redirect location") {
		return errOVHInvalidRedirect
	}
	return err
}

func (c *Client) AuthTime(ctx context.Context) (int64, error) {
	var timestamp int64
	if err := c.do(ctx, http.MethodGet, "/auth/time", nil, &timestamp); err != nil {
		return 0, err
	}
	c.cacheServerTime(timestamp)
	return timestamp, nil
}

func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var out projectList
	err := c.do(ctx, http.MethodGet, "/cloud/project", nil, &out)
	return []Project(out), err
}

func (c *Client) ListRegions(ctx context.Context, projectID string) ([]Region, error) {
	var out []Region
	err := c.do(ctx, http.MethodGet, cloudPath(projectID, "region"), nil, &out)
	return out, err
}

func (c *Client) ListFlavors(ctx context.Context, projectID, region string) ([]Flavor, error) {
	var out []Flavor
	err := c.do(ctx, http.MethodGet, cloudPath(projectID, "flavor")+queryRegion(region), nil, &out)
	return out, err
}

func (c *Client) GetFlavor(ctx context.Context, projectID, flavorID string) (Flavor, error) {
	var out Flavor
	err := c.do(ctx, http.MethodGet, cloudPath(projectID, "flavor", flavorID), nil, &out)
	return out, err
}

func (c *Client) ListImages(ctx context.Context, projectID, region string) ([]Image, error) {
	var out []Image
	err := c.do(ctx, http.MethodGet, cloudPath(projectID, "image")+queryRegion(region), nil, &out)
	return out, err
}

func (c *Client) GetImage(ctx context.Context, projectID, imageID string) (Image, error) {
	var out Image
	err := c.do(ctx, http.MethodGet, cloudPath(projectID, "image", imageID), nil, &out)
	return out, err
}

func (c *Client) ListSSHKeys(ctx context.Context, projectID string) ([]SSHKey, error) {
	var out []SSHKey
	err := c.do(ctx, http.MethodGet, cloudPath(projectID, "sshkey"), nil, &out)
	return out, err
}

func (c *Client) GetSSHKey(ctx context.Context, projectID, keyID string) (SSHKey, error) {
	var out SSHKey
	err := c.do(ctx, http.MethodGet, cloudPath(projectID, "sshkey", keyID), nil, &out)
	return out, err
}

func (c *Client) CreateSSHKey(ctx context.Context, projectID, name, publicKey string) (SSHKey, error) {
	body := map[string]string{
		"name":      name,
		"publicKey": publicKey,
	}
	var out SSHKey
	err := c.do(ctx, http.MethodPost, cloudPath(projectID, "sshkey"), body, &out)
	return out, err
}

func (c *Client) DeleteSSHKey(ctx context.Context, projectID, keyID string) error {
	return c.do(ctx, http.MethodDelete, cloudPath(projectID, "sshkey", keyID), nil, nil)
}

func (c *Client) ListInstances(ctx context.Context, projectID string) ([]Instance, error) {
	var out []Instance
	err := c.do(ctx, http.MethodGet, cloudPath(projectID, "instance"), nil, &out)
	return out, err
}

func (c *Client) GetInstance(ctx context.Context, projectID, instanceID string) (Instance, error) {
	var out Instance
	err := c.do(ctx, http.MethodGet, cloudPath(projectID, "instance", instanceID), nil, &out)
	return out, err
}

func (c *Client) CreateInstance(ctx context.Context, projectID string, body InstanceCreateRequest) (Instance, error) {
	var out Instance
	err := c.do(ctx, http.MethodPost, cloudPath(projectID, "instance"), body, &out)
	return out, err
}

func (c *Client) DeleteInstance(ctx context.Context, projectID, instanceID string) error {
	return c.do(ctx, http.MethodDelete, cloudPath(projectID, "instance", instanceID), nil, nil)
}

func (c *Client) do(ctx context.Context, method, requestPath string, body any, out any) error {
	if requestPath != "/auth/time" {
		if err := c.ensureServerTime(ctx); err != nil {
			return fmt.Errorf("ovh synchronize server time: %w", err)
		}
	}
	bodyBytes, err := encodeBody(body)
	if err != nil {
		return err
	}
	reqURL := c.urlFor(requestPath)
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Ovh-Application", c.applicationKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if requestPath != "/auth/time" {
		timestamp := strconv.FormatInt(c.signedTimestamp(), 10)
		req.Header.Set("X-Ovh-Timestamp", timestamp)
		req.Header.Set("X-Ovh-Consumer", c.consumerKey)
		req.Header.Set("X-Ovh-Signature", c.signature(method, reqURL, string(bodyBytes), timestamp))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return sanitizeOVHClientError(err)
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := redactSecrets(strings.TrimSpace(string(data)), c.applicationKey, c.applicationSecret, c.consumerKey)
		if len(body) > 400 {
			body = body[:400]
		}
		if readErr != nil {
			if body != "" {
				body += "; "
			}
			body += "response body read failed: " + readErr.Error()
		}
		return &APIError{Operation: method + " " + requestPath, Status: resp.StatusCode, Body: body}
	}
	if readErr != nil {
		return fmt.Errorf("ovh %s %s response body: %w", method, requestPath, readErr)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("ovh %s %s decode: %w", method, requestPath, err)
	}
	return nil
}

func (c *Client) ensureServerTime(ctx context.Context) error {
	c.timeMu.Lock()
	hasServerTime := c.hasServerTime
	c.timeMu.Unlock()
	if hasServerTime {
		return nil
	}
	c.timeSyncMu.Lock()
	defer c.timeSyncMu.Unlock()
	c.timeMu.Lock()
	hasServerTime = c.hasServerTime
	c.timeMu.Unlock()
	if hasServerTime {
		return nil
	}
	_, err := c.AuthTime(ctx)
	return err
}

func (c *Client) cacheServerTime(serverTimestamp int64) {
	localTimestamp := c.now()
	c.timeMu.Lock()
	defer c.timeMu.Unlock()
	c.serverTimeDelta = serverTimestamp - localTimestamp
	c.hasServerTime = true
}

func (c *Client) signedTimestamp() int64 {
	localTimestamp := c.now()
	c.timeMu.Lock()
	defer c.timeMu.Unlock()
	if !c.hasServerTime {
		return localTimestamp
	}
	return localTimestamp + c.serverTimeDelta
}

func encodeBody(body any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func (c *Client) urlFor(requestPath string) string {
	base := strings.TrimRight(c.endpoint.String(), "/")
	if c.endpoint.RawQuery != "" {
		base = strings.TrimSuffix(base, "?"+c.endpoint.RawQuery)
	}
	return base + "/" + strings.TrimLeft(requestPath, "/")
}

func (c *Client) signature(method, fullURL, body, timestamp string) string {
	sum := sha1.Sum([]byte(strings.Join([]string{
		c.applicationSecret,
		c.consumerKey,
		strings.ToUpper(method),
		fullURL,
		body,
		timestamp,
	}, "+")))
	return "$1$" + hex.EncodeToString(sum[:])
}

func cloudPath(projectID string, parts ...string) string {
	all := []string{"cloud", "project", projectID}
	all = append(all, parts...)
	escaped := make([]string, 0, len(all))
	for _, part := range all {
		escaped = append(escaped, escapePathSegment(part))
	}
	return "/" + strings.Join(escaped, "/")
}

func escapePathSegment(part string) string {
	switch part {
	case ".":
		return "%2E"
	case "..":
		return "%2E%2E"
	default:
		return url.PathEscape(part)
	}
}

func queryRegion(region string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		return ""
	}
	values := url.Values{}
	values.Set("region", region)
	return "?" + values.Encode()
}

func redactSecrets(value string, secrets ...string) string {
	out := value
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			out = strings.ReplaceAll(out, secret, "[redacted]")
		}
	}
	out = regexp.MustCompile(`\$1\$[0-9a-fA-F]{40}`).ReplaceAllString(out, "[redacted-signature]")
	return out
}

func isOVHEndpointHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "ovhcloud.com" || strings.HasSuffix(host, ".ovhcloud.com") || host == "ovh.com" || strings.HasSuffix(host, ".ovh.com")
}

func normalizeEndpointAlias(endpoint string) string {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case "":
		return defaultEndpoint
	case "ovh-us":
		return "https://api.us.ovhcloud.com/1.0"
	case "ovh-ca":
		return "https://ca.api.ovh.com/1.0"
	case "ovh-eu":
		return "https://eu.api.ovh.com/1.0"
	default:
		return strings.TrimSpace(endpoint)
	}
}

func redactedEndpoint(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "-"
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("[redacted]")
	return u.String()
}
