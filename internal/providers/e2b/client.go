package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
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
}

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
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	apiURL, err := validateE2BAPIURL(blank(cfg.E2B.APIURL, "https://api.e2b.app"))
	if err != nil {
		return nil, err
	}
	domain := strings.TrimSpace(blank(cfg.E2B.Domain, "e2b.app"))
	return &e2bClient{apiKey: apiKey, apiURL: apiURL, domain: domain, user: cfg.E2B.User, httpClient: httpClient}, nil
}

func validateE2BAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=e2b API URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=e2b API URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isE2BLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "provider=e2b API URL must use HTTPS except for loopback development endpoints")
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

func isE2BLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	return c.sessionFromSandbox(sandbox), nil
}

func (c *e2bClient) GetSandbox(ctx context.Context, sandboxID string) (e2bSandbox, error) {
	var sandbox e2bSandbox
	if err := c.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(sandboxID), nil, nil, &sandbox); err != nil {
		return e2bSandbox{}, err
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
	resp, err := c.httpClient.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		_ = pw.CloseWithError(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &e2bAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: summarizeJSON(data)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *e2bClient) StartProcess(ctx context.Context, session e2bSession, req e2bProcessRequest) (int, error) {
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
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 1, &e2bAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: summarizeJSON(data)}
	}
	return parseE2BProcessStream(resp.Body, req.Stdout, req.Stderr)
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
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &e2bAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: summarizeJSON(data)}
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

type e2bStartResponse struct {
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

type e2bEndStream struct {
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

func parseE2BProcessStream(r io.Reader, stdout, stderr io.Writer) (int, error) {
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
			var end e2bEndStream
			if len(data) > 0 {
				if err := json.Unmarshal(data, &end); err != nil {
					return 1, err
				}
			}
			if end.Error != nil {
				return 1, fmt.Errorf("%s: %s", end.Error.Code, end.Error.Message)
			}
			break
		}
		var event e2bStartResponse
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
				fmt.Fprintln(stderr, event.Event.End.Error)
			}
		}
	}
	if !seenEnd {
		return 1, fmt.Errorf("e2b process stream ended without end event")
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
