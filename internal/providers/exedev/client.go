package exedev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type exeDevAPI interface {
	Exec(ctx context.Context, command string, stdout, stderr io.Writer) (int, error)
}

type exeDevClient struct {
	apiKey     string
	apiURL     string
	httpClient *http.Client
}

type exeDevAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *exeDevAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

// exeDevCommandFailedError signals that the exe.dev API ran the command
// successfully (HTTP 422 per https://exe.dev/docs/https-api) but the command
// itself exited non-zero. The body carries the error message from the command.
type exeDevCommandFailedError struct {
	Body string
}

func (e *exeDevCommandFailedError) Error() string {
	if e.Body == "" {
		return "command failed"
	}
	return "command failed: " + e.Body
}

func newExeDevClient(cfg Config, rt Runtime) (exeDevAPI, error) {
	apiKey := strings.TrimSpace(cfg.ExeDev.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=%s requires EXE_API_KEY", providerName)
	}
	apiURL := strings.TrimRight(strings.TrimSpace(blank(cfg.ExeDev.APIURL, "https://exe.dev")), "/")
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, exit(2, "%s url %q is invalid", providerName, apiURL)
	}
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return nil, exit(2, "%s url %q must use https unless it targets localhost", providerName, apiURL)
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &exeDevClient{apiKey: apiKey, apiURL: apiURL, httpClient: httpClient}, nil
}

func (c *exeDevClient) Exec(ctx context.Context, command string, stdout, stderr io.Writer) (int, error) {
	if strings.TrimSpace(command) == "" {
		return 1, errors.New("empty command")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/exec", bytes.NewReader([]byte(command)))
	if err != nil {
		return 1, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json, text/plain, application/octet-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if stdout == nil {
		stdout = io.Discard
	}
	// Per https://exe.dev/docs/https-api, HTTP 422 means the command ran but
	// returned a non-zero exit code; the response body contains the error
	// message. Treat it as a command failure (the body is still part of the
	// command's output) rather than a transport-level API error.
	if resp.StatusCode == http.StatusUnprocessableEntity {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if _, err := io.Copy(stdout, bytes.NewReader(data)); err != nil {
			return 1, fmt.Errorf("read exe.dev exec body: %w", err)
		}
		return 1, &exeDevCommandFailedError{Body: strings.TrimSpace(string(data))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 1, &exeDevAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(data))}
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		return 1, fmt.Errorf("read exe.dev exec body: %w", err)
	}
	// Per https://exe.dev/docs/https-api, JSON output is always enabled for
	// API responses (equivalent to --json) and the returned body is the ssh
	// command output. A 2xx response means the command exited 0; 422 (handled
	// above) means non-zero exit; all other non-2xx statuses are transport
	// errors surfaced through exeDevAPIError.
	return 0, nil
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
