package smolvm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type api interface {
	CreateMachine(context.Context, createRequest) (machineData, error)
	GetMachine(context.Context, string) (machineData, error)
	StartMachine(context.Context, string) error
	StopMachine(context.Context, string) error
	DeleteMachine(context.Context, string) error
	ListMachines(context.Context) ([]machineData, error)
	Exec(context.Context, string, string, string) (execResult, error)
	ExecStream(context.Context, string, string, string, io.Writer) (int, error)
	InjectArchive(context.Context, string, string, string) error
	WriteFile(context.Context, string, string, string) error
}

type client struct {
	apiKey string
	base   string
	http   *http.Client
}

type createRequest struct {
	Name       string                 `json:"name,omitempty"`
	Source     smolvmMachineSource    `json:"source"`
	Resources  smolvmMachineResources `json:"resources,omitempty"`
	Network    *smolvmMachineNetwork  `json:"network,omitempty"`
	Workdir    string                 `json:"workdir,omitempty"`
	Ephemeral  bool                   `json:"ephemeral,omitempty"`
	TTLSeconds int                    `json:"ttlSeconds,omitempty"`
}

type machineData struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	State     string                 `json:"state"`
	Source    smolvmMachineSource    `json:"source"`
	Resources smolvmMachineResources `json:"resources"`
	Network   *smolvmMachineNetwork  `json:"network,omitempty"`
	Workdir   string                 `json:"workdir,omitempty"`
	CreatedAt string                 `json:"createdAt,omitempty"`
	UpdatedAt string                 `json:"updatedAt,omitempty"`
}

type execResult struct {
	// ExitCode is the canonical (camelCase) field the hosted smolfleet API
	// returns. exitCodeSnake captures the snake_case spelling used by the
	// local `smolvm serve` variant; Exec reconciles the two so a non-zero
	// status is never silently dropped. (A single field can't carry two json
	// tags — the second is ignored by encoding/json.)
	ExitCode      int    `json:"exitCode"`
	ExitCodeSnake int    `json:"exit_code"`
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	// stdout/stderr fallbacks for variants that use output/error keys.
	Output string `json:"output"`
	Error  string `json:"error"`
}

// smolvmAPIError is the structured error returned for non-2xx responses from
// the smolfleet control plane. It is modeled after runpodAPIError / wandbAPIError
// so that callers get consistent "API %s: body" messages and the body snippet
// for debugging (exactly like islo's raw http error paths and runpod).
type smolvmAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *smolvmAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

// Typed request/response shapes for the smolfleet /v1/machines API,
// hand-written from the published OpenAPI document.
type smolvmMachineSource struct {
	Type      string `json:"type"`
	Reference string `json:"reference"`
}

type smolvmMachineResources struct {
	CPUs     int `json:"cpus,omitempty"`
	MemoryMB int `json:"memoryMb,omitempty"`
	DiskGB   int `json:"diskGb,omitempty"`
}

type smolvmMachineNetwork struct {
	Mode  string   `json:"mode"`
	CIDRs []string `json:"cidrs,omitempty"`
}

type smolvmCreateMachineRequest struct {
	Name            string                 `json:"name,omitempty"`
	Source          smolvmMachineSource    `json:"source"`
	Resources       smolvmMachineResources `json:"resources,omitempty"`
	Network         *smolvmMachineNetwork  `json:"network,omitempty"`
	Workdir         string                 `json:"workdir,omitempty"`
	Env             map[string]string      `json:"env,omitempty"`
	Ephemeral       bool                   `json:"ephemeral,omitempty"`
	TTLSeconds      int                    `json:"ttlSeconds,omitempty"`
	AutoStopSeconds *int                   `json:"autoStopSeconds,omitempty"`
}

type smolvmExecCommandRequest struct {
	Command        any               `json:"command"` // CommandSpec accepts array or string (we pass the buildCommand script string for shell forms)
	CWD            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	Stream         bool              `json:"stream,omitempty"`
	TimeoutSeconds *int              `json:"timeoutSeconds,omitempty"`
}

