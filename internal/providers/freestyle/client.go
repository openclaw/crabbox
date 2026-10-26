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
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type freestyleAPI interface {
	// CreateVM preserves an allocated VM on setup failure; the caller owns rollback.
	CreateVM(ctx context.Context, req freestyleCreateVMRequest) (freestyleVM, error)
	GetVM(ctx context.Context, id string) (freestyleVM, error)
	ListVMs(ctx context.Context) ([]freestyleVM, error)
	DeleteVM(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, command string, stdout, stderr io.Writer) (int, error)
	WriteFile(ctx context.Context, id, path, content, encoding string) error
}

type freestyleVM struct {
	ID    string `json:"id"`
	Name  string `json:"displayName"`
	Slug  string `json:"slug"`
	State string `json:"state"`
}

func normalizeFreestyleVM(vm freestyleVM) freestyleVM {
	if vm.Name == "" {
		vm.Name = vm.Slug
	}
	return vm
}

type freestyleListVMsResponse struct {
	VMs        []freestyleVM `json:"vms"`
	TotalCount int           `json:"totalCount"`
}

type freestyleCreateVMRequest struct {
	Name     string
	VCPUs    int
	MemoryGB int
}

type freestyleExecRequest struct {
	Command   string `json:"command"`
	Stdin     string `json:"stdin,omitempty"`
	LinuxUser string `json:"linuxUser"`
}

type freestyleExecResponse struct {
	StatusCode *int    `json:"statusCode"`
	Stdout     *string `json:"stdout"`
	Stderr     *string `json:"stderr"`
}

type freestyleHTTPClient struct {
	apiKey         string
	apiURL         string
	httpClient     *http.Client
	dataHTTPClient *http.Client
}

const freestyleListPageSize = 100
const freestyleControlTimeout = 60 * time.Second

var newFreestyleClient = func(cfg core.Config, rt core.Runtime) (freestyleAPI, error) {
	apiKey := strings.TrimSpace(cfg.Freestyle.APIKey)
	if apiKey == "" {
		return nil, core.Exit(2, "provider=freestyle requires FREESTYLE_API_KEY")
	}
	apiURL, err := validateFreestyleAPIURL(core.Blank(cfg.Freestyle.APIURL, core.FreestyleConfigDefaultAPIURL))
	if err != nil {
		return nil, err
	}
	httpClient, dataHTTPClient := shared.ControlAndDataHTTPClients(rt.HTTP, freestyleControlTimeout)
	trusted, _ := url.Parse(apiURL)
	return &freestyleHTTPClient{
		apiKey:         apiKey,
		apiURL:         apiURL,
		httpClient:     shared.SecureHTTPClient(httpClient, trusted, freestyleRedirectError),
		dataHTTPClient: shared.SecureHTTPClient(dataHTTPClient, trusted, freestyleRedirectError),
	}, nil
}

func validateFreestyleAPIURL(raw string) (string, error) {
	return shared.NormalizeHTTPSURL(raw, shared.EndpointURLErrors{
		Invalid:    core.Exit(2, "provider=freestyle API URL must be an absolute HTTPS URL"),
		Components: core.Exit(2, "provider=freestyle API URL must not contain userinfo, query parameters, or a fragment"),
		Insecure:   core.Exit(2, "provider=freestyle API URL must use HTTPS except for loopback development endpoints"),
	})
}

func freestyleRedirectError(destination *url.URL) error {
	return fmt.Errorf("freestyle refused cross-origin redirect to %s", destination.Redacted())
}

func (c *freestyleHTTPClient) do(ctx context.Context, method, urlPath string, body io.Reader) (*http.Response, error) {
	return c.doWithClient(ctx, c.httpClient, method, urlPath, body)
}

func (c *freestyleHTTPClient) doData(ctx context.Context, method, urlPath string, body io.Reader) (*http.Response, error) {
	httpClient := c.dataHTTPClient
	if httpClient == nil {
		httpClient = c.httpClient
	}
	return c.doWithClient(ctx, httpClient, method, urlPath, body)
}

func (c *freestyleHTTPClient) doWithClient(ctx context.Context, httpClient *http.Client, method, urlPath string, body io.Reader) (*http.Response, error) {
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
	return httpClient.Do(req)
}

