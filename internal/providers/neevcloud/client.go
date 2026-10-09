package neevcloud

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	controlTimeout     = 60 * time.Second
	readyTimeout       = 5 * time.Minute
	execCeiling        = time.Hour
	uploadChunkSize    = 1 << 20
	listPageLimit      = 100
	maxListPages       = 1000
	maxExecEventBytes  = 16 << 20
	protocolVersion    = "1"
	tusResumable       = "1.0.0"
	workspaceRoot      = "/workspace"
	phaseReady         = "Ready"
	phaseRestoreFailed = "RestoreFailed"
)

// readyPollInterval is a variable so tests can poll without real waits.
var readyPollInterval = 2 * time.Second

// apiKeyEnvNames lists the only credential sources, primary first.
var apiKeyEnvNames = []string{"CRABBOX_NEEVCLOUD_API_KEY", "NEEV_API_KEY"}

type apiError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *apiError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

// client implements shared.EnvdSandboxAPI over the NeevCloud lifecycle and sandbox runtime APIs.
type client struct {
	apiKey     string
	baseURL    string
	orgID      string
	projectID  string
	httpClient *http.Client
	dataClient *http.Client
}

// sandbox is the NeevCloud lifecycle sandbox object.
type sandbox struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Phase      string            `json:"phase"`
	ConnectURL *string           `json:"connect_url"`
	Labels     map[string]string `json:"labels"`
	CreatedAt  string            `json:"created_at"`
}

type sandboxPage struct {
	Items []sandbox `json:"items"`
	Total int       `json:"total"`
}

type execEvent struct {
	Type       string `json:"type"`
	Data       string `json:"data"`
	ExitCode   *int   `json:"exit_code"`
	ReasonCode string `json:"reason_code"`
	Message    string `json:"message"`
}

// newClient is a variable so backend tests can inject a fake API.
var newClient = func(cfg core.Config, rt core.Runtime) (shared.EnvdSandboxAPI, error) {
	apiKey := apiKeyFromEnv()
	if apiKey == "" {
		return nil, core.Exit(2, "provider=neevcloud needs an API key; load CRABBOX_NEEVCLOUD_API_KEY or NEEV_API_KEY from a secret manager")
	}
	return newClientWithKey(cfg, rt, apiKey)
}

// newClientWithKey validates the project scope and endpoint before any request.
func newClientWithKey(cfg core.Config, rt core.Runtime, apiKey string) (*client, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	orgID, projectID, err := requiredScope(cfg)
	if err != nil {
		return nil, err
	}
	baseURL, _ := validateBaseURL(cfg.Neevcloud.BaseURL)
	httpClient, dataClient := shared.ControlAndDataHTTPClients(rt.HTTP, controlTimeout)
	return &client{
		apiKey: apiKey, baseURL: baseURL, orgID: orgID, projectID: projectID,
		httpClient: httpClient, dataClient: dataClient,
	}, nil
}

