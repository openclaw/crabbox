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
	req.Header.Set("Accept", "application/octet-stream, text/plain, application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 1, &exeDevAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(data))}
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		return 1, fmt.Errorf("read exe.dev exec body: %w", err)
	}
	// exe.dev does not document a separate exit-code surface, so a 2xx response
	// is treated as a successful command run; non-2xx is surfaced through
	// exeDevAPIError above. If the service later returns an exit code via a
	// trailer/header, parse it here.
	return 0, nil
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
