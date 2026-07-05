package orgo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const defaultAPIBase = "https://www.orgo.ai/api"

type orgoAPI interface {
	CreateWorkspace(ctx context.Context, name string) (orgoWorkspace, error)
	DeleteWorkspace(ctx context.Context, id string) error
	ListWorkspaces(ctx context.Context) ([]orgoWorkspace, error)
	GetWorkspace(ctx context.Context, id string) (orgoWorkspace, error)
	CreateComputer(ctx context.Context, req orgoCreateComputerRequest) (orgoComputer, error)
	GetComputer(ctx context.Context, id string) (orgoComputer, error)
	StartComputer(ctx context.Context, id string) error
	DeleteComputer(ctx context.Context, id string) error
	RunBash(ctx context.Context, id, command string, stdout, stderr io.Writer) (int, error)
}

type orgoWorkspace struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Status    string         `json:"status"`
	CreatedAt string         `json:"created_at"`
	Computers []orgoComputer `json:"desktops"`
}

type orgoComputer struct {
	ID            string `json:"id"`
	InstanceID    string `json:"instance_id"`
	Name          string `json:"name"`
	WorkspaceID   string `json:"workspace_id"`
	Status        string `json:"status"`
	OS            string `json:"os"`
	RAMGB         int    `json:"ram"`
	CPUs          int    `json:"cpu"`
	DiskGB        int    `json:"disk_size_gb"`
	Resolution    string `json:"resolution"`
	Hostname      string `json:"hostname"`
	ConnectionURL string `json:"connection_url"`
	CreatedAt     string `json:"created_at"`
}

type orgoCreateComputerRequest struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	OS          string `json:"os"`
	RAMGB       int    `json:"ram"`
	CPUs        int    `json:"cpu"`
	DiskGB      int    `json:"disk_size_gb"`
	Resolution  string `json:"resolution,omitempty"`
}

type orgoBashResponse struct {
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	Output        string `json:"output"`
	Result        string `json:"result"`
	Text          string `json:"text"`
	Message       string `json:"message"`
	ExitCode      *int   `json:"exit_code"`
	ExitCodeCamel *int   `json:"exitCode"`
	Success       *bool  `json:"success"`
}

type orgoActionResponse struct {
	Success *bool  `json:"success"`
	Status  string `json:"status"`
}

type orgoHTTPClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type orgoHTTPError struct {
	StatusCode int
	Body       string
}

func (e *orgoHTTPError) Error() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("orgo API http %d", e.StatusCode)
	}
	return fmt.Sprintf("orgo API http %d: %s", e.StatusCode, body)
}

func (e *orgoHTTPError) As(target any) bool {
	t, ok := target.(*ExitError)
	if !ok {
		return false
	}
	code := 1
	switch {
	case e.StatusCode == http.StatusNotFound:
		code = 4
	case e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden:
		code = 77
	case e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500:
		code = 69
	}
	*t = ExitError{Code: code, Message: e.Error()}
	return true
}

func newOrgoClient(cfg Config, rt Runtime) (orgoAPI, error) {
	apiKey := strings.TrimSpace(os.Getenv("CRABBOX_ORGO_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.Orgo.APIKey)
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("ORGO_API_KEY"))
	}
	if apiKey == "" {
		return nil, exit(2, "provider=%s requires CRABBOX_ORGO_API_KEY, orgo.apiKey, or ORGO_API_KEY", providerName)
	}
	baseURL := strings.TrimSpace(cfg.Orgo.APIBase)
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("CRABBOX_ORGO_API_BASE"))
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("ORGO_API_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = defaultAPIBase
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, exit(2, "provider=%s invalid Orgo API base URL %q: %v", providerName, baseURL, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, exit(2, "provider=%s invalid Orgo API base URL %q", providerName, baseURL)
	}
	if parsed.Scheme != "https" && !isOrgoLoopbackHTTP(parsed) {
		return nil, exit(2, "provider=%s API base URL %q must use https unless it targets localhost", providerName, baseURL)
	}
	client := rt.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	return &orgoHTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    client,
	}, nil
}