// apiKeyFromEnv returns the first non-blank credential; it is never read from config or argv.
func apiKeyFromEnv() string {
	for _, name := range apiKeyEnvNames {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// CreateSandbox creates a named sandbox; a 409 for that name resolves the existing one by lease.
func (c *client) CreateSandbox(ctx context.Context, req shared.EnvdSandboxCreateRequest) (shared.EnvdSandbox, error) {
	name := sandboxName(req.Metadata["slug"], req.Metadata["lease"])
	body := map[string]any{
		"name":                name,
		"sandbox_template_id": req.TemplateID,
		"labels":              req.Metadata,
		"egress":              map[string]any{"mode": "allow_list", "allow_internet": req.AllowInternetAccess},
	}
	if req.TimeoutSeconds > 0 {
		body["lifecycle"] = map[string]any{"max_lifetime_seconds": req.TimeoutSeconds}
	}
	var created sandbox
	err := c.control(ctx, http.MethodPost, "", nil, body, &created)
	var conflict *apiError
	if errors.As(err, &conflict) && conflict.StatusCode == http.StatusConflict {
		return c.resolveConflict(ctx, name, req.Metadata["lease"])
	}
	if err != nil {
		return shared.EnvdSandbox{}, err
	}
	return created.envd(), nil
}

// resolveConflict adopts an existing sandbox only when its name and lease label both match.
func (c *client) resolveConflict(ctx context.Context, name, leaseID string) (shared.EnvdSandbox, error) {
	sandboxes, err := c.ListSandboxes(ctx, map[string]string{"lease": leaseID, "provider": providerName})
	if err != nil {
		return shared.EnvdSandbox{}, err
	}
	for _, sb := range sandboxes {
		if sb.Alias == name {
			return sb, nil
		}
	}
	return shared.EnvdSandbox{}, fmt.Errorf("sandbox name %q already exists in this project and is not owned by lease %q", name, leaseID)
}

// ConnectSandbox waits until the sandbox is Ready with a connect URL; the session's Domain carries that URL.
func (c *client) ConnectSandbox(ctx context.Context, sandboxID string, _ int) (shared.EnvdSandboxSession, error) {
	ready, err := shared.PollReady(ctx, readyTimeout, readyPollInterval,
		func(ctx context.Context) (sandbox, error) {
			sb, err := c.getSandbox(ctx, sandboxID)
			if err == nil && sb.Phase == phaseRestoreFailed {
				return sb, fmt.Errorf("sandbox %s is in phase %s", sandboxID, sb.Phase)
			}
			return sb, err
		},
		func(sb sandbox) bool { return sb.Phase == phaseReady && sb.connectURL() != "" },
		fmt.Errorf("timed out after %s waiting for sandbox %s to become Ready", readyTimeout, sandboxID))
	if err != nil {
		return shared.EnvdSandboxSession{}, err
	}
	connectURL, err := validateConnectURL(ready.connectURL())
	if err != nil {
		return shared.EnvdSandboxSession{}, err
	}
	return shared.EnvdSandboxSession{SandboxID: ready.ID, Domain: connectURL}, nil
}

func (c *client) GetSandbox(ctx context.Context, sandboxID string) (shared.EnvdSandbox, error) {
	sb, err := c.getSandbox(ctx, sandboxID)
	if err != nil {
		return shared.EnvdSandbox{}, err
	}
	return sb.envd(), nil
}

// getSandbox reads one sandbox and rejects a response for a different ID.
func (c *client) getSandbox(ctx context.Context, sandboxID string) (sandbox, error) {
	var sb sandbox
	if err := c.control(ctx, http.MethodGet, "/"+url.PathEscape(sandboxID), nil, nil, &sb); err != nil {
		return sandbox{}, err
	}
	if shared.ValidateResourceID(sandboxID, sb.ID) != nil {
		return sandbox{}, errors.New("get sandbox returned a different or missing sandbox ID")
	}
	return sb, nil
}

// ListSandboxes pages through the project with AND-ed label filters, re-checking labels locally.
func (c *client) ListSandboxes(ctx context.Context, labels map[string]string) ([]shared.EnvdSandbox, error) {
	filters := make([]string, 0, len(labels))
	for key, value := range labels {
		filters = append(filters, key+"="+value)
	}
	slices.Sort(filters)
	var all []shared.EnvdSandbox
	for page, seen := 1, 0; page <= maxListPages; page++ {
		query := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(listPageLimit)}, "label": filters}
		var out sandboxPage
		if err := c.control(ctx, http.MethodGet, "", query, nil, &out); err != nil {
			return nil, err
		}
		for _, sb := range out.Items {
			if labelsMatch(sb.Labels, labels) {
				all = append(all, sb.envd())
			}
		}
		seen += len(out.Items)
		if len(out.Items) == 0 || len(out.Items) < listPageLimit || seen >= out.Total {
			return all, nil
		}
	}
	return nil, fmt.Errorf("list sandboxes exceeded %d pages", maxListPages)
}

func (c *client) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return c.control(ctx, http.MethodDelete, "/"+url.PathEscape(sandboxID), nil, nil, nil)
}