var newAPI = func(cfg Config, rt Runtime) (api, error) {
	apiKey := strings.TrimSpace(cfg.Smolvm.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=%s requires CRABBOX_SMOLVM_API_KEY, SMOLMACHINES_API_KEY, or SMK_API_KEY", providerName)
	}

	// There is no official Go SDK for the hosted smolfleet control plane
	// (the published smolvm Go module and SDKs target the local `smolvm serve`
	// server), so this client talks net/http directly to the documented
	// OpenAPI, like other direct-API providers. If an official Go client
	// appears, the transport can be swapped behind the api interface.
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	base := blank(strings.TrimSpace(cfg.Smolvm.BaseURL), "https://api.smolmachines.com")
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, exit(2, "%s url %q is invalid", providerName, base)
	}
	if parsed.User != nil {
		return nil, exit(2, "%s url must not include userinfo", providerName)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, exit(2, "%s url %q is invalid", providerName, base)
	}
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return nil, exit(2, "%s url %q must use https unless it targets localhost", providerName, base)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, exit(2, "%s url %q must not include query or fragment components", providerName, base)
	}
	if !trustedSmolvmAPIHost(parsed) && !customSmolvmBaseURLAllowed() {
		return nil, exit(2, "%s url host %q is not an official Smol Machines endpoint; set CRABBOX_SMOLVM_ALLOW_CUSTOM_BASE_URL=1 to send credentials to a custom control plane", providerName, parsed.Hostname())
	}
	base = strings.TrimRight(parsed.String(), "/")
	return &client{apiKey: apiKey, base: base, http: secureSmolvmHTTPClient(httpClient, base)}, nil
}

func secureSmolvmHTTPClient(source *http.Client, apiURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(apiURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameSmolvmOrigin(trusted, req.URL) {
			return fmt.Errorf("%s refused cross-origin redirect to %s", providerName, req.URL.Redacted())
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

func sameSmolvmOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveSmolvmPort(a) == effectiveSmolvmPort(b)
}

func effectiveSmolvmPort(value *url.URL) string {
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

func trustedSmolvmAPIHost(parsed *url.URL) bool {
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		host == "smolmachines.com" || strings.HasSuffix(host, ".smolmachines.com")
}

func customSmolvmBaseURLAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CRABBOX_SMOLVM_ALLOW_CUSTOM_BASE_URL"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *client) CreateMachine(ctx context.Context, req createRequest) (machineData, error) {
	apiReq := smolvmCreateMachineRequest{
		Name:       req.Name,
		Source:     req.Source,
		Resources:  req.Resources,
		Network:    req.Network,
		Workdir:    req.Workdir,
		Ephemeral:  req.Ephemeral,
		TTLSeconds: req.TTLSeconds,
	}
	var m machineData
	if err := c.doJSON(ctx, http.MethodPost, "/v1/machines", nil, apiReq, &m); err != nil {
		return machineData{}, err
	}
	// Return immediately after create (state is typically "stopped"). Caller is
	// responsible for StartMachine + waiting for ready as needed for exec/sync.
	return m, nil
}

func createStatusReady(status string) bool {
	switch status {
	case "idle", "running", "ready", "started", "active", "paused":
		return true
	default:
		return false
	}
}

func (c *client) GetMachine(ctx context.Context, id string) (machineData, error) {
	var m machineData
	if err := c.doJSON(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id), nil, nil, &m); err != nil {
		return machineData{}, err
	}
	return m, nil
}

func (c *client) ListMachines(ctx context.Context) ([]machineData, error) {
	var machines []machineData
	if err := c.doJSON(ctx, http.MethodGet, "/v1/machines", nil, nil, &machines); err != nil {
		return nil, err
	}
	return machines, nil
}

func (c *client) DeleteMachine(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/machines/"+url.PathEscape(id), nil, nil, nil)
}

func (c *client) StartMachine(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/start", nil, map[string]any{}, nil)
}

func (c *client) StopMachine(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/stop", nil, map[string]any{}, nil)
}

func (c *client) Exec(ctx context.Context, machineID, command, folder string) (execResult, error) {
	var result execResult
	execReq := smolvmExecCommandRequest{
		Command: command, // string script or argv slice; the API CommandSpec accepts either
	}
	if strings.TrimSpace(folder) != "" {
		f := strings.TrimSpace(folder)
		if !strings.HasPrefix(f, "/") {
			f = "/" + strings.Trim(f, "/")
		}
		if f == "/" || f == "" {
			f = "/workspace"
		}
		execReq.CWD = f
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/exec", nil, execReq, &result); err != nil {
		return execResult{}, err
	}
	// Hosted API uses exitCode; the local smolvm serve variant uses exit_code.
	// Prefer the canonical field, fall back to snake_case so a failing command
	// is never reported as a success.
	if result.ExitCode == 0 && result.ExitCodeSnake != 0 {
		result.ExitCode = result.ExitCodeSnake
	}
	if result.Output == "" {
		result.Output = result.Stdout
	}
	if result.Error == "" {
		result.Error = result.Stderr
	}
	return result, nil
}

func (c *client) ExecStream(ctx context.Context, machineID, command, folder string, stdout io.Writer) (int, error) {
	// No native stream on this API; fall back to Exec and dump output at end.
	result, err := c.Exec(ctx, machineID, command, folder)
	if err != nil {
		return 0, err
	}
	out := result.Stdout
	if out == "" {
		out = result.Output
	}
	if stdout != nil && out != "" {
		_, _ = io.WriteString(stdout, out)
	}
	if result.Stderr != "" && stdout != nil {
		_, _ = io.WriteString(stdout, result.Stderr)
	}
	return result.ExitCode, nil
}

func (c *client) InjectArchive(ctx context.Context, machineID, localPath, targetDir string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString(data)

	// Direct API call: embed the base64 of the tgz in a heredoc inside the exec
	// command string sent to /exec. The guest only needs sh + base64 + tar (no
	// Python, no "installing" anything). This is a pure direct call to the
	// smolfleet exec API with the payload in the request body.
	// We use a distinctive delimiter that cannot appear in base64 output.
	const eof = "CRABBOX_SYNC_B64_EOF_9f8e7d6c5b4a"
	absTarget := targetDir
	if !strings.HasPrefix(absTarget, "/") {
		absTarget = "/workspace"
	}
	script := fmt.Sprintf(`cat > /tmp/crabbox-sync.tgz << '%s'
%s
%s
base64 -d /tmp/crabbox-sync.tgz | tar -xzf - -C %s && rm -f /tmp/crabbox-sync.tgz
echo "smolvm-direct-archive-extract: ok"
`, eof, b64, eof, shellQuote(absTarget))

	res, err := c.Exec(ctx, machineID, script, "")
	if err != nil {
		return fmt.Errorf("smolvm direct archive exec: %w", err)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Error)
		if msg == "" {
			msg = strings.TrimSpace(res.Output)
		}
		return exit(res.ExitCode, "smolvm direct archive extract: %s", msg)
	}
	return nil
}

