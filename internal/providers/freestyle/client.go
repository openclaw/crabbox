package freestyle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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
	Name      string `json:"name"`
	VcpuCount int    `json:"vcpuCount,omitempty"`
	MemSizeGb int    `json:"memSizeGb,omitempty"`
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
	apiURL := strings.TrimRight(blank(cfg.Freestyle.APIURL, "https://api.freestyle.sh"), "/")
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &freestyleHTTPClient{
		apiKey:     apiKey,
		apiURL:     apiURL,
		httpClient: httpClient,
	}, nil
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
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return freestyleVM{}, freestyleError("create vm", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet))))
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
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return freestyleVM{}, freestyleError("get vm", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet))))
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
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, freestyleError("list vms", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet))))
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
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return freestyleError("delete vm", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet))))
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
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 1, freestyleError("exec", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet))))
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
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return freestyleError("write file", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet))))
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
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", freestyleError("read file", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet))))
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

func freestyleError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("freestyle %s: %w", action, err)
}
