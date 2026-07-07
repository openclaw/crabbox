package cubesandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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
	proxyHost   string
	proxyPort   int
	proxyScheme string
	httpClient  *http.Client
}

const cubesandboxEnvdPort = 49983

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
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	apiURL, err := validateCubeSandboxAPIURL(blank(cfg.CubeSandbox.APIURL, "http://127.0.0.1:3000"))
	if err != nil {
		return nil, err
	}
	domain := strings.TrimSpace(blank(cfg.CubeSandbox.Domain, "cube.app"))
	proxyScheme := normalizeCubeSandboxProxyScheme(cfg.CubeSandbox.ProxyScheme, cfg.CubeSandbox.ProxyPortHTTP)
	proxyPort := cfg.CubeSandbox.ProxyPortHTTP
	if proxyPort <= 0 {
		proxyPort = 80
	}
	return &cubesandboxClient{
		apiKey:      apiKey,
		apiURL:      apiURL,
		domain:      domain,
		user:        cfg.CubeSandbox.User,
		proxyHost:   strings.TrimSpace(cfg.CubeSandbox.ProxyNodeIP),
		proxyPort:   proxyPort,
		proxyScheme: proxyScheme,
		httpClient:  httpClient,
	}, nil
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

func normalizeCubeSandboxProxyScheme(scheme string, port int) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http", "https":
		return strings.ToLower(strings.TrimSpace(scheme))
	default:
		if port == 443 {
			return "https"
		}
		return "http"
	}
}

func secureCubeSandboxHTTPClient(source *http.Client, trusted *url.URL) *http.Client {
	client := *source
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameCubeSandboxOrigin(trusted, req.URL) {
			return fmt.Errorf("cubesandbox refused cross-origin redirect to %s", req.URL.Redacted())
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

func sameCubeSandboxOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveCubeSandboxPort(a) == effectiveCubeSandboxPort(b)
}

func effectiveCubeSandboxPort(value *url.URL) string {
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
	return c.sessionFromSandbox(sandbox), nil
}

func (c *cubesandboxClient) GetSandbox(ctx context.Context, sandboxID string) (cubesandboxSandbox, error) {
	var sandbox cubesandboxSandbox
	if err := c.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(sandboxID), nil, nil, &sandbox); err != nil {
		return cubesandboxSandbox{}, err
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
	endpoint, err := url.Parse(c.envdURL(session, "/files"))
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("path", targetPath)
	if strings.TrimSpace(c.user) != "" {
		query.Set("username", c.user)
	}
	endpoint.RawQuery = query.Encode()
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		_ = pw.CloseWithError(err)
		return err
	}
	c.setEnvdHeaders(req, session)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	go func() {
		part, err := writer.CreateFormFile("file", targetPath)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, r); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := writer.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	resp, err := secureCubeSandboxHTTPClient(c.httpClient, req.URL).Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		_ = pw.CloseWithError(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &cubesandboxAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: shared.RedactErrorSecrets(summarizeJSON(data), session.EnvdAccessToken)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *cubesandboxClient) StartProcess(ctx context.Context, session cubesandboxSession, req cubesandboxProcessRequest) (int, error) {
	if req.Stdout == nil {
		req.Stdout = io.Discard
	}
	if req.Stderr == nil {
		req.Stderr = io.Discard
	}
	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	start := map[string]any{
		"process": map[string]any{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", req.Command},
			"envs": env,
			"cwd":  req.CWD,
		},
		"stdin": false,
	}
	body, err := encodeConnectJSONEnvelope(start)
	if err != nil {
		return 1, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.envdURL(session, "/process.Process/Start"), bytes.NewReader(body))
	if err != nil {
		return 1, err
	}
	c.setEnvdHeaders(httpReq, session)
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("Connect-Content-Encoding", "identity")
	httpReq.Header.Set("Content-Type", "application/connect+json")
	httpReq.Header.Set("Keepalive-Ping-Interval", "50")
	if req.User != "" {
		httpReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(req.User+":")))
	}
	if timeoutMs := durationMillisCeil(req.Timeout); timeoutMs > 0 {
		httpReq.Header.Set("Connect-Timeout-Ms", fmt.Sprint(timeoutMs))
	}
	resp, err := secureCubeSandboxHTTPClient(c.httpClient, httpReq.URL).Do(httpReq)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 1, &cubesandboxAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: shared.RedactErrorSecrets(summarizeJSON(data), session.EnvdAccessToken)}
	}
	return parseCubeSandboxProcessStream(resp.Body, req.Stdout, req.Stderr, session.EnvdAccessToken)
}

