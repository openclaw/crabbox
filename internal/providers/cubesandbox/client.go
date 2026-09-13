package cubesandbox

import (
	"context"
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

var newCubeSandboxClient = func(cfg core.Config, rt core.Runtime) (shared.EnvdSandboxAPI, error) {
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
		return nil, core.Exit(2, "provider=cubesandbox CubeProxy direct routing requires an HTTP transport that supports a dial override")
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
		return "", core.Exit(2, "provider=cubesandbox API URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", core.Exit(2, "provider=cubesandbox API URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", core.Exit(2, "provider=cubesandbox API URL must use HTTP or HTTPS")
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
		return "", core.Exit(2, "provider=cubesandbox proxy scheme %q must be http or https", scheme)
	}
}

func cubeSandboxRedirectError(destination *url.URL) error {
	return fmt.Errorf("cubesandbox refused cross-origin redirect to %s", destination.Redacted())
}

func (c *cubesandboxClient) CreateSandbox(ctx context.Context, req shared.EnvdSandboxCreateRequest) (shared.EnvdSandbox, error) {
	body := map[string]any{
		"templateID": req.TemplateID,
		"timeout":    req.TimeoutSeconds,
		"metadata":   req.Metadata,
	}
	if !req.AllowInternetAccess {
		body["allowInternetAccess"] = false
	}
	return c.control().CreateSandbox(ctx, body, req.Metadata)
}

func (c *cubesandboxClient) ConnectSandbox(ctx context.Context, sandboxID string, timeoutSeconds int) (shared.EnvdSandboxSession, error) {
	sandbox, err := c.control().ConnectSandbox(ctx, sandboxID, timeoutSeconds)
	if err != nil {
		return shared.EnvdSandboxSession{}, err
	}
	return c.sessionFromSandbox(sandbox), nil
}

func (c *cubesandboxClient) GetSandbox(ctx context.Context, sandboxID string) (shared.EnvdSandbox, error) {
	return c.control().GetSandbox(ctx, sandboxID)
}

func (c *cubesandboxClient) ListSandboxes(ctx context.Context, metadata map[string]string) ([]shared.EnvdSandbox, error) {
	return c.control().ListSandboxes(ctx, metadata)
}

func (c *cubesandboxClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return c.control().DeleteSandbox(ctx, sandboxID)
}

func (c *cubesandboxClient) UploadFile(ctx context.Context, session shared.EnvdSandboxSession, targetPath string, r io.Reader) error {
	return shared.UploadEnvdFile(ctx, shared.EnvdUploadFileRequest{
		Endpoint:       c.envdURL(session, "/files"),
		TargetPath:     targetPath,
		User:           c.user,
		AccessToken:    session.EnvdAccessToken,
		Content:        r,
		HTTPClient:     c.dataPlaneHTTPClient(),
		SetHeaders:     func(req *http.Request) { c.setEnvdHeaders(req, session) },
		RedirectError:  cubeSandboxRedirectError,
		SummarizeError: core.SummarizeJSON,
		APIError: func(statusCode int, status, body string) error {
			return &cubesandboxAPIError{StatusCode: statusCode, Status: status, Body: body}
		},
	})
}

func (c *cubesandboxClient) StartProcess(ctx context.Context, session shared.EnvdSandboxSession, req shared.EnvdSandboxProcessRequest) (int, error) {
	return shared.StartEnvdProcess(ctx, shared.EnvdProcessRequest{
		EnvdSandboxProcessRequest: req,
		Endpoint:                  c.envdURL(session, "/process.Process/Start"),
		AccessToken:               session.EnvdAccessToken,
		HTTPClient:                c.dataPlaneHTTPClient(),
		SetHeaders:                func(httpReq *http.Request) { c.setEnvdHeaders(httpReq, session) },
		RedirectError:             cubeSandboxRedirectError,
		Provider:                  "cubesandbox",
		InterpretEnd:              interpretCubeSandboxProcessEnd,
		SummarizeError:            core.SummarizeJSON,
		APIError: func(statusCode int, status, body string) error {
			return &cubesandboxAPIError{StatusCode: statusCode, Status: status, Body: body}
		},
	})
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
		return &cubesandboxAPIError{StatusCode: statusCode, Status: status, Body: shared.RedactErrorSecrets(core.SummarizeJSON(data), c.apiKey)}
	}); err != nil {
		return nil, err
	}
	return resp.Header.Clone(), nil
}

func (c *cubesandboxClient) sessionFromSandbox(sandbox shared.EnvdSandbox) shared.EnvdSandboxSession {
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

func (c *cubesandboxClient) envdURL(session shared.EnvdSandboxSession, path string) string {
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

func (c *cubesandboxClient) setEnvdHeaders(req *http.Request, session shared.EnvdSandboxSession) {
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

func (c *cubesandboxClient) control() shared.EnvdSandboxControl {
	return shared.EnvdSandboxControl{Request: c.doJSONWithHeaders}
}
