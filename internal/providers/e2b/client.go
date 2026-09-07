package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

type e2bAPI interface {
	CreateSandbox(context.Context, e2bCreateSandboxRequest) (e2bSandbox, error)
	ConnectSandbox(context.Context, string, int) (e2bSession, error)
	GetSandbox(context.Context, string) (e2bSandbox, error)
	ListSandboxes(context.Context, map[string]string) ([]e2bSandbox, error)
	DeleteSandbox(context.Context, string) error
	UploadFile(context.Context, e2bSession, string, io.Reader) error
	StartProcess(context.Context, e2bSession, e2bProcessRequest) (int, error)
}

type e2bClient struct {
	apiKey     string
	apiURL     string
	domain     string
	user       string
	httpClient *http.Client
	envdClient *http.Client
}

const e2bControlTimeout = 60 * time.Second

type e2bCreateSandboxRequest struct {
	TemplateID          string
	TimeoutSeconds      int
	Metadata            map[string]string
	AllowInternetAccess bool
}

type e2bSandbox struct {
	TemplateID      string            `json:"templateID"`
	SandboxID       string            `json:"sandboxID"`
	ClientID        string            `json:"clientID"`
	StartedAt       string            `json:"startedAt"`
	EndAt           string            `json:"endAt"`
	EnvdVersion     string            `json:"envdVersion"`
	EnvdAccessToken string            `json:"envdAccessToken"`
	TrafficToken    string            `json:"trafficAccessToken"`
	Alias           string            `json:"alias"`
	Domain          string            `json:"domain"`
	State           string            `json:"state"`
	CPUCount        int               `json:"cpuCount"`
	MemoryMB        int               `json:"memoryMB"`
	DiskSizeMB      int               `json:"diskSizeMB"`
	Metadata        map[string]string `json:"metadata"`
}

type e2bSession struct {
	SandboxID       string
	EnvdVersion     string
	EnvdAccessToken string
	Domain          string
}

type e2bProcessRequest struct {
	Command string
	CWD     string
	Env     map[string]string
	User    string
	Timeout time.Duration
	Stdout  io.Writer
	Stderr  io.Writer
}

type e2bAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *e2bAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

var newE2BClient = func(cfg Config, rt Runtime) (e2bAPI, error) {
	apiKey := strings.TrimSpace(cfg.E2B.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=e2b requires E2B_API_KEY")
	}
	httpClient, envdClient := shared.ControlAndDataHTTPClients(rt.HTTP, e2bControlTimeout)
	apiURL, err := validateE2BAPIURL(blank(cfg.E2B.APIURL, "https://api.e2b.app"))
	if err != nil {
		return nil, err
	}
	domain := strings.TrimSpace(blank(cfg.E2B.Domain, "e2b.app"))
	return &e2bClient{
		apiKey:     apiKey,
		apiURL:     apiURL,
		domain:     domain,
		user:       cfg.E2B.User,
		httpClient: httpClient,
		envdClient: envdClient,
	}, nil
}

func validateE2BAPIURL(raw string) (string, error) {
	return shared.NormalizeHTTPSURL(raw, shared.EndpointURLErrors{
		Invalid:    exit(2, "provider=e2b API URL must be an absolute HTTPS URL"),
		Components: exit(2, "provider=e2b API URL must not contain userinfo, query parameters, or a fragment"),
		Insecure:   exit(2, "provider=e2b API URL must use HTTPS except for loopback development endpoints"),
	})
}

func e2bRedirectError(destination *url.URL) error {
	return fmt.Errorf("e2b refused cross-origin redirect to %s", destination.Redacted())
}

func (c *e2bClient) CreateSandbox(ctx context.Context, req e2bCreateSandboxRequest) (e2bSandbox, error) {
	body := map[string]any{
		"templateID":            req.TemplateID,
		"timeout":               req.TimeoutSeconds,
		"secure":                true,
		"allow_internet_access": req.AllowInternetAccess,
		"metadata":              req.Metadata,
	}
	var sandbox e2bSandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes", nil, body, &sandbox); err != nil {
		return e2bSandbox{}, err
	}
	if sandbox.Metadata == nil {
		sandbox.Metadata = req.Metadata
	}
	if sandbox.State == "" {
		sandbox.State = "running"
	}
	return sandbox, nil
}

func (c *e2bClient) ConnectSandbox(ctx context.Context, sandboxID string, timeoutSeconds int) (e2bSession, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 300
	}
	body := map[string]any{"timeout": timeoutSeconds}
	var sandbox e2bSandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(sandboxID)+"/connect", nil, body, &sandbox); err != nil {
		return e2bSession{}, err
	}
	if shared.ValidateResourceID(sandboxID, sandbox.SandboxID) != nil {
		return e2bSession{}, errors.New("connect sandbox returned a different or missing sandbox ID")
	}
	return c.sessionFromSandbox(sandbox), nil
}

func (c *e2bClient) GetSandbox(ctx context.Context, sandboxID string) (e2bSandbox, error) {
	var sandbox e2bSandbox
	if err := c.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(sandboxID), nil, nil, &sandbox); err != nil {
		return e2bSandbox{}, err
	}
	if shared.ValidateResourceID(sandboxID, sandbox.SandboxID) != nil {
		return e2bSandbox{}, errors.New("get sandbox returned a different or missing sandbox ID")
	}
	if sandbox.Metadata == nil {
		sandbox.Metadata = map[string]string{}
	}
	return sandbox, nil
}

