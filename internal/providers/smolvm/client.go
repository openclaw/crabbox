package smolvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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
	apiKey   string
	base     string
	http     *http.Client
	dataHTTP *http.Client
}

const smolvmControlTimeout = 60 * time.Second

const uploadCleanupTimeout = 30 * time.Second

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
	// The hosted API caps stdout/stderr at 1 MiB and flags the cut; the
	// base64 copies carry every byte.
	StdoutB64       []byte `json:"stdoutB64"`
	StderrB64       []byte `json:"stderrB64"`
	StdoutTruncated bool   `json:"stdoutTruncated"`
	StderrTruncated bool   `json:"stderrTruncated"`
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

var newAPI = func(cfg core.Config, rt core.Runtime) (api, error) {
	apiKey := strings.TrimSpace(cfg.Smolvm.APIKey)
	if apiKey == "" {
		return nil, core.Exit(2, "provider=%s requires CRABBOX_SMOLVM_API_KEY, SMOLMACHINES_API_KEY, or SMK_API_KEY", providerName)
	}

	// There is no official Go SDK for the hosted smolfleet control plane
	// (the published smolvm Go module and SDKs target the local `smolvm serve`
	// server), so this client talks net/http directly to the documented
	// OpenAPI, like other direct-API providers. If an official Go client
	// appears, the transport can be swapped behind the api interface.
	httpClient, dataHTTPClient := shared.ControlAndDataHTTPClients(rt.HTTP, smolvmControlTimeout)
	base, err := smolvmEndpoint(cfg)
	if err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(base)
	return &client{
		apiKey: apiKey, base: base,
		http:     shared.SecureHTTPClient(httpClient, parsed, smolvmRedirectError),
		dataHTTP: shared.SecureHTTPClient(dataHTTPClient, parsed, smolvmRedirectError),
	}, nil
}

func smolvmEndpoint(cfg core.Config) (string, error) {
	base := core.Blank(strings.TrimSpace(cfg.Smolvm.BaseURL), core.SmolvmConfigDefaultBaseURL)
	parsed, err := url.Parse(base)
	if err != nil {
		return "", core.Exit(2, "%s url %q is invalid", providerName, base)
	}
	if parsed.User != nil {
		return "", core.Exit(2, "%s url must not include userinfo", providerName)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", core.Exit(2, "%s url %q is invalid", providerName, base)
	}
	if parsed.Scheme != "https" && !shared.IsLoopbackHTTPURL(parsed) {
		return "", core.Exit(2, "%s url %q must use https unless it targets localhost", providerName, base)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", core.Exit(2, "%s url %q must not include query or fragment components", providerName, base)
	}
	if !trustedSmolvmAPIHost(parsed) && !customSmolvmBaseURLAllowed() {
		return "", core.Exit(2, "%s url host %q is not an official Smol Machines endpoint; set CRABBOX_SMOLVM_ALLOW_CUSTOM_BASE_URL=1 to send credentials to a custom control plane", providerName, parsed.Hostname())
	}
	base = strings.TrimRight(parsed.String(), "/")
	return base, nil
}

func smolvmRedirectError(destination *url.URL) error {
	return fmt.Errorf("%s refused cross-origin redirect to %s", providerName, destination.Redacted())
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
	if err := c.doDataJSON(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/exec", nil, execReq, &result); err != nil {
		return execResult{}, err
	}
	// Hosted API uses exitCode; the local smolvm serve variant uses exit_code.
	// Prefer the canonical field, fall back to snake_case so a failing command
	// is never reported as a success.
	if result.ExitCode == 0 && result.ExitCodeSnake != 0 {
		result.ExitCode = result.ExitCodeSnake
	}
	result.Stdout = exactOutput(result.Stdout, result.StdoutB64, result.StdoutTruncated, "stdout")
	result.Stderr = exactOutput(result.Stderr, result.StderrB64, result.StderrTruncated, "stderr")
	if result.Output == "" {
		result.Output = result.Stdout
	}
	if result.Error == "" {
		result.Error = result.Stderr
	}
	return result, nil
}

// exactOutput prefers the byte-exact copy and marks capped text it cannot replace.
func exactOutput(text string, exact []byte, truncated bool, stream string) string {
	if exact != nil {
		return string(exact)
	}
	if truncated {
		return text + "\ncrabbox: the smolvm API truncated " + stream + " and sent no complete copy; rerun with reduced verbosity or save output to a guest file\n"
	}
	return text
}

func (c *client) ExecStream(ctx context.Context, machineID, command, folder string, stdout io.Writer) (int, error) {
	// The hosted API can stream exec output as Server-Sent Events; this
	// provider still collects it with Exec and writes it at the end.
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
	archive, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return err
	}
	absTarget := targetDir
	if !strings.HasPrefix(absTarget, "/") {
		absTarget = "/workspace"
	}
	return c.withUploadedFile(ctx, machineID, archive, info.Size(), "", `"${TMPDIR:-/tmp}"`,
		"tar -xzf \"$upload_dir/payload\" -C "+core.ShellQuote(absTarget), "archive extract")
}

