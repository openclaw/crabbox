package superserve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const defaultSuperserveRequestTimeout = 2 * time.Minute

type superserveClient interface {
	BaseURL() string
	CreateSandbox(context.Context, createSandboxRequest) (superserveSandbox, error)
	ListSandboxes(context.Context, map[string]string) ([]superserveSandbox, error)
	GetSandbox(context.Context, string) (superserveSandbox, error)
	ActivateSandbox(context.Context, string) (sandboxAccess, error)
	UpdateSandboxMetadata(context.Context, string, map[string]string) (superserveSandbox, error)
	PauseSandbox(context.Context, string) (superserveSandbox, error)
	ResumeSandbox(context.Context, string) (sandboxAccess, error)
	DeleteSandbox(context.Context, string) error
	Probe(context.Context) error
}

type httpSuperserveClient struct {
	http    *http.Client
	baseURL string
	apiKey  string
}

type createSandboxRequest struct {
	Template    string            `json:"template,omitempty"`
	Snapshot    string            `json:"snapshot,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
	TimeoutSecs int               `json:"timeout_secs,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type updateSandboxRequest struct {
	Metadata map[string]string `json:"metadata"`
}

type superserveSandbox struct {
	ID       string            `json:"id"`
	Status   string            `json:"status,omitempty"`
	State    string            `json:"state,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type sandboxAccess struct {
	Sandbox     superserveSandbox
	AccessToken string
}

type superserveAPIError struct {
	StatusCode int
	err        error
}

func (e *superserveAPIError) Error() string { return e.err.Error() }
func (e *superserveAPIError) Unwrap() error { return e.err }

func newSuperserveClient(cfg Config, rt Runtime) (superserveClient, error) {
	baseURL, err := validateSuperserveBaseURL(cfg.Superserve.BaseURL)
	if err != nil {
		return nil, err
	}
	apiKey := firstNonEmpty(
		os.Getenv("CRABBOX_SUPERSERVE_API_KEY"),
		os.Getenv("SUPERSERVE_API_KEY"),
	)
	if apiKey == "" {
		return nil, exit(2, "provider=superserve needs an API key; load CRABBOX_SUPERSERVE_API_KEY or SUPERSERVE_API_KEY from a secret manager")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &httpSuperserveClient{http: secureSuperserveHTTPClient(httpClient, baseURL), baseURL: baseURL, apiKey: apiKey}, nil
}

func secureSuperserveHTTPClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameSuperserveOrigin(trusted, req.URL) {
			return fmt.Errorf("superserve refused cross-origin redirect to %s", req.URL.Redacted())
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

func sameSuperserveOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveSuperservePort(a) == effectiveSuperservePort(b)
}

func effectiveSuperservePort(value *url.URL) string {
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

func (c *httpSuperserveClient) BaseURL() string { return c.baseURL }

func (c *httpSuperserveClient) doJSON(ctx context.Context, method, apiPath string, body, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, defaultSuperserveRequestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("superserve marshal %s: %w", apiPath, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(requestCtx, method, c.baseURL+apiPath, reader)
	if err != nil {
		return fmt.Errorf("superserve request %s: %w", apiPath, err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("superserve %s %s: %w", method, apiPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.apiError(method, apiPath, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("superserve decode %s: %w", apiPath, err)
	}
	return nil
}

func (c *httpSuperserveClient) CreateSandbox(ctx context.Context, req createSandboxRequest) (superserveSandbox, error) {
	var sb superserveSandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes", req, &sb); err != nil {
		return superserveSandbox{}, err
	}
	if sb.ID == "" {
		return superserveSandbox{}, exit(5, "superserve create returned no sandbox id")
	}
	return sb, nil
}

func (c *httpSuperserveClient) ListSandboxes(ctx context.Context, metadata map[string]string) ([]superserveSandbox, error) {
	values := url.Values{}
	for k, v := range metadata {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		values.Set("metadata."+k, v)
	}
	apiPath := "/sandboxes"
	if encoded := values.Encode(); encoded != "" {
		apiPath += "?" + encoded
	}
	var out []superserveSandbox
	if err := c.doJSON(ctx, http.MethodGet, apiPath, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *httpSuperserveClient) GetSandbox(ctx context.Context, id string) (superserveSandbox, error) {
	var sb superserveSandbox
	if err := c.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, &sb); err != nil {
		return superserveSandbox{}, err
	}
	return sb, nil
}

func (c *httpSuperserveClient) ActivateSandbox(ctx context.Context, id string) (sandboxAccess, error) {
	return c.postAccess(ctx, "/sandboxes/"+url.PathEscape(id)+"/activate")
}

func (c *httpSuperserveClient) UpdateSandboxMetadata(ctx context.Context, id string, metadata map[string]string) (superserveSandbox, error) {
	var sb superserveSandbox
	if err := c.doJSON(ctx, http.MethodPatch, "/sandboxes/"+url.PathEscape(id), updateSandboxRequest{Metadata: metadata}, &sb); err != nil {
		return superserveSandbox{}, err
	}
	if sb.ID == "" {
		sb.ID = id
	}
	return sb, nil
}

func (c *httpSuperserveClient) PauseSandbox(ctx context.Context, id string) (superserveSandbox, error) {
	var sb superserveSandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/pause", nil, &sb); err != nil {
		return superserveSandbox{}, err
	}
	if sb.ID == "" {
		sb.ID = id
	}
	return sb, nil
}

func (c *httpSuperserveClient) ResumeSandbox(ctx context.Context, id string) (sandboxAccess, error) {
	return c.postAccess(ctx, "/sandboxes/"+url.PathEscape(id)+"/resume")
}

func (c *httpSuperserveClient) DeleteSandbox(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil)
}

func (c *httpSuperserveClient) Probe(ctx context.Context) error {
	var raw json.RawMessage
	return c.doJSON(ctx, http.MethodGet, "/sandboxes", nil, &raw)
}

func (c *httpSuperserveClient) postAccess(ctx context.Context, apiPath string) (sandboxAccess, error) {
	var raw struct {
		AccessToken string            `json:"access_token"`
		Token       string            `json:"token"`
		Sandbox     superserveSandbox `json:"sandbox"`
		ID          string            `json:"id"`
		Status      string            `json:"status"`
		State       string            `json:"state"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := c.doJSON(ctx, http.MethodPost, apiPath, nil, &raw); err != nil {
		return sandboxAccess{}, err
	}
	token := blank(raw.AccessToken, raw.Token)
	if token == "" {
		return sandboxAccess{}, exit(5, "superserve %s returned no access token", apiPath)
	}
	sb := raw.Sandbox
	if sb.ID == "" {
		sb.ID = raw.ID
	}
	if sb.Status == "" {
		sb.Status = raw.Status
	}
	if sb.State == "" {
		sb.State = raw.State
	}
	if sb.Metadata == nil {
		sb.Metadata = raw.Metadata
	}
	return sandboxAccess{Sandbox: sb, AccessToken: token}, nil
}

func (c *httpSuperserveClient) apiError(method, apiPath string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	var wrapped struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &wrapped) == nil {
		msg = blank(wrapped.Error, blank(wrapped.Message, msg))
	}
	msg = redactSuperserveSecrets(msg, c.apiKey)
	return &superserveAPIError{
		StatusCode: resp.StatusCode,
		err:        exit(5, "superserve %s %s failed: %s: %s", method, apiPath, resp.Status, msg),
	}
}

func isSuperserveNotFound(err error) bool {
	var apiErr *superserveAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func redactSuperserveSecrets(value string, secrets ...string) string {
	redacted := value
	for _, secret := range secrets {
		if strings.TrimSpace(secret) != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[redacted]")
		}
	}
	redacted = redactJSONSecretField(redacted, "access_token")
	redacted = redactJSONSecretField(redacted, "token")
	return redacted
}

func redactJSONSecretField(value, key string) string {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"[^"]*"`)
	return pattern.ReplaceAllString(value, `"`+key+`":"[redacted]"`)
}

func superserveEndpointScope(baseURL string) string {
	digest := sha256.Sum256([]byte(baseURL))
	return "endpoint-sha256:" + hex.EncodeToString(digest[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