func (c *e2bClient) ListSandboxes(ctx context.Context, metadata map[string]string) ([]e2bSandbox, error) {
	var all []e2bSandbox
	nextToken := ""
	for {
		query := url.Values{}
		query.Set("limit", "100")
		query.Set("state", "running,paused")
		if nextToken != "" {
			query.Set("nextToken", nextToken)
		}
		if len(metadata) > 0 {
			values := url.Values{}
			for key, value := range metadata {
				values.Set(key, value)
			}
			query.Set("metadata", values.Encode())
		}
		var page []e2bSandbox
		headers, err := c.doJSONWithHeaders(ctx, http.MethodGet, "/v2/sandboxes", query, nil, &page)
		if err != nil {
			return nil, err
		}
		for i := range page {
			if page[i].Metadata == nil {
				page[i].Metadata = map[string]string{}
			}
		}
		all = append(all, page...)
		nextToken = headers.Get("x-next-token")
		if nextToken == "" {
			return all, nil
		}
	}
}

func (c *e2bClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(sandboxID), nil, nil, nil)
}

func (c *e2bClient) UploadFile(ctx context.Context, session e2bSession, targetPath string, r io.Reader) error {
	return shared.UploadEnvdFile(ctx, shared.EnvdUploadFileRequest{
		Endpoint:       c.envdURL(session, "/files"),
		TargetPath:     targetPath,
		User:           c.user,
		AccessToken:    session.EnvdAccessToken,
		Content:        r,
		HTTPClient:     c.dataPlaneHTTPClient(),
		SetHeaders:     func(req *http.Request) { c.setEnvdHeaders(req, session) },
		RedirectError:  e2bRedirectError,
		SummarizeError: summarizeJSON,
		APIError: func(statusCode int, status, body string) error {
			return &e2bAPIError{StatusCode: statusCode, Status: status, Body: body}
		},
	})
}

func (c *e2bClient) StartProcess(ctx context.Context, session e2bSession, req e2bProcessRequest) (int, error) {
	return shared.StartEnvdProcess(ctx, shared.EnvdProcessRequest{
		Endpoint:       c.envdURL(session, "/process.Process/Start"),
		Command:        req.Command,
		CWD:            req.CWD,
		Env:            req.Env,
		User:           req.User,
		Timeout:        req.Timeout,
		Stdout:         req.Stdout,
		Stderr:         req.Stderr,
		AccessToken:    session.EnvdAccessToken,
		HTTPClient:     c.dataPlaneHTTPClient(),
		SetHeaders:     func(httpReq *http.Request) { c.setEnvdHeaders(httpReq, session) },
		RedirectError:  e2bRedirectError,
		Provider:       "e2b",
		InterpretEnd:   interpretE2BProcessEnd,
		SummarizeError: summarizeJSON,
		APIError: func(statusCode int, status, body string) error {
			return &e2bAPIError{StatusCode: statusCode, Status: status, Body: body}
		},
	})
}

func (c *e2bClient) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	_, err := c.doJSONWithHeaders(ctx, method, path, query, body, out)
	return err
}

func (c *e2bClient) doJSONWithHeaders(ctx context.Context, method, path string, query url.Values, body any, out any) (http.Header, error) {
	var r io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
		r = &buf
	}
	endpoint := c.apiURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := shared.SecureHTTPClient(c.httpClient, req.URL, e2bRedirectError).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &e2bAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: shared.RedactErrorSecrets(summarizeJSON(data), c.apiKey)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, err
		}
	}
	return resp.Header.Clone(), nil
}

func (c *e2bClient) sessionFromSandbox(sandbox e2bSandbox) e2bSession {
	domain := strings.TrimSpace(sandbox.Domain)
	if domain == "" {
		domain = c.domain
	}
	return e2bSession{
		SandboxID:       sandbox.SandboxID,
		EnvdVersion:     sandbox.EnvdVersion,
		EnvdAccessToken: sandbox.EnvdAccessToken,
		Domain:          domain,
	}
}

func (c *e2bClient) envdURL(session e2bSession, path string) string {
	domain := strings.TrimSpace(session.Domain)
	if domain == "" {
		domain = c.domain
	}
	return "https://49983-" + session.SandboxID + "." + domain + path
}

func (c *e2bClient) setEnvdHeaders(req *http.Request, session e2bSession) {
	req.Header.Set("X-Access-Token", session.EnvdAccessToken)
	req.Header.Set("E2b-Sandbox-Id", session.SandboxID)
	req.Header.Set("E2b-Sandbox-Port", "49983")
}

func (c *e2bClient) dataPlaneHTTPClient() *http.Client {
	if c.envdClient != nil {
		return c.envdClient
	}
	return &http.Client{Timeout: 0}
}

func interpretE2BProcessEnd(end shared.EnvdProcessEnd, stderr io.Writer, secrets ...string) (int, error) {
	if !end.Exited && end.Error != "" {
		fmt.Fprintln(stderr, shared.RedactErrorSecrets(end.Error, secrets...))
	}
	return end.ExitCode, nil
}
