package e2b

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type e2bClient struct {
	apiKey     string
	apiURL     string
	domain     string
	user       string
	httpClient *http.Client
	envdClient *http.Client
}

const e2bControlTimeout = 60 * time.Second

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

var newE2BClient = func(cfg core.Config, rt core.Runtime) (shared.EnvdSandboxAPI, error) {
	apiKey := strings.TrimSpace(cfg.E2B.APIKey)
	if apiKey == "" {
		return nil, core.Exit(2, "provider=e2b requires E2B_API_KEY")
	}
	httpClient, envdClient := shared.ControlAndDataHTTPClients(rt.HTTP, e2bControlTimeout)
	apiURL, err := validateE2BAPIURL(core.Blank(cfg.E2B.APIURL, core.E2BConfigDefaultAPIURL))
	if err != nil {
		return nil, err
	}
	domain := strings.TrimSpace(core.Blank(cfg.E2B.Domain, core.E2BConfigDefaultDomain))
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
		Invalid:    core.Exit(2, "provider=e2b API URL must be an absolute HTTPS URL"),
		Components: core.Exit(2, "provider=e2b API URL must not contain userinfo, query parameters, or a fragment"),
		Insecure:   core.Exit(2, "provider=e2b API URL must use HTTPS except for loopback development endpoints"),
	})
}

func e2bRedirectError(destination *url.URL) error {
	return fmt.Errorf("e2b refused cross-origin redirect to %s", destination.Redacted())
}

func (c *e2bClient) CreateSandbox(ctx context.Context, req shared.EnvdSandboxCreateRequest) (shared.EnvdSandbox, error) {
	body := map[string]any{
		"templateID":            req.TemplateID,
		"timeout":               req.TimeoutSeconds,
		"secure":                true,
		"allow_internet_access": req.AllowInternetAccess,
		"metadata":              req.Metadata,
	}
	return c.control().CreateSandbox(ctx, body, req.Metadata)
}

func (c *e2bClient) ConnectSandbox(ctx context.Context, sandboxID string, timeoutSeconds int) (shared.EnvdSandboxSession, error) {
	sandbox, err := c.control().ConnectSandbox(ctx, sandboxID, timeoutSeconds)
	if err != nil {
		return shared.EnvdSandboxSession{}, err
	}
	return c.sessionFromSandbox(sandbox), nil
}

func (c *e2bClient) GetSandbox(ctx context.Context, sandboxID string) (shared.EnvdSandbox, error) {
	return c.control().GetSandbox(ctx, sandboxID)
}

func (c *e2bClient) ListSandboxes(ctx context.Context, metadata map[string]string) ([]shared.EnvdSandbox, error) {
	return c.control().ListSandboxes(ctx, metadata)
}

func (c *e2bClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return c.control().DeleteSandbox(ctx, sandboxID)
}

func (c *e2bClient) UploadFile(ctx context.Context, session shared.EnvdSandboxSession, targetPath string, r io.Reader) error {
	return shared.UploadEnvdFile(ctx, shared.EnvdUploadFileRequest{
		Endpoint:       c.envdURL(session, "/files"),
		TargetPath:     targetPath,
		User:           c.user,
		AccessToken:    session.EnvdAccessToken,
		Content:        r,
		HTTPClient:     c.dataPlaneHTTPClient(),
		SetHeaders:     func(req *http.Request) { c.setEnvdHeaders(req, session) },
		RedirectError:  e2bRedirectError,
		SummarizeError: core.SummarizeJSON,
		APIError: func(statusCode int, status, body string) error {
			return &e2bAPIError{StatusCode: statusCode, Status: status, Body: body}
		},
	})
}

func (c *e2bClient) StartProcess(ctx context.Context, session shared.EnvdSandboxSession, req shared.EnvdSandboxProcessRequest) (int, error) {
	return shared.StartEnvdProcess(ctx, shared.EnvdProcessRequest{
		EnvdSandboxProcessRequest: req,
		Endpoint:                  c.envdURL(session, "/process.Process/Start"),
		AccessToken:               session.EnvdAccessToken,
		HTTPClient:                c.dataPlaneHTTPClient(),
		SetHeaders:                func(httpReq *http.Request) { c.setEnvdHeaders(httpReq, session) },
		RedirectError:             e2bRedirectError,
		Provider:                  "e2b",
		InterpretEnd:              interpretE2BProcessEnd,
		SummarizeError:            core.SummarizeJSON,
		APIError: func(statusCode int, status, body string) error {
			return &e2bAPIError{StatusCode: statusCode, Status: status, Body: body}
		},
	})
}

func (c *e2bClient) doJSONWithHeaders(ctx context.Context, method, path string, query url.Values, body any, out any) (http.Header, error) {
	endpoint := c.apiURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := shared.NewJSONRequest(ctx, method, endpoint, body)
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
	if err := shared.DecodeUnboundedJSONResponse(resp, out, func(statusCode int, status string, data []byte) error {
		return &e2bAPIError{StatusCode: statusCode, Status: status, Body: shared.RedactErrorSecrets(core.SummarizeJSON(data), c.apiKey)}
	}); err != nil {
		return nil, err
	}
	return resp.Header.Clone(), nil
}

func (c *e2bClient) sessionFromSandbox(sandbox shared.EnvdSandbox) shared.EnvdSandboxSession {
	domain := strings.TrimSpace(sandbox.Domain)
	if domain == "" {
		domain = c.domain
	}
	return shared.EnvdSandboxSession{
		SandboxID:       sandbox.SandboxID,
		EnvdVersion:     sandbox.EnvdVersion,
		EnvdAccessToken: sandbox.EnvdAccessToken,
		Domain:          domain,
	}
}

func (c *e2bClient) envdURL(session shared.EnvdSandboxSession, path string) string {
	domain := strings.TrimSpace(session.Domain)
	if domain == "" {
		domain = c.domain
	}
	return "https://49983-" + session.SandboxID + "." + domain + path
}

func (c *e2bClient) setEnvdHeaders(req *http.Request, session shared.EnvdSandboxSession) {
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

func (c *e2bClient) control() shared.EnvdSandboxControl {
	return shared.EnvdSandboxControl{Request: c.doJSONWithHeaders}
}