func (c *freestyleHTTPClient) CreateVM(ctx context.Context, req freestyleCreateVMRequest) (freestyleVM, error) {
	// v5 creates a default-sized VM; custom sizes are a separate grow-only
	// resize operation. The empty source denotes this new VM, never public ingress.
	payload, err := json.Marshal(struct {
		DisplayName string `json:"displayName"`
		Slug        string `json:"slug"`
		Firewall    any    `json:"firewall"`
	}{req.Name, req.Name, map[string]any{"rules": []any{map[string]any{
		"action": "allow", "source": map[string]any{}, "destination": map[string]any{"public": true},
	}}}})
	if err != nil {
		return freestyleVM{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v5/vms", bytes.NewReader(payload))
	if err != nil {
		return freestyleVM{}, freestyleError("create vm", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return freestyleVM{}, c.responseError("create vm", resp)
	}
	var vm freestyleVM
	if err := json.NewDecoder(resp.Body).Decode(&vm); err != nil {
		return freestyleVM{}, freestyleError("create vm", err)
	}
	if (req.VCPUs > 0 || req.MemoryGB > 0) && vm.ID != "" {
		if err := c.resizeVM(ctx, vm.ID, req.VCPUs, req.MemoryGB); err != nil {
			// Acquisition cleanup belongs to the backend, which also supplies exact
			// adoption and stop commands if deleting the allocated VM fails.
			return normalizeFreestyleVM(vm), err
		}
	}
	return normalizeFreestyleVM(vm), nil
}

func (c *freestyleHTTPClient) resizeVM(ctx context.Context, id string, vcpus, memoryGB int) error {
	payload, err := json.Marshal(struct {
		CPU    int   `json:"cpu,omitempty"`
		Memory int64 `json:"memory,omitempty"`
	}{vcpus, int64(memoryGB) * 1024})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v5/vms/"+url.PathEscape(id)+"/resize", bytes.NewReader(payload))
	if err != nil {
		return freestyleError("resize vm", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return c.responseError("resize vm", resp)
	}
	return nil
}

func (c *freestyleHTTPClient) GetVM(ctx context.Context, id string) (freestyleVM, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v5/vms/"+url.PathEscape(id), nil)
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
	return normalizeFreestyleVM(vm), nil
}

func (c *freestyleHTTPClient) ListVMs(ctx context.Context) ([]freestyleVM, error) {
	var vms []freestyleVM
	for offset := 0; ; offset += freestyleListPageSize {
		urlPath := fmt.Sprintf("/v5/vms?limit=%d&offset=%d", freestyleListPageSize, offset)
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
		for _, vm := range parsed.VMs {
			vms = append(vms, normalizeFreestyleVM(vm))
		}
		if len(parsed.VMs) < freestyleListPageSize || len(vms) >= parsed.TotalCount {
			return vms, nil
		}
	}
}

func (c *freestyleHTTPClient) DeleteVM(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v5/vms/"+url.PathEscape(id), nil)
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
	return c.exec(ctx, id, freestyleExecRequest{Command: command}, stdout, stderr)
}

func (c *freestyleHTTPClient) exec(ctx context.Context, id string, request freestyleExecRequest, stdout, stderr io.Writer) (int, error) {
	request.LinuxUser = "root"
	payload, err := json.Marshal(request)
	if err != nil {
		return 1, err
	}
	resp, err := c.doData(ctx, http.MethodPost, "/v5/vms/"+url.PathEscape(id)+"/exec-await", bytes.NewReader(payload))
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
	// Freestyle omits the exit status (or returns null) when the command times
	// out. Preserve any captured output, but never infer a successful exit.
	if parsed.StatusCode == nil {
		return 1, freestyleError("exec", fmt.Errorf("command timed out: %w", context.DeadlineExceeded))
	}
	return *parsed.StatusCode, nil
}

// This adapter uses exec for file transport alongside its archive transaction.
// Freestyle also exposes native /fs read/write endpoints. Base64-encoded stdin
// preserves file bytes through exec's JSON request.
func (c *freestyleHTTPClient) WriteFile(ctx context.Context, id, path, content, encoding string) error {
	data := []byte(content)
	switch encoding {
	case "base64":
		var err error
		data, err = base64.StdEncoding.DecodeString(content)
		if err != nil {
			return freestyleError("write file", err)
		}
	case "", "utf8", "utf-8":
	default:
		return freestyleError("write file", fmt.Errorf("unsupported encoding %q", encoding))
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// Base64 and JSON must fit the service's 1 MiB HTTP request limit.
	const chunkSize = 512 << 10
	for offset := 0; ; offset += chunkSize {
		end := min(offset+chunkSize, len(data))
		redirect := ">"
		if offset > 0 {
			redirect = ">>"
		}
		var stderr bytes.Buffer
		code, err := c.exec(ctx, id, freestyleExecRequest{
			Command: "cat " + redirect + " " + core.ShellQuote(path),
			Stdin:   base64.StdEncoding.EncodeToString(data[offset:end]),
		}, io.Discard, &stderr)
		if err != nil {
			return freestyleError("write file", err)
		}
		if code != 0 {
			return freestyleError("write file", fmt.Errorf("guest command exited %d: %s", code, redactFreestyleSecret(strings.TrimSpace(stderr.String()), c.apiKey)))
		}
		if end == len(data) {
			return nil
		}
	}
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
