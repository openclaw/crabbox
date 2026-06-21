package freestyle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type freestyleAPI interface {
	CreateVM(ctx context.Context, req freestyleCreateVMRequest) (freestyleVM, error)
	GetVM(ctx context.Context, id string) (freestyleVM, error)
	ListVMs(ctx context.Context) ([]freestyleVM, error)
	DeleteVM(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, command string, stdout, stderr io.Writer) (int, error)
	WriteFile(ctx context.Context, id, path, content, encoding string) error
	ReadFile(ctx context.Context, id, path string) (string, error)
}

type freestyleVM struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type freestyleListVMsResponse struct {
	VMs        []freestyleVM `json:"vms"`
	TotalCount int           `json:"totalCount"`
}

type freestyleCreateVMResponse struct {
	ID string `json:"id"`
}

type freestyleCreateVMRequest struct {
	Name     string                     `json:"name"`
	Ports    []freestylePortMapping     `json:"ports"`
	Template *freestyleCreateVMTemplate `json:"template,omitempty"`
}

type freestyleCreateVMTemplate struct {
	VcpuCount int `json:"vcpuCount,omitempty"`
	MemSizeGb int `json:"memSizeGb,omitempty"`
}

type freestylePortMapping struct {
	Port       int `json:"port"`
	TargetPort int `json:"targetPort"`
}

type freestyleExecRequest struct {
	Command string `json:"command"`
}

type freestyleExecResponse struct {
	StatusCode int     `json:"statusCode"`
	Stdout     *string `json:"stdout"`
	Stderr     *string `json:"stderr"`
}

type freestyleWriteFileRequest struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type freestyleReadFileResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type freestyleHTTPClient struct {
	apiKey     string
	apiURL     string
	httpClient *http.Client
}

const freestyleListPageSize = 100

var newFreestyleClient = func(cfg Config, rt Runtime) (freestyleAPI, error) {
	apiKey := strings.TrimSpace(cfg.Freestyle.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=freestyle requires FREESTYLE_API_KEY")
	}
	apiURL, err := validateFreestyleAPIURL(blank(cfg.Freestyle.APIURL, "https://api.freestyle.sh"))
	if err != nil {
		return nil, err
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &freestyleHTTPClient{
		apiKey:     apiKey,
		apiURL:     apiURL,
		httpClient: secureFreestyleHTTPClient(httpClient, apiURL),
	}, nil
}

func validateFreestyleAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=freestyle API URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=freestyle API URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isFreestyleLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "provider=freestyle API URL must use HTTPS except for loopback development endpoints")
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

func isFreestyleLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func secureFreestyleHTTPClient(source *http.Client, apiURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(apiURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameFreestyleOrigin(trusted, req.URL) {
			return fmt.Errorf("freestyle refused cross-origin redirect to %s", req.URL.Redacted())
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

func sameFreestyleOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveFreestylePort(a) == effectiveFreestylePort(b)
}

func effectiveFreestylePort(value *url.URL) string {
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

func (c *freestyleHTTPClient) do(ctx context.Context, method, urlPath string, body io.Reader) (*http.Response, error) {
	u, err := url.Parse(c.apiURL + urlPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.httpClient.Do(req)
}

func (c *freestyleHTTPClient) CreateVM(ctx context.Context, req freestyleCreateVMRequest) (freestyleVM, error) {
	if req.Ports == nil {
		req.Ports = []freestylePortMapping{}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return freestyleVM{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/vms", bytes.NewReader(payload))
	if err != nil {
		return freestyleVM{}, freestyleError("create vm", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return freestyleVM{}, c.responseError("create vm", resp)
	}
	var parsed freestyleCreateVMResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return freestyleVM{}, freestyleError("create vm", err)
	}
	return freestyleVM{ID: parsed.ID, State: "running"}, nil
}

func (c *freestyleHTTPClient) GetVM(ctx context.Context, id string) (freestyleVM, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id), nil)
	if err != nil {
		return freestyleVM{}, freestyleError("get vm", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return freestyleVM{}, c.responseError("get vm", resp)
	}
	var vm freestyleVM
	if err := json.NewDecoder(resp.Body).Decode(&vm); err != nil {
		return freestyleVM{}, freestyleError("get vm", err)
	}
	return vm, nil
}

func (c *freestyleHTTPClient) ListVMs(ctx context.Context) ([]freestyleVM, error) {
	var vms []freestyleVM
	for offset := 0; ; offset += freestyleListPageSize {
		urlPath := fmt.Sprintf("/v1/vms?limit=%d&offset=%d", freestyleListPageSize, offset)
		resp, err := c.do(ctx, http.MethodGet, urlPath, nil)
		if err != nil {
			return nil, freestyleError("list vms", err)
		}
		if resp.StatusCode >= 400 {
			responseErr := c.responseError("list vms", resp)
			_ = resp.Body.Close()
			return nil, responseErr
		}
		var parsed freestyleListVMsResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&parsed)
		closeErr := resp.Body.Close()
		if decodeErr != nil {
			return nil, freestyleError("list vms", decodeErr)
		}
		if closeErr != nil {
			return nil, freestyleError("list vms", closeErr)
		}
		vms = append(vms, parsed.VMs...)
		if len(parsed.VMs) < freestyleListPageSize || len(vms) >= parsed.TotalCount {
			return vms, nil
		}
	}
}

func (c *freestyleHTTPClient) DeleteVM(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/vms/"+url.PathEscape(id), nil)
	if err != nil {
		return freestyleError("delete vm", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 400 {
		return c.responseError("delete vm", resp)
	}
	return nil
}

func (c *freestyleHTTPClient) Exec(ctx context.Context, id string, command string, stdout, stderr io.Writer) (int, error) {
	payload, err := json.Marshal(freestyleExecRequest{Command: command})
	if err != nil {
		return 1, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/exec-await", bytes.NewReader(payload))
	if err != nil {
		return 1, freestyleError("exec", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 1, c.responseError("exec", resp)
	}
	var parsed freestyleExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 1, freestyleError("exec", err)
	}
	if parsed.Stdout != nil {
		_, _ = stdout.Write([]byte(*parsed.Stdout))
	}
	if parsed.Stderr != nil {
		_, _ = stderr.Write([]byte(*parsed.Stderr))
	}
	return parsed.StatusCode, nil
}

func (c *freestyleHTTPClient) WriteFile(ctx context.Context, id, path string, content, encoding string) error {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	payload, err := json.Marshal(freestyleWriteFileRequest{Content: content, Encoding: encoding})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, freestyleFileURLPath(id, path), bytes.NewReader(payload))
	if err != nil {
		return freestyleError("write file", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return c.responseError("write file", resp)
	}
	return nil
}

func (c *freestyleHTTPClient) ReadFile(ctx context.Context, id, path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	resp, err := c.do(ctx, http.MethodGet, freestyleFileURLPath(id, path), nil)
	if err != nil {
		return "", freestyleError("read file", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", c.responseError("read file", resp)
	}
	var parsed freestyleReadFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", freestyleError("read file", err)
	}
	if parsed.Encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(parsed.Content)
		if err != nil {
			return "", freestyleError("read file", err)
		}
		return string(decoded), nil
	}
	return parsed.Content, nil
}

func freestyleFileURLPath(id, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "/v1/vms/" + url.PathEscape(id) + "/files/" + url.PathEscape(path)
}

func (c *freestyleHTTPClient) responseError(action string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	detail := redactFreestyleSecret(strings.TrimSpace(string(snippet)), c.apiKey)
	return freestyleError(action, fmt.Errorf("%s: %s", resp.Status, detail))
}

func redactFreestyleSecret(value, apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return value
	}
	return strings.NewReplacer(
		"Bearer "+apiKey, "Bearer [redacted]",
		apiKey, "[redacted]",
	).Replace(value)
}

func freestyleError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("freestyle %s: %w", action, err)
}