// UploadFile streams r in chunks with the tus resumable upload protocol.
func (c *client) UploadFile(ctx context.Context, session shared.EnvdSandboxSession, targetPath string, r io.Reader) error {
	relative, err := workspaceRelative(targetPath)
	if err != nil {
		return err
	}
	if relative == "" {
		return core.Exit(2, "neevcloud upload target %q must name a file under %s", targetPath, workspaceRoot)
	}
	size, body, err := uploadSize(r)
	if err != nil {
		return err
	}
	createURL, err := c.dataURL(session, "/v1/files/uploads")
	if err != nil {
		return err
	}
	resp, err := c.data(ctx, http.MethodPost, createURL, nil, func(req *http.Request) {
		req.Header.Set("Tus-Resumable", tusResumable)
		req.Header.Set("Upload-Length", strconv.FormatInt(size, 10))
		req.Header.Set("Upload-Metadata", "path "+base64.StdEncoding.EncodeToString([]byte(relative)))
	})
	if err != nil {
		return err
	}
	location := resp.Header.Get("Location")
	if err := c.expectStatus(resp, http.StatusCreated); err != nil {
		return err
	}
	uploadURL, err := sameOriginLocation(createURL, location)
	if err != nil {
		return err
	}
	chunk := make([]byte, uploadChunkSize)
	for offset := int64(0); offset < size; {
		n, err := io.ReadFull(body, chunk[:min(int64(uploadChunkSize), size-offset)])
		if err != nil {
			return fmt.Errorf("read upload content: %w", err)
		}
		resp, err := c.data(ctx, http.MethodPatch, uploadURL, bytes.NewReader(chunk[:n]), func(req *http.Request) {
			req.Header.Set("Tus-Resumable", tusResumable)
			req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
			req.Header.Set("Content-Type", "application/offset+octet-stream")
		})
		if err != nil {
			return err
		}
		next := resp.Header.Get("Upload-Offset")
		if err := c.expectStatus(resp, http.StatusNoContent); err != nil {
			return err
		}
		offset += int64(n)
		if next != strconv.FormatInt(offset, 10) {
			return fmt.Errorf("neevcloud upload offset %q after chunk, want %d", next, offset)
		}
	}
	return nil
}

// StartProcess runs a shell command and streams NDJSON stdout/stderr events until the exit event.
func (c *client) StartProcess(ctx context.Context, session shared.EnvdSandboxSession, req shared.EnvdSandboxProcessRequest) (int, error) {
	if user := strings.TrimSpace(req.User); user != "" && user != "root" {
		return 0, core.Exit(2, "provider=neevcloud runs commands as the sandbox default user; user %q is not supported", req.User)
	}
	cwd := ""
	if strings.TrimSpace(req.CWD) != "" {
		var err error
		if cwd, err = workspaceRelative(req.CWD); err != nil {
			return 0, err
		}
	}
	body := map[string]any{
		"command": "/bin/sh",
		"args":    []string{"-c", req.Command},
		"cwd":     cwd,
		"env":     execEnv(req.Env),
	}
	if timeout := min(req.Timeout, execCeiling); timeout > 0 {
		body["timeout_ms"] = timeout.Milliseconds()
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	endpoint, err := c.dataURL(session, "/v1/exec")
	if err != nil {
		return 0, err
	}
	resp, err := c.data(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded), func(httpReq *http.Request) {
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/x-ndjson")
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, c.readAPIError(resp)
	}
	return parseExecStream(resp.Body, writerOrDiscard(req.Stdout), writerOrDiscard(req.Stderr), c.apiKey)
}

// parseExecStream decodes NDJSON events; a stream without an exit event is an error.
func parseExecStream(r io.Reader, stdout, stderr io.Writer, secrets ...string) (int, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxExecEventBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event execEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return 0, fmt.Errorf("decode exec event: %w", err)
		}
		switch event.Type {
		case "stdout", "stderr":
			data, err := base64.StdEncoding.DecodeString(event.Data)
			if err != nil {
				return 0, fmt.Errorf("decode exec %s: %w", event.Type, err)
			}
			out := stdout
			if event.Type == "stderr" {
				out = stderr
			}
			if _, err := out.Write(data); err != nil {
				return 0, err
			}
		case "exit":
			if event.ExitCode == nil {
				return 0, errors.New("exec exit event has no exit_code")
			}
			return *event.ExitCode, nil
		case "error":
			return 0, fmt.Errorf("exec failed: %s: %s", event.ReasonCode, shared.RedactErrorSecrets(event.Message, secrets...))
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read exec stream: %w", err)
	}
	return 0, errors.New("exec stream ended without an exit event")
}

// control sends one authenticated lifecycle request under the project's sandboxes path.
func (c *client) control(ctx context.Context, method, suffix string, query url.Values, body, out any) error {
	endpoint := c.baseURL + "/agent/api/v1beta1/orgs/" + url.PathEscape(c.orgID) + "/projects/" + url.PathEscape(c.projectID) + "/sandboxes" + suffix
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := shared.NewJSONRequest(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := shared.SecureHTTPClient(c.httpClient, req.URL, redirectError).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return shared.DecodeUnboundedJSONResponse(resp, out, func(statusCode int, status string, data []byte) error {
		return &apiError{StatusCode: statusCode, Status: status, Body: shared.RedactErrorSecrets(core.SummarizeJSON(data), c.apiKey)}
	})
}

// data sends one runtime request; the caller owns the returned body.
func (c *client) data(ctx context.Context, method, endpoint string, body io.Reader, setHeaders func(*http.Request)) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("X-Protocol-Version", protocolVersion)
	setHeaders(req)
	return shared.SecureHTTPClient(c.dataClient, req.URL, redirectError).Do(req)
}

