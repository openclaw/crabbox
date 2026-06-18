package crownest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultRequestTimeout = 2 * time.Minute

type client interface {
	BaseURL() string
	CreateSandbox(context.Context, createSandboxRequest) (sandbox, error)
	GetSandbox(context.Context, string) (sandbox, error)
	DeleteSandbox(context.Context, string) error
	CreateWorkspaceRun(context.Context, createWorkspaceRunRequest, string) (workspaceRun, error)
	CreateArchiveTransfer(context.Context, string, createArchiveTransferRequest, string) (archiveTransfer, error)
	UploadArchive(context.Context, archiveTransfer, io.Reader) error
	FinalizeArchive(context.Context, string, finalizeArchiveRequest, string) (workspaceRun, error)
	StartWorkspaceRun(context.Context, string, string) (workspaceRun, error)
	CancelWorkspaceRun(context.Context, string, string) (workspaceRun, error)
	GetWorkspaceRun(context.Context, string) (workspaceRun, error)
	StreamWorkspaceRunEvents(context.Context, string, int64) (io.ReadCloser, error)
	Probe(context.Context) error
}

type httpClient struct {
	http    *http.Client
	baseURL string
	apiKey  string
}

type createSandboxRequest struct {
	Metadata  map[string]string `json:"metadata,omitempty"`
	ProjectID string            `json:"projectId,omitempty"`
	Template  string            `json:"template,omitempty"`
	TTLMS     int64             `json:"ttlMs,omitempty"`
}

type sandbox struct {
	ID        string            `json:"id"`
	Status    string            `json:"status,omitempty"`
	CreatedAt string            `json:"createdAt,omitempty"`
	ExpiresAt string            `json:"expiresAt,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type createWorkspaceRunRequest struct {
	Command    string            `json:"command"`
	Keep       bool              `json:"keepSandbox,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	ProjectID  string            `json:"projectId,omitempty"`
	SandboxID  string            `json:"sandboxId,omitempty"`
	Template   string            `json:"template,omitempty"`
	TimeoutMS  int64             `json:"timeoutMs,omitempty"`
	SourceMeta map[string]string `json:"sourceMetadata,omitempty"`
}

type workspaceRun struct {
	ID              string            `json:"id"`
	Status          string            `json:"status"`
	Command         string            `json:"command,omitempty"`
	ExitCode        *int              `json:"exitCode,omitempty"`
	FailureReason   string            `json:"failureReason,omitempty"`
	FailureClass    string            `json:"failureClass,omitempty"`
	KeepSandbox     bool              `json:"keepSandbox,omitempty"`
	SandboxID       string            `json:"sandboxId,omitempty"`
	TemplateSlug    string            `json:"templateSlug,omitempty"`
	ProjectID       string            `json:"projectId,omitempty"`
	DurationMS      int64             `json:"durationMs,omitempty"`
	CleanupStatus   string            `json:"cleanupStatus,omitempty"`
	OrchestrationOK *bool             `json:"orchestrationSucceeded,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type createArchiveTransferRequest struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

type archiveTransfer struct {
	ID                string            `json:"id"`
	ChecksumAlgorithm string            `json:"checksumAlgorithm"`
	ExpiresAt         string            `json:"expiresAt"`
	Headers           map[string]string `json:"headers"`
	MaxSizeBytes      int64             `json:"maxSizeBytes"`
	Method            string            `json:"method"`
	Status            string            `json:"status"`
	UploadURL         string            `json:"uploadUrl"`
	WorkspaceRunID    string            `json:"workspaceRunId"`
}

type finalizeArchiveRequest struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
	UploadID  string `json:"uploadId"`
}

type apiError struct {
	StatusCode int
	err        error
}

func (e *apiError) Error() string { return e.err.Error() }
func (e *apiError) Unwrap() error { return e.err }

func newClient(cfg Config, rt Runtime) (client, error) {
	baseURL, err := validateBaseURL(cfg.Crownest.APIURL)
	if err != nil {
		return nil, err
	}
	apiKey := firstNonEmpty(os.Getenv("CRABBOX_CROWNEST_API_KEY"), os.Getenv("CROWNEST_API_KEY"))
	if apiKey == "" {
		return nil, exit(2, "provider=crownest needs an API key; load CRABBOX_CROWNEST_API_KEY or CROWNEST_API_KEY from a secret manager")
	}
	rawHTTPClient := rt.HTTP
	if rawHTTPClient == nil {
		rawHTTPClient = http.DefaultClient
	}
	return &httpClient{http: secureHTTPClient(rawHTTPClient, baseURL), baseURL: baseURL, apiKey: apiKey}, nil
}

func secureHTTPClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(trusted, req.URL) {
			return fmt.Errorf("crownest refused cross-origin redirect to %s", req.URL.Redacted())
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

func (c *httpClient) BaseURL() string { return c.baseURL }

func (c *httpClient) Probe(ctx context.Context) error {
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/sandboxes?limit=1", nil, nil, "")
	return err
}

func (c *httpClient) CreateSandbox(ctx context.Context, req createSandboxRequest) (sandbox, error) {
	var out struct {
		Sandbox sandbox `json:"sandbox"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes", req, &out, idempotencyKey("sandbox")); err != nil {
		return sandbox{}, err
	}
	if out.Sandbox.ID == "" {
		return sandbox{}, exit(5, "crownest create sandbox returned no sandbox id")
	}
	return out.Sandbox, nil
}