func (c *client) WriteFile(ctx context.Context, machineID, remotePath, content string) error {
	absPath := remotePath
	if !strings.HasPrefix(absPath, "/") {
		absPath = "/workspace/" + strings.TrimLeft(absPath, "/")
	}
	parent := core.ShellQuote(path.Dir(absPath))
	// Stage beside the destination so publication stays on one filesystem.
	prepare := "mkdir -p " + parent + "\n[ ! -d " + core.ShellQuote(absPath) + " ] || { echo 'file destination is a directory' >&2; exit 1; }\n"
	// The copy makes the published file private and owned by the exec user,
	// whichever owner and mode the files route gave the upload.
	consume := "umask 077\ncat \"$upload_dir/payload\" > \"$upload_dir/file\"\nmv -f \"$upload_dir/file\" " + core.ShellQuote(absPath)
	return c.withUploadedFile(ctx, machineID, strings.NewReader(content), int64(len(content)), prepare, parent, consume, "write")
}

// withUploadedFile owns only this upload's private staging directory: one exec
// creates it, the files route fills it, and a second exec consumes and removes
// it. Cleanup is not guaranteed after SIGKILL or ambiguous remote HTTP completion.
func (c *client) withUploadedFile(ctx context.Context, machineID string, data io.Reader, size int64, prepare, stagingParent, consume, operation string) error {
	stage := "set -eu\numask 077\n" + prepare +
		"upload_parent=$(cd " + stagingParent + " && pwd -P)\n" +
		"mktemp -d \"${upload_parent%/}/.crabbox-upload.XXXXXXXXXX\"\n"
	res, err := c.Exec(ctx, machineID, stage, "")
	if err != nil {
		return fmt.Errorf("smolvm direct %s staging: %w", operation, err)
	}
	if res.ExitCode != 0 {
		return commandExitError("smolvm direct "+operation, res)
	}
	uploadDir := strings.TrimSuffix(res.Output, "\n")
	if !validUploadDir(uploadDir) {
		return fmt.Errorf("smolvm direct %s staging returned an invalid private upload directory", operation)
	}
	// The payload goes over the files route because /exec takes a JSON body
	// capped at 1 MiB; the files route takes the raw bytes up to 100 MiB.
	err = c.putFile(ctx, machineID, uploadDir+"/payload", data, size)
	if err == nil {
		res, err = c.Exec(ctx, machineID, "set -eu\nupload_dir="+core.ShellQuote(uploadDir)+"\n"+
			`trap 'upload_status=$?; trap - 0; rm -rf -- "$upload_dir" || { echo "upload staging cleanup failed" >&2; [ "$upload_status" -ne 0 ] || upload_status=1; }; exit "$upload_status"' 0`+"\n"+
			"trap 'exit 129' HUP\ntrap 'exit 130' INT\ntrap 'exit 143' TERM\n"+consume+"\n", "")
	}
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadCleanupTimeout)
		defer cancel()
		if cleanup, cleanupErr := c.Exec(cleanupCtx, machineID, "rm -rf -- "+core.ShellQuote(uploadDir), ""); cleanupErr != nil || cleanup.ExitCode != 0 {
			return fmt.Errorf("smolvm direct %s: %w (upload staging cleanup failed)", operation, err)
		}
		return fmt.Errorf("smolvm direct %s: %w", operation, err)
	}
	if res.ExitCode != 0 {
		return commandExitError("smolvm direct "+operation, res)
	}
	return nil
}

func validUploadDir(dir string) bool {
	// Never turn malformed remote output into an upload target or rm -rf path.
	if !path.IsAbs(dir) || path.Clean(dir) != dir || strings.IndexFunc(dir, unicode.IsControl) >= 0 {
		return false
	}
	suffix, ok := strings.CutPrefix(path.Base(dir), ".crabbox-upload.")
	if !ok || len(suffix) != 10 {
		return false
	}
	for _, char := range suffix {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func (c *client) putFile(ctx context.Context, machineID, guestPath string, data io.Reader, size int64) error {
	u := c.base + "/v1/machines/" + url.PathEscape(machineID) + "/files" + (&url.URL{Path: guestPath}).EscapedPath()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, data)
	if err != nil {
		return err
	}
	req.ContentLength = size
	c.addHeaders(req)
	req.Header.Set("Content-Type", "application/octet-stream")
	httpClient := c.dataHTTP
	if httpClient == nil {
		httpClient = c.http
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp, c.apiKey)
	}
	return nil
}

func (c *client) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	return c.doJSONWithClient(ctx, c.http, method, path, query, body, out)
}

func (c *client) doDataJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	httpClient := c.dataHTTP
	if httpClient == nil {
		httpClient = c.http
	}
	return c.doJSONWithClient(ctx, httpClient, method, path, query, body, out)
}

func (c *client) doJSONWithClient(ctx context.Context, httpClient *http.Client, method, path string, query url.Values, body any, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := shared.NewCompactJSONRequest(ctx, method, u, body)
	if err != nil {
		return err
	}
	c.addHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp, c.apiKey)
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

func apiError(resp *http.Response, apiKey string) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	msg := shared.RedactErrorSecrets(strings.TrimSpace(string(data)), apiKey)
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
	return core.Exit(result.ExitCode, "%s: %s", prefix, msg)
}

func isNotFound(err error) bool {
	var apiErr *smolvmAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
