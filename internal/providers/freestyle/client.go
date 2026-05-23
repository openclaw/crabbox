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
	GetVM(ctx context.Context, vmID string) (freestyleVM, error)
	ListVMs(ctx context.Context) ([]freestyleVM, error)
	DeleteVM(ctx context.Context, vmID string) error
	Exec(ctx context.Context, vmID string, command string, stdout, stderr io.Writer) (int, error)
	WriteFile(ctx context.Context, vmID, path string, content []byte) error
	ReadFile(ctx context.Context, vmID, path string) ([]byte, error)
}

type freestyleVM struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	State string `json:"state,omitempty"`
}

type freestyleListVMsResponse struct {
	VMs        []freestyleVM `json:"vms"`
	TotalCount int           `json:"totalCount"`
}

type freestyleCreateVMResponse struct {
	ID string `json:"id"`
}

type freestyleCreateVMRequest struct {
	Name               string `json:"name,omitempty"`
	Workdir            string `json:"workdir,omitempty"`
	VcpuCount          int    `json:"vcpuCount,omitempty"`
	MemSizeMb          int    `json:"memSizeMb,omitempty"`
	IdleTimeoutSeconds *int   `json:"idleTimeoutSeconds,omitempty"`
}

type freestyleExecResponse struct {
	StatusCode int     `json:"statusCode"`
	Stdout     *string `json:"stdout"`
	Stderr     *string `json:"stderr"`
}

type freestyleExecRequest struct {
	Command string `json:"command"`
}

type freestyleWriteFileRequest struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type freestyleReadFileResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty"`
}

type freestyleHTTPClient struct {
	apiKey     string
	apiURL     string
	httpClient *http.Client
}

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
	return &freestyleHTTPClient{apiKey: apiKey, apiURL: apiURL, httpClient: httpClient}, nil
}

func (c *freestyleHTTPClient) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	u, err := url.Parse(c.apiURL + path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return c.httpClient.Do(req)
}

func (c *freestyleHTTPClient) CreateVM(ctx context.Context, req freestyleCreateVMRequest) (freestyleVM, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return freestyleVM{}, fmt.Errorf("freestyle encode create vm: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/vms", bytes.NewReader(body))
	if err != nil {
		return freestyleVM{}, freestyleError("create vm", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return freestyleVM{}, fmt.Errorf("freestyle create vm %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var createResp freestyleCreateVMResponse
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return freestyleVM{}, fmt.Errorf("freestyle decode create vm: %w", err)
	}
	return freestyleVM{ID: createResp.ID, State: "running"}, nil
}

func (c *freestyleHTTPClient) GetVM(ctx context.Context, vmID string) (freestyleVM, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(vmID), nil)
	if err != nil {
		return freestyleVM{}, freestyleError("get vm", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return freestyleVM{}, fmt.Errorf("freestyle get vm %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var vm freestyleVM
	if err := json.NewDecoder(resp.Body).Decode(&vm); err != nil {
		return freestyleVM{}, fmt.Errorf("freestyle decode get vm: %w", err)
	}
	return vm, nil
}

func (c *freestyleHTTPClient) ListVMs(ctx context.Context) ([]freestyleVM, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/vms", nil)
	if err != nil {
		return nil, freestyleError("list vms", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("freestyle list vms %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var listResp freestyleListVMsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("freestyle decode list vms: %w", err)
	}
	return listResp.VMs, nil
}

func (c *freestyleHTTPClient) DeleteVM(ctx context.Context, vmID string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/vms/"+url.PathEscape(vmID), nil)
	if err != nil {
		return freestyleError("delete vm", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("freestyle delete vm %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (c *freestyleHTTPClient) Exec(ctx context.Context, vmID string, command string, stdout, stderr io.Writer) (int, error) {
	reqBody := freestyleExecRequest{Command: command}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return 1, fmt.Errorf("freestyle encode exec: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(vmID)+"/exec-await", bytes.NewReader(body))
	if err != nil {
		return 1, freestyleError("exec", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 1, fmt.Errorf("freestyle exec %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var execResp freestyleExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&execResp); err != nil {
		return 1, fmt.Errorf("freestyle decode exec: %w", err)
	}
	if execResp.Stdout != nil {
		_, _ = stdout.Write([]byte(*execResp.Stdout))
	}
	if execResp.Stderr != nil {
		_, _ = stderr.Write([]byte(*execResp.Stderr))
	}
	return execResp.StatusCode, nil
}

func (c *freestyleHTTPClient) WriteFile(ctx context.Context, vmID, path string, content []byte) error {
	reqBody := freestyleWriteFileRequest{
		Content:  base64.StdEncoding.EncodeToString(content),
		Encoding: "base64",
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("freestyle encode write file: %w", err)
	}
	escapedPath := "/" + strings.TrimLeft(path, "/")
	resp, err := c.do(ctx, http.MethodPut, "/v1/vms/"+url.PathEscape(vmID)+"/files/"+url.PathEscape(escapedPath), bytes.NewReader(body))
	if err != nil {
		return freestyleError("write file", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("freestyle write file %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (c *freestyleHTTPClient) ReadFile(ctx context.Context, vmID, path string) ([]byte, error) {
	escapedPath := "/" + strings.TrimLeft(path, "/")
	resp, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(vmID)+"/files/"+url.PathEscape(escapedPath), nil)
	if err != nil {
		return nil, freestyleError("read file", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("freestyle read file %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var fileResp freestyleReadFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil {
		return nil, fmt.Errorf("freestyle decode read file: %w", err)
	}
	if fileResp.Encoding != "" && fileResp.Encoding != "base64" {
		return []byte(fileResp.Content), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(fileResp.Content)
	if err != nil {
		return []byte(fileResp.Content), nil
	}
	return decoded, nil
}