func isOrgoLoopbackHTTP(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func (c *orgoHTTPClient) CreateWorkspace(ctx context.Context, name string) (orgoWorkspace, error) {
	var workspace orgoWorkspace
	err := c.doJSON(ctx, http.MethodPost, "/workspaces", map[string]string{"name": name}, &workspace)
	return workspace, err
}

func (c *orgoHTTPClient) DeleteWorkspace(ctx context.Context, id string) error {
	return c.deleteResource(ctx, "/workspaces/"+url.PathEscape(id), "workspace", id)
}

func (c *orgoHTTPClient) ListWorkspaces(ctx context.Context) ([]orgoWorkspace, error) {
	var envelope struct {
		Projects   []orgoWorkspace `json:"projects"`
		Workspaces []orgoWorkspace `json:"workspaces"`
		Data       []orgoWorkspace `json:"data"`
		Items      []orgoWorkspace `json:"items"`
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/workspaces", nil, &raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		switch {
		case envelope.Projects != nil:
			return envelope.Projects, nil
		case envelope.Workspaces != nil:
			return envelope.Workspaces, nil
		case envelope.Data != nil:
			return envelope.Data, nil
		case envelope.Items != nil:
			return envelope.Items, nil
		}
	}
	var workspaces []orgoWorkspace
	if err := json.Unmarshal(raw, &workspaces); err != nil {
		return nil, fmt.Errorf("decode orgo workspaces: %w", err)
	}
	return workspaces, nil
}

func (c *orgoHTTPClient) GetWorkspace(ctx context.Context, id string) (orgoWorkspace, error) {
	var workspace orgoWorkspace
	err := c.doJSON(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(id), nil, &workspace)
	return workspace, err
}

func (c *orgoHTTPClient) CreateComputer(ctx context.Context, req orgoCreateComputerRequest) (orgoComputer, error) {
	var computer orgoComputer
	err := c.doJSON(ctx, http.MethodPost, "/computers", req, &computer)
	return computer, err
}

func (c *orgoHTTPClient) GetComputer(ctx context.Context, id string) (orgoComputer, error) {
	var computer orgoComputer
	err := c.doJSON(ctx, http.MethodGet, "/computers/"+url.PathEscape(id), nil, &computer)
	return computer, err
}

func (c *orgoHTTPClient) StartComputer(ctx context.Context, id string) error {
	var res orgoActionResponse
	if err := c.doJSON(ctx, http.MethodPost, "/computers/"+url.PathEscape(id)+"/start", nil, &res); err != nil {
		return err
	}
	if res.Success == nil || !*res.Success {
		return fmt.Errorf("orgo did not start computer %s (status=%s)", id, strings.TrimSpace(res.Status))
	}
	return nil
}

func (c *orgoHTTPClient) DeleteComputer(ctx context.Context, id string) error {
	return c.deleteResource(ctx, "/computers/"+url.PathEscape(id), "computer", id)
}

func (c *orgoHTTPClient) deleteResource(ctx context.Context, path, kind, id string) error {
	var res orgoActionResponse
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &res); err != nil {
		return err
	}
	if res.Success != nil && !*res.Success {
		return fmt.Errorf("orgo did not delete %s %s (status=%s)", kind, id, strings.TrimSpace(res.Status))
	}
	return nil
}

func (c *orgoHTTPClient) RunBash(ctx context.Context, id, command string, stdout, stderr io.Writer) (int, error) {
	var res orgoBashResponse
	if err := c.doJSON(ctx, http.MethodPost, "/computers/"+url.PathEscape(id)+"/bash", map[string]string{"command": command}, &res); err != nil {
		return 1, err
	}
	if res.Stdout != "" {
		fmt.Fprint(stdout, res.Stdout)
	} else if res.Output != "" {
		fmt.Fprint(stdout, res.Output)
	} else if res.Result != "" {
		fmt.Fprint(stdout, res.Result)
	} else if res.Text != "" {
		fmt.Fprint(stdout, res.Text)
	} else if res.Message != "" {
		fmt.Fprint(stdout, res.Message)
	}
	if res.Stderr != "" {
		fmt.Fprint(stderr, res.Stderr)
	}
	if res.ExitCodeCamel != nil {
		return *res.ExitCodeCamel, nil
	}
	if res.ExitCode != nil {
		return *res.ExitCode, nil
	}
	if res.Success != nil && !*res.Success {
		return 1, nil
	}
	return 0, nil
}

func (c *orgoHTTPClient) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(body); err != nil {
			return err
		}
		reader = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &orgoHTTPError{StatusCode: res.StatusCode, Body: string(data)}
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], data...)
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode orgo response: %w", err)
	}
	return nil
}