func (c *httpClient) GetSandbox(ctx context.Context, id string) (sandbox, error) {
	var out struct {
		Sandbox sandbox `json:"sandbox"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, &out, ""); err != nil {
		return sandbox{}, err
	}
	return out.Sandbox, nil
}

func (c *httpClient) DeleteSandbox(ctx context.Context, id string) error {
	_, err := c.doJSON(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil, idempotencyKey("delete"))
	return err
}

func (c *httpClient) CreateWorkspaceRun(ctx context.Context, req createWorkspaceRunRequest, key string) (workspaceRun, error) {
	var out struct {
		WorkspaceRun workspaceRun `json:"workspaceRun"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/v1/workspace-runs", req, &out, key); err != nil {
		return workspaceRun{}, err
	}
	if out.WorkspaceRun.ID == "" {
		return workspaceRun{}, exit(5, "crownest create workspace run returned no id")
	}
	return out.WorkspaceRun, nil
}

func (c *httpClient) CreateArchiveTransfer(ctx context.Context, workspaceRunID string, req createArchiveTransferRequest, key string) (archiveTransfer, error) {
	var out struct {
		Transfer archiveTransfer `json:"transfer"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/v1/workspace-runs/"+url.PathEscape(workspaceRunID)+"/archive-transfer", req, &out, key); err != nil {
		return archiveTransfer{}, err
	}
	if out.Transfer.ID == "" || out.Transfer.UploadURL == "" {
		return archiveTransfer{}, exit(5, "crownest archive transfer returned incomplete upload target")
	}
	return out.Transfer, nil
}

func (c *httpClient) UploadArchive(ctx context.Context, transfer archiveTransfer, body io.Reader) error {
	method := strings.TrimSpace(transfer.Method)
	if method == "" {
		method = http.MethodPut
	}
	target := transfer.UploadURL
	if strings.HasPrefix(target, "/") {
		target = c.baseURL + target
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fmt.Errorf("crownest archive upload request: %w", err)
	}
	for name, value := range transfer.Headers {
		req.Header.Set(name, value)
	}
	if c.sameOrigin(req.URL) {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("crownest archive upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.apiError(method, "archive-transfer upload", resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *httpClient) sameOrigin(target *url.URL) bool {
	base, err := url.Parse(c.baseURL)
	return err == nil && sameOrigin(base, target)
}

func (c *httpClient) FinalizeArchive(ctx context.Context, workspaceRunID string, req finalizeArchiveRequest, key string) (workspaceRun, error) {
	var out struct {
		WorkspaceRun workspaceRun `json:"workspaceRun"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/v1/workspace-runs/"+url.PathEscape(workspaceRunID)+"/archive/finalize", req, &out, key); err != nil {
		return workspaceRun{}, err
	}
	return out.WorkspaceRun, nil
}