func (c *client) WriteFile(ctx context.Context, machineID, remotePath, content string) error {
	// Direct API: write small text (e.g. env profile) via heredoc in a single
	// /exec call. Pure shell, no Python or extra interpreters required.
	absPath := remotePath
	if !strings.HasPrefix(absPath, "/") {
		absPath = "/workspace/" + strings.TrimLeft(absPath, "/")
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	const eof = "CRABBOX_WRITE_B64_EOF_4e2d8a7c9b1f"
	tmpPath := "/tmp/crabbox-write-" + base64.RawURLEncoding.EncodeToString([]byte(filepath.Base(absPath))) + ".b64"
	script := fmt.Sprintf(`mkdir -p %s && cat > %s << '%s'
%s
%s
base64 -d %s > %s && rm -f %s
echo "smolvm-direct-write: ok"
`, shellQuote(filepath.Dir(absPath)), shellQuote(tmpPath), eof, b64, eof, shellQuote(tmpPath), shellQuote(absPath), shellQuote(tmpPath))

	res, err := c.Exec(ctx, machineID, script, "")
	if err != nil {
		return fmt.Errorf("smolvm direct write exec: %w", err)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Error)
		if msg == "" {
			msg = strings.TrimSpace(res.Output)
		}
		return exit(res.ExitCode, "smolvm direct write: %s", msg)
	}
	return nil
}

func (c *client) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, input)
	if err != nil {
		return err
	}
	c.addHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode smolvm response: %w", err)
	}
	return nil
}

func (c *client) addHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
}

func apiError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	msg := strings.TrimSpace(string(data))
	if msg == "" {
		msg = resp.Status
	}
	return &smolvmAPIError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       msg,
	}
}

func commandExitError(prefix string, result execResult) error {
	msg := strings.TrimSpace(result.Error)
	if msg == "" {
		msg = strings.TrimSpace(result.Output)
	}
	if msg == "" {
		msg = "exit " + strconv.Itoa(result.ExitCode)
	}
	return exit(result.ExitCode, "%s: %s", prefix, msg)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") || strings.Contains(msg, "not found")
}
