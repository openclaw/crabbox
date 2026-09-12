package cubesandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type cubesandboxAPI interface {
	CreateSandbox(context.Context, cubesandboxCreateSandboxRequest) (cubesandboxSandbox, error)
	ConnectSandbox(context.Context, string, int) (cubesandboxSession, error)
	GetSandbox(context.Context, string) (cubesandboxSandbox, error)
	ListSandboxes(context.Context, map[string]string) ([]cubesandboxSandbox, error)
	DeleteSandbox(context.Context, string) error
	UploadFile(context.Context, cubesandboxSession, string, io.Reader) error
	StartProcess(context.Context, cubesandboxSession, cubesandboxProcessRequest) (int, error)
}

type cubesandboxClient struct {
	apiKey      string
	apiURL      string
	domain      string
	user        string
	proxyScheme string
	httpClient  *http.Client
	envdClient  *http.Client
}

const (
	cubesandboxEnvdPort       = 49983
	cubesandboxControlTimeout = 60 * time.Second
)

type cubesandboxCreateSandboxRequest struct {
	TemplateID          string
	TimeoutSeconds      int
	Metadata            map[string]string
	AllowInternetAccess bool
}

type cubesandboxSandbox struct {
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

type cubesandboxSession struct {
	SandboxID       string
	EnvdVersion     string
	EnvdAccessToken string
	Domain          string
}

type cubesandboxProcessRequest struct {
	Command string
	CWD     string
	Env     map[string]string
	User    string
	Timeout time.Duration
	Stdout  io.Writer
	Stderr  io.Writer
}

type cubesandboxAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *cubesandboxAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

var newCubeSandboxClient = func(cfg Config, rt Runtime) (cubesandboxAPI, error) {
	apiKey := strings.TrimSpace(cfg.CubeSandbox.APIKey)
	httpClient, dataPlaneClient := shared.ControlAndDataHTTPClients(rt.HTTP, cubesandboxControlTimeout)
	apiURL, err := validateCubeSandboxAPIURL(core.Blank(cfg.CubeSandbox.APIURL, "http://127.0.0.1:3000"))
	if err != nil {
		return nil, err
	}
	domain := strings.TrimSpace(core.Blank(cfg.CubeSandbox.Domain, "cube.app"))
	proxyScheme, err := cubeSandboxProxyScheme(cfg.CubeSandbox.ProxyScheme, cfg.CubeSandbox.ProxyPortHTTP)
	if err != nil {
		return nil, err
	}
	user, err := cubesandboxProcessUser(cfg.CubeSandbox.User)
	if err != nil {
		return nil, err
	}
	proxyPort := cfg.CubeSandbox.ProxyPortHTTP
	if proxyPort <= 0 {
		proxyPort = 80
	}
	envdClient, err := cubeSandboxDataPlaneHTTPClient(dataPlaneClient, strings.TrimSpace(cfg.CubeSandbox.ProxyNodeIP), proxyPort)
	if err != nil {
		return nil, err
	}
	return &cubesandboxClient{
		apiKey:      apiKey,
		apiURL:      apiURL,
		domain:      domain,
		user:        user,
		proxyScheme: proxyScheme,
		httpClient:  httpClient,
		envdClient:  envdClient,
	}, nil
}

func cubeSandboxDataPlaneHTTPClient(source *http.Client, proxyHost string, proxyPort int) (*http.Client, error) {
	proxyHost = strings.TrimSpace(proxyHost)
	if proxyHost == "" {
		return source, nil
	}
	transport := source.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	base, ok := transport.(*http.Transport)
	if !ok {
		return nil, exit(2, "provider=cubesandbox CubeProxy direct routing requires an HTTP transport that supports a dial override")
	}
	clone := base.Clone()
	dialContext := clone.DialContext
	if dialContext == nil {
		dialContext = (&net.Dialer{}).DialContext
	}
	if strings.HasPrefix(proxyHost, "[") && strings.HasSuffix(proxyHost, "]") {
		proxyHost = strings.TrimSuffix(strings.TrimPrefix(proxyHost, "["), "]")
	}
	dialTarget := net.JoinHostPort(proxyHost, strconv.Itoa(proxyPort))
	clone.Proxy = nil
	clone.DialTLSContext = nil
	clone.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialContext(ctx, network, dialTarget)
	}
	client := *source
	client.Transport = clone
	return &client, nil
}

func validateCubeSandboxAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=cubesandbox API URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=cubesandbox API URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", exit(2, "provider=cubesandbox API URL must use HTTP or HTTPS")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func cubeSandboxProxyScheme(scheme string, port int) (string, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(scheme)); normalized {
	case "http", "https":
		return normalized, nil
	case "":
		if port == 443 {
			return "https", nil
		}
		return "http", nil
	default:
		return "", exit(2, "provider=cubesandbox proxy scheme %q must be http or https", scheme)
	}
}

func cubeSandboxRedirectError(destination *url.URL) error {
	return fmt.Errorf("cubesandbox refused cross-origin redirect to %s", destination.Redacted())
}

func (c *cubesandboxClient) CreateSandbox(ctx context.Context, req cubesandboxCreateSandboxRequest) (cubesandboxSandbox, error) {
	body := map[string]any{
		"templateID": req.TemplateID,
		"timeout":    req.TimeoutSeconds,
		"metadata":   req.Metadata,
	}
	if !req.AllowInternetAccess {
		body["allowInternetAccess"] = false
	}
	var sandbox cubesandboxSandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes", nil, body, &sandbox); err != nil {
		return cubesandboxSandbox{}, err
	}
	if sandbox.Metadata == nil {
		sandbox.Metadata = req.Metadata
	}
	if sandbox.State == "" {
		sandbox.State = "running"
	}
	return sandbox, nil
}

func (c *cubesandboxClient) ConnectSandbox(ctx context.Context, sandboxID string, timeoutSeconds int) (cubesandboxSession, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 300
	}
	body := map[string]any{"timeout": timeoutSeconds}
	var sandbox cubesandboxSandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(sandboxID)+"/connect", nil, body, &sandbox); err != nil {
		return cubesandboxSession{}, err
	}
	if shared.ValidateResourceID(sandboxID, sandbox.SandboxID) != nil {
		return cubesandboxSession{}, errors.New("connect sandbox returned a different or missing sandbox ID")
	}
	return c.sessionFromSandbox(sandbox), nil
}

func (c *cubesandboxClient) GetSandbox(ctx context.Context, sandboxID string) (cubesandboxSandbox, error) {
	var sandbox cubesandboxSandbox
	if err := c.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(sandboxID), nil, nil, &sandbox); err != nil {
		return cubesandboxSandbox{}, err
	}
	if shared.ValidateResourceID(sandboxID, sandbox.SandboxID) != nil {
		return cubesandboxSandbox{}, errors.New("get sandbox returned a different or missing sandbox ID")
	}
	if sandbox.Metadata == nil {
		sandbox.Metadata = map[string]string{}
	}
	return sandbox, nil
}

func (c *cubesandboxClient) ListSandboxes(ctx context.Context, metadata map[string]string) ([]cubesandboxSandbox, error) {
	var all []cubesandboxSandbox
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
		var page []cubesandboxSandbox
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

func (c *cubesandboxClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(sandboxID), nil, nil, nil)
}

func (c *cubesandboxClient) UploadFile(ctx context.Context, session cubesandboxSession, targetPath string, r io.Reader) error {
	return shared.UploadEnvdFile(ctx, shared.EnvdUploadFileRequest{
		Endpoint:       c.envdURL(session, "/files"),
		TargetPath:     targetPath,
		User:           c.user,
		AccessToken:    session.EnvdAccessToken,
		Content:        r,
		HTTPClient:     c.dataPlaneHTTPClient(),
		SetHeaders:     func(req *http.Request) { c.setEnvdHeaders(req, session) },
		RedirectError:  cubeSandboxRedirectError,
		SummarizeError: summarizeJSON,
		APIError: func(statusCode int, status, body string) error {
			return &cubesandboxAPIError{StatusCode: statusCode, Status: status, Body: body}
		},
	})
}