func (c *httpClient) StartWorkspaceRun(ctx context.Context, id, key string) (workspaceRun, error) {
	var out struct {
		WorkspaceRun workspaceRun `json:"workspaceRun"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/v1/workspace-runs/"+url.PathEscape(id)+"/start", nil, &out, key); err != nil {
		return workspaceRun{}, err
	}
	return out.WorkspaceRun, nil
}

func (c *httpClient) CancelWorkspaceRun(ctx context.Context, id, key string) (workspaceRun, error) {
	var out struct {
		WorkspaceRun workspaceRun `json:"workspaceRun"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/v1/workspace-runs/"+url.PathEscape(id)+"/cancel", nil, &out, key); err != nil {
		return workspaceRun{}, err
	}
	return out.WorkspaceRun, nil
}

func (c *httpClient) GetWorkspaceRun(ctx context.Context, id string) (workspaceRun, error) {
	var out struct {
		WorkspaceRun workspaceRun `json:"workspaceRun"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, "/v1/workspace-runs/"+url.PathEscape(id), nil, &out, ""); err != nil {
		return workspaceRun{}, err
	}
	return out.WorkspaceRun, nil
}

func (c *httpClient) StreamWorkspaceRunEvents(ctx context.Context, id string, afterSeq int64) (io.ReadCloser, error) {
	apiPath := "/v1/workspace-runs/" + url.PathEscape(id) + "/events?stream=true"
	if afterSeq > 0 {
		apiPath += fmt.Sprintf("&afterSeq=%d", afterSeq)
	}
	resp, _, err := c.request(ctx, http.MethodGet, apiPath, nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, c.apiError(http.MethodGet, apiPath, resp)
	}
	return resp.Body, nil
}

func (c *httpClient) doJSON(ctx context.Context, method, apiPath string, body, out any, idempotency string) (bool, error) {
	resp, cancel, err := c.request(ctx, method, apiPath, body, idempotency)
	if err != nil {
		return false, err
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, c.apiError(method, apiPath, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, fmt.Errorf("crownest decode %s: %w", apiPath, err)
	}
	return false, nil
}

func (c *httpClient) request(ctx context.Context, method, apiPath string, body any, idempotency string) (*http.Response, func(), error) {
	requestCtx := ctx
	cancel := func() {}
	if !strings.Contains(apiPath, "/events?stream=true") {
		requestCtx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("crownest marshal %s: %w", apiPath, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(requestCtx, method, c.baseURL+apiPath, reader)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("crownest request %s: %w", apiPath, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(idempotency) != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("crownest %s %s: %w", method, apiPath, err)
	}
	return resp, cancel, nil
}

func (c *httpClient) apiError(method, apiPath string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	var wrapped struct {
		Error any    `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &wrapped) == nil {
		if value, ok := wrapped.Error.(map[string]any); ok {
			if message, ok := value["message"].(string); ok {
				msg = message
			}
		}
		if value, ok := wrapped.Error.(string); ok {
			msg = value
		}
	}
	msg = redactSecrets(msg, c.apiKey)
	return &apiError{
		StatusCode: resp.StatusCode,
		err:        exit(5, "crownest %s %s failed: %s: %s", method, apiPath, resp.Status, msg),
	}
}

func isNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func redactSecrets(value string, secrets ...string) string {
	redacted := value
	for _, secret := range secrets {
		if strings.TrimSpace(secret) != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[redacted]")
		}
	}
	return redacted
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func idempotencyKey(parts ...string) string {
	return "crabbox-crownest-" + strings.Join(parts, "-") + "-" + fmt.Sprint(time.Now().UnixNano())
}

type streamEvent struct {
	Type         string       `json:"type"`
	Seq          int64        `json:"seq"`
	Data         string       `json:"data,omitempty"`
	Code         string       `json:"code,omitempty"`
	Message      string       `json:"message,omitempty"`
	Status       string       `json:"status,omitempty"`
	WorkspaceRun workspaceRun `json:"workspaceRun,omitempty"`
}

func readSSE(reader io.Reader, emit func(streamEvent) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := data.String()
		data.Reset()
		var event streamEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return fmt.Errorf("crownest parse event: %w", err)
		}
		return emit(event)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
