package upstashbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type api interface {
	CreateBox(context.Context, createRequest) (boxData, error)
	GetBox(context.Context, string) (boxData, error)
	ListBoxes(context.Context) ([]boxData, error)
	DeleteBoxes(context.Context, []string) error
	Exec(context.Context, string, string, string) (execResult, error)
	ExecStream(context.Context, string, string, string, io.Writer) (int, error)
	UploadFile(context.Context, string, string, string) error
	WriteFile(context.Context, string, string, string) error
}

type client struct {
	apiKey string
	base   string
	http   *http.Client
}

type createRequest struct {
	Name      string
	Runtime   string
	Size      string
	KeepAlive bool
	Env       map[string]string
}

type boxData struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Runtime   string `json:"runtime"`
	Size      string `json:"size"`
	Status    string `json:"status"`
	KeepAlive bool   `json:"keep_alive"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type execResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
	Error    string `json:"error"`
}

const upstashBoxDefaultResponseHeaderTimeout = 30 * time.Second

var upstashBoxCleanupTimeout = 15 * time.Second

var newAPI = func(cfg Config, rt Runtime) (api, error) {
	apiKey := strings.TrimSpace(cfg.UpstashBox.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=%s requires UPSTASH_BOX_API_KEY", providerName)
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = defaultUpstashBoxHTTPClient()
	}
	base := strings.TrimRight(blank(strings.TrimSpace(cfg.UpstashBox.BaseURL), "https://us-east-1.box.upstash.com"), "/")
	return &client{apiKey: apiKey, base: base, http: httpClient}, nil
}

func upstashBoxCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), upstashBoxCleanupTimeout)
}

func defaultUpstashBoxHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = upstashBoxDefaultResponseHeaderTimeout
	return &http.Client{Transport: transport}
}

func (c *client) CreateBox(ctx context.Context, req createRequest) (boxData, error) {
	body := map[string]any{}
	if req.Name != "" {
		body["name"] = req.Name
	}
	if req.Runtime != "" {
		body["runtime"] = req.Runtime
	}
	if req.Size != "" {
		body["size"] = req.Size
	}
	if req.KeepAlive {
		body["keep_alive"] = true
	}
	if len(req.Env) > 0 {
		body["env_vars"] = req.Env
	}
	var box boxData
	if err := c.doJSON(ctx, http.MethodPost, "/v2/box", nil, body, &box); err != nil {
		return boxData{}, err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		status := strings.ToLower(strings.TrimSpace(box.Status))
		if createStatusReady(status) {
			return box, nil
		}
		if status == "error" || status == "failed" {
			return boxData{}, c.cleanupCreatedBox(box.ID, exit(5, "upstash-box creation failed for %s", box.ID))
		}
		if time.Now().After(deadline) {
			return boxData{}, c.cleanupCreatedBox(box.ID, exit(5, "upstash-box creation timed out for %s status=%s", box.ID, blank(box.Status, "unknown")))
		}
		select {
		case <-ctx.Done():
			return boxData{}, c.cleanupCreatedBox(box.ID, ctx.Err())
		case <-time.After(2 * time.Second):
		}
		next, err := c.GetBox(ctx, box.ID)
		if err == nil {
			box = next
		}
	}
}

func (c *client) cleanupCreatedBox(boxID string, cause error) error {
	if strings.TrimSpace(boxID) == "" {
		return cause
	}
	cleanupCtx, cancel := upstashBoxCleanupContext()
	defer cancel()
	if err := c.DeleteBoxes(cleanupCtx, []string{boxID}); err != nil {
		return fmt.Errorf("%w; cleanup failed for upstash-box %s: %v", cause, boxID, err)
	}
	return cause
}

func createStatusReady(status string) bool {
	switch status {
	case "idle", "running", "ready", "paused":
		return true
	default:
		return false
	}
}

func (c *client) GetBox(ctx context.Context, id string) (boxData, error) {
	var box boxData
	if err := c.doJSON(ctx, http.MethodGet, "/v2/box/"+url.PathEscape(id), nil, nil, &box); err != nil {
		return boxData{}, err
	}
	return box, nil
}

func (c *client) ListBoxes(ctx context.Context) ([]boxData, error) {
	var boxes []boxData
	if err := c.doJSON(ctx, http.MethodGet, "/v2/box", nil, nil, &boxes); err != nil {
		return nil, err
	}
	return boxes, nil
}

func (c *client) DeleteBoxes(ctx context.Context, ids []string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v2/box", nil, map[string]any{"ids": ids}, nil)
}

func (c *client) Exec(ctx context.Context, boxID, command, folder string) (execResult, error) {
	var result execResult
	body := map[string]any{"command": []string{"sh", "-c", command}}
	if strings.TrimSpace(folder) != "" {
		body["folder"] = strings.TrimSpace(folder)
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v2/box/"+url.PathEscape(boxID)+"/exec", nil, body, &result); err != nil {
		return execResult{}, err
	}
	return result, nil
}

func (c *client) ExecStream(ctx context.Context, boxID, command, folder string, stdout io.Writer) (int, error) {
	body := map[string]any{"command": []string{"sh", "-c", command}}
	if strings.TrimSpace(folder) != "" {
		body["folder"] = strings.TrimSpace(folder)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v2/box/"+url.PathEscape(boxID)+"/exec-stream", bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	c.addHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, c.apiError(resp)
	}
	return parseExecStream(resp.Body, stdout, c.apiKey)
}

func (c *client) UploadFile(ctx context.Context, boxID, localPath, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	reader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	producerDone := make(chan error, 1)
	go func() {
		var writeErr error
		defer func() {
			if closeErr := file.Close(); writeErr == nil {
				writeErr = closeErr
			}
			if closeErr := writer.Close(); writeErr == nil {
				writeErr = closeErr
			}
			_ = pipeWriter.CloseWithError(writeErr)
			producerDone <- writeErr
		}()
		if writeErr = writer.WriteField("paths", remotePath); writeErr != nil {
			return
		}
		var part io.Writer
		part, writeErr = writer.CreateFormFile("files", filepath.Base(remotePath))
		if writeErr != nil {
			return
		}
		_, writeErr = io.Copy(part, file)
	}()
	finishProducer := func(primary error) error {
		if primary != nil {
			_ = reader.CloseWithError(primary)
		}
		producerErr := <-producerDone
		if primary != nil {
			return primary
		}
		return producerErr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v2/box/"+url.PathEscape(boxID)+"/files/upload", reader)
	if err != nil {
		return finishProducer(err)
	}
	c.addHeaders(req)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return finishProducer(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return finishProducer(c.apiError(resp))
	}
	return finishProducer(nil)
}

func (c *client) WriteFile(ctx context.Context, boxID, remotePath, content string) error {
	body := map[string]any{"path": remotePath, "content": content}
	return c.doJSON(ctx, http.MethodPost, "/v2/box/"+url.PathEscape(boxID)+"/files/write", nil, body, nil)
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
		return c.apiError(resp)
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
		return fmt.Errorf("decode upstash-box response: %w", err)
	}
	return nil
}

func (c *client) addHeaders(req *http.Request) {
	req.Header.Set("X-Box-Api-Key", c.apiKey)
}

func (c *client) apiError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	msg := redactUpstashBoxSecrets(strings.TrimSpace(string(data)), c.apiKey)
	status := redactUpstashBoxSecrets(resp.Status, c.apiKey)
	if msg == "" {
		msg = status
	}
	return fmt.Errorf("upstash-box API %s: %s", status, msg)
}

func redactUpstashBoxSecrets(value string, secrets ...string) string {
	for _, secret := range secrets {
		if strings.TrimSpace(secret) != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func parseExecStream(r io.Reader, stdout io.Writer, secrets ...string) (int, error) {
	reader := bufio.NewReader(r)
	var buffer strings.Builder
	for {
		chunk := make([]byte, 8192)
		n, readErr := reader.Read(chunk)
		if n > 0 {
			buffer.Write(chunk[:n])
			flushExecOutputCandidates(&buffer, stdout)
		}
		if readErr == io.EOF {
			return finishExecStream(&buffer, stdout, secrets...)
		}
		if readErr != nil {
			return 0, readErr
		}
	}
}

const (
	execExitMarker  = "event: exit\n"
	execErrorMarker = "event: error\n"
)

func flushExecOutputCandidates(buffer *strings.Builder, stdout io.Writer) {
	text := buffer.String()
	idx := lastExecEventIndex(text)
	if idx >= 0 {
		if idx > 0 {
			writeStreamOutput(stdout, text[:idx])
			rest := text[idx:]
			buffer.Reset()
			buffer.WriteString(rest)
		}
		return
	}
	keep := 64
	if len(text) > keep {
		emit := text[:len(text)-keep]
		writeStreamOutput(stdout, emit)
		rest := text[len(text)-keep:]
		buffer.Reset()
		buffer.WriteString(rest)
	}
}

func finishExecStream(buffer *strings.Builder, stdout io.Writer, secrets ...string) (int, error) {
	text := buffer.String()
	buffer.Reset()
	exitIdx := strings.LastIndex(text, execExitMarker)
	errorIdx := strings.LastIndex(text, execErrorMarker)
	if exitIdx >= 0 && exitIdx >= errorIdx {
		if exitIdx > 0 {
			writeStreamOutput(stdout, text[:exitIdx])
		}
		data := parseSSEData(text[exitIdx+len(execExitMarker):])
		if data == "" {
			return 1, fmt.Errorf("upstash-box exec stream missing exit data")
		}
		var parsed struct {
			ExitCode int `json:"exit_code"`
		}
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			return 1, fmt.Errorf("upstash-box exec stream invalid exit data: %w", err)
		}
		return parsed.ExitCode, nil
	}
	if errorIdx >= 0 {
		if errorIdx > 0 {
			writeStreamOutput(stdout, text[:errorIdx])
		}
		msg := parseSSEData(text[errorIdx+len(execErrorMarker):])
		if msg == "" {
			msg = "stream error"
		}
		msg = redactUpstashBoxSecrets(msg, secrets...)
		return 1, fmt.Errorf("upstash-box exec stream: %s", msg)
	}
	if text != "" {
		writeStreamOutput(stdout, text)
	}
	return 1, fmt.Errorf("upstash-box exec stream ended without exit event")
}

func lastExecEventIndex(text string) int {
	exitIdx := strings.LastIndex(text, execExitMarker)
	errorIdx := strings.LastIndex(text, execErrorMarker)
	if errorIdx > exitIdx {
		return errorIdx
	}
	return exitIdx
}

func writeStreamOutput(stdout io.Writer, text string) {
	if stdout == nil || text == "" {
		return
	}
	_, _ = io.WriteString(stdout, text)
}

func parseSSEData(text string) string {
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return ""
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