func (c *cubesandboxClient) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	_, err := c.doJSONWithHeaders(ctx, method, path, query, body, out)
	return err
}

func (c *cubesandboxClient) doJSONWithHeaders(ctx context.Context, method, path string, query url.Values, body any, out any) (http.Header, error) {
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
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := secureCubeSandboxHTTPClient(c.httpClient, req.URL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &cubesandboxAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: shared.RedactErrorSecrets(summarizeJSON(data), c.apiKey)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, err
		}
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
	endpoint, _ := c.envdEndpoint(session, path)
	return endpoint
}

func (c *cubesandboxClient) envdEndpoint(session cubesandboxSession, path string) (string, string) {
	domain := strings.TrimSpace(session.Domain)
	if domain == "" {
		domain = c.domain
	}
	scheme := normalizeCubeSandboxProxyScheme(c.proxyScheme, c.proxyPort)
	virtualHost := fmt.Sprintf("%d-%s.%s", cubesandboxEnvdPort, session.SandboxID, domain)
	if c.proxyHost == "" {
		return scheme + "://" + virtualHost + path, ""
	}
	host := c.proxyHost
	if c.proxyPort > 0 {
		host = net.JoinHostPort(c.proxyHost, strconv.Itoa(c.proxyPort))
	}
	return scheme + "://" + host + path, virtualHost
}

func (c *cubesandboxClient) setEnvdHeaders(req *http.Request, session cubesandboxSession) {
	if _, host := c.envdEndpoint(session, req.URL.Path); host != "" {
		req.Host = host
	}
	req.Header.Set("X-Access-Token", session.EnvdAccessToken)
	req.Header.Set("E2b-Sandbox-Id", session.SandboxID)
	req.Header.Set("E2b-Sandbox-Port", strconv.Itoa(cubesandboxEnvdPort))
}

type cubesandboxStartResponse struct {
	Event struct {
		Start *struct {
			PID uint32 `json:"pid"`
		} `json:"start,omitempty"`
		Data *struct {
			Stdout string `json:"stdout,omitempty"`
			Stderr string `json:"stderr,omitempty"`
			PTY    string `json:"pty,omitempty"`
		} `json:"data,omitempty"`
		End *struct {
			ExitCode int    `json:"exitCode"`
			Exited   bool   `json:"exited"`
			Status   string `json:"status"`
			Error    string `json:"error,omitempty"`
		} `json:"end,omitempty"`
		Keepalive map[string]any `json:"keepalive,omitempty"`
	} `json:"event"`
}

type cubesandboxEndStream struct {
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func encodeConnectJSONEnvelope(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteByte(0)
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(data)))
	out.Write(size[:])
	out.Write(data)
	return out.Bytes(), nil
}

func parseCubeSandboxProcessStream(r io.Reader, stdout, stderr io.Writer, secrets ...string) (int, error) {
	exitCode := 0
	seenEnd := false
	for {
		var header [5]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF {
				break
			}
			return 1, err
		}
		flags := header[0]
		size := binary.BigEndian.Uint32(header[1:])
		if flags&1 != 0 {
			return 1, fmt.Errorf("compressed connect envelopes are not supported")
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return 1, err
		}
		if flags&2 != 0 {
			var end cubesandboxEndStream
			if len(data) > 0 {
				if err := json.Unmarshal(data, &end); err != nil {
					return 1, err
				}
			}
			if end.Error != nil {
				return 1, errors.New(shared.RedactErrorSecrets(end.Error.Code+": "+end.Error.Message, secrets...))
			}
			break
		}
		var event cubesandboxStartResponse
		if err := json.Unmarshal(data, &event); err != nil {
			return 1, err
		}
		if event.Event.Data != nil {
			if err := writeBase64(event.Event.Data.Stdout, stdout); err != nil {
				return 1, err
			}
			if err := writeBase64(event.Event.Data.Stderr, stderr); err != nil {
				return 1, err
			}
		}
		if event.Event.End != nil {
			exitCode = event.Event.End.ExitCode
			seenEnd = true
			if !event.Event.End.Exited && event.Event.End.Error != "" {
				fmt.Fprintln(stderr, shared.RedactErrorSecrets(event.Event.End.Error, secrets...))
			}
		}
	}
	if !seenEnd {
		return 1, fmt.Errorf("cubesandbox process stream ended without end event")
	}
	return exitCode, nil
}

func writeBase64(value string, w io.Writer) error {
	if value == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func durationMillisCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Millisecond - 1) / time.Millisecond)
}