func (c *cubesandboxClient) StartProcess(ctx context.Context, session cubesandboxSession, req cubesandboxProcessRequest) (int, error) {
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
		RedirectError:  cubeSandboxRedirectError,
		Provider:       "cubesandbox",
		InterpretEnd:   interpretCubeSandboxProcessEnd,
		SummarizeError: summarizeJSON,
		APIError: func(statusCode int, status, body string) error {
			return &cubesandboxAPIError{StatusCode: statusCode, Status: status, Body: body}
		},
	})
}

func (c *cubesandboxClient) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	_, err := c.doJSONWithHeaders(ctx, method, path, query, body, out)
	return err
}

func (c *cubesandboxClient) doJSONWithHeaders(ctx context.Context, method, path string, query url.Values, body any, out any) (http.Header, error) {
	endpoint := c.apiURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := shared.NewJSONRequest(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := shared.SecureHTTPClient(c.httpClient, req.URL, cubeSandboxRedirectError).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := shared.DecodeUnboundedJSONResponse(resp, out, func(statusCode int, status string, data []byte) error {
		return &cubesandboxAPIError{StatusCode: statusCode, Status: status, Body: shared.RedactErrorSecrets(summarizeJSON(data), c.apiKey)}
	}); err != nil {
		return nil, err
	}
	return resp.Header.Clone(), nil
}

func (c *cubesandboxClient) sessionFromSandbox(sandbox cubesandboxSandbox) cubesandboxSession {
	domain := strings.TrimSpace(sandbox.Domain)
	if domain == "" {
		domain = c.domain
	}
	return cubesandboxSession{
		SandboxID:       sandbox.SandboxID,
		EnvdVersion:     sandbox.EnvdVersion,
		EnvdAccessToken: sandbox.EnvdAccessToken,
		Domain:          domain,
	}
}

func (c *cubesandboxClient) envdURL(session cubesandboxSession, path string) string {
	domain := strings.TrimSpace(session.Domain)
	if domain == "" {
		domain = c.domain
	}
	virtualHost := fmt.Sprintf("%d-%s.%s", cubesandboxEnvdPort, session.SandboxID, domain)
	scheme := c.proxyScheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + virtualHost + path
}

func (c *cubesandboxClient) setEnvdHeaders(req *http.Request, session cubesandboxSession) {
	req.Header.Set("X-Access-Token", session.EnvdAccessToken)
	req.Header.Set("E2b-Sandbox-Id", session.SandboxID)
	req.Header.Set("E2b-Sandbox-Port", strconv.Itoa(cubesandboxEnvdPort))
}

func (c *cubesandboxClient) dataPlaneHTTPClient() *http.Client {
	if c.envdClient != nil {
		return c.envdClient
	}
	return &http.Client{Timeout: 0}
}

func interpretCubeSandboxProcessEnd(end shared.EnvdProcessEnd, stderr io.Writer, secrets ...string) (int, error) {
	if end.Exited {
		return end.ExitCode, nil
	}
	detail := strings.TrimSpace(end.Error)
	if detail == "" {
		detail = strings.TrimSpace(end.Status)
	}
	if detail == "" {
		detail = "process did not exit normally"
	}
	detail = shared.RedactErrorSecrets(detail, secrets...)
	fmt.Fprintln(stderr, detail)
	code := end.ExitCode
	if code == 0 {
		code = 1
	}
	return code, shared.ObservedProcessEndError(fmt.Sprintf("cubesandbox process did not exit normally: %s", detail))
}