// expectStatus closes resp and maps any other status to an apiError.
func (c *client) expectStatus(resp *http.Response, want int) error {
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return c.readAPIError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *client) readAPIError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return &apiError{StatusCode: resp.StatusCode, Status: resp.Status, Body: shared.RedactErrorSecrets(core.SummarizeJSON(data), c.apiKey)}
}

func (c *client) dataURL(session shared.EnvdSandboxSession, suffix string) (string, error) {
	base, err := validateConnectURL(session.Domain)
	if err != nil {
		return "", err
	}
	return base + suffix, nil
}

// envd projects a lifecycle sandbox into the shared view; Domain carries connect_url.
func (sb sandbox) envd() shared.EnvdSandbox {
	labels := shared.CloneLabels(sb.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	return shared.EnvdSandbox{
		SandboxID:  sb.ID,
		Alias:      sb.Name,
		TemplateID: labels["server_type"],
		StartedAt:  sb.CreatedAt,
		State:      sandboxState(sb.Phase),
		Domain:     sb.connectURL(),
		Metadata:   labels,
	}
}

func (sb sandbox) connectURL() string {
	if sb.ConnectURL == nil {
		return ""
	}
	return strings.TrimSpace(*sb.ConnectURL)
}

// sandboxState maps phases to shared states; only Ready reports as running.
func sandboxState(phase string) string {
	if phase == phaseReady {
		return "running"
	}
	return core.Blank(strings.ToLower(strings.TrimSpace(phase)), "unknown")
}

func labelsMatch(labels, filter map[string]string) bool {
	for key, value := range filter {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// workspaceRelative maps an absolute path under /workspace to the runtime API's relative form.
func workspaceRelative(target string) (string, error) {
	clean := path.Clean(strings.TrimSpace(target))
	if clean == workspaceRoot {
		return "", nil
	}
	if !strings.HasPrefix(clean, workspaceRoot+"/") {
		return "", core.Exit(2, "neevcloud path %q must be under %s", target, workspaceRoot)
	}
	return strings.TrimPrefix(clean, workspaceRoot+"/"), nil
}

// execEnv renders sorted K=V pairs and never forwards the provider API key.
func execEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for name, value := range env {
		if slices.Contains(apiKeyEnvNames, name) {
			continue
		}
		out = append(out, name+"="+value)
	}
	slices.Sort(out)
	return out
}

// sameOriginLocation resolves a tus Location and refuses one that would send the key elsewhere.
func sameOriginLocation(base, location string) (string, error) {
	if strings.TrimSpace(location) == "" {
		return "", errors.New("neevcloud upload create returned no Location")
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	resolved, err := baseURL.Parse(location)
	if err != nil {
		return "", fmt.Errorf("neevcloud upload Location: %w", err)
	}
	if !core.SameHTTPOrigin(baseURL, resolved) {
		return "", fmt.Errorf("neevcloud upload Location %s leaves the sandbox origin", resolved.Redacted())
	}
	return resolved.String(), nil
}

func validateConnectURL(raw string) (string, error) {
	return shared.NormalizeHTTPSBaseURL(raw, shared.EndpointURLErrors{
		Invalid:    errors.New("neevcloud sandbox connect URL must be an absolute URL"),
		Components: errors.New("neevcloud sandbox connect URL must not contain userinfo, query parameters, or a fragment"),
		Insecure:   errors.New("neevcloud sandbox connect URL must use HTTPS except for loopback development endpoints"),
	})
}

func redirectError(destination *url.URL) error {
	return fmt.Errorf("neevcloud refused cross-origin redirect to %s", destination.Redacted())
}

// uploadSize reads the remaining size of a file without buffering it; other readers are buffered.
func uploadSize(r io.Reader) (int64, io.Reader, error) {
	if file, ok := r.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return 0, nil, fmt.Errorf("stat upload content: %w", err)
		}
		offset, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, nil, fmt.Errorf("seek upload content: %w", err)
		}
		return info.Size() - offset, file, nil
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return 0, nil, fmt.Errorf("read upload content: %w", err)
	}
	return int64(len(content)), bytes.NewReader(content), nil
}

func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
