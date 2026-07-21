package semaphore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type apiClient struct {
	host  string
	token string
	http  *http.Client
	rt    core.Runtime
}

type jobInfo struct {
	ID    string
	Name  string
	State string
}

type jobStatus struct {
	Name    string
	State   string
	IP      string
	SSHPort int
}

const userAgent = "SemaphoreCI v2.0 Client"

var waitForRunningPollInterval = 2 * time.Second

func newAPIClient(host, token string, rt core.Runtime) *apiClient {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	if rt.HTTP != nil {
		httpClient = rt.HTTP
	}
	return &apiClient{host: host, token: token, http: httpClient, rt: rt}
}

// CreateJob creates a standalone Semaphore job with a keepalive script.
// Returns the job ID.
func (c *apiClient) CreateJob(ctx context.Context, project, machine, osImage string, idleTimeout time.Duration) (string, error) {
	// Resolve project name to ID
	projectID, err := c.resolveProjectID(ctx, project)
	if err != nil {
		return "", fmt.Errorf("resolve project %q: %w", project, err)
	}

	durationSecs := int(idleTimeout.Seconds())

	keepalive := fmt.Sprintf("sudo mkdir -p /work/crabbox && sudo chown $(whoami) /work/crabbox && echo crabbox-testbox-ready && sleep %d", durationSecs)

	body := map[string]any{
		"apiVersion": "v1alpha",
		"kind":       "Job",
		"metadata":   map[string]string{"name": "crabbox testbox"},
		"spec": map[string]any{
			"project_id": projectID,
			"agent": map[string]any{
				"machine": map[string]string{
					"type":     machine,
					"os_image": osImage,
				},
			},
			"commands": []string{keepalive},
		},
	}

	var result struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	if err := c.post(ctx, "/api/v1alpha/jobs", body, &result); err != nil {
		return "", err
	}
	if result.Metadata.ID == "" {
		return "", fmt.Errorf("job creation returned no ID")
	}
	return result.Metadata.ID, nil
}

// WaitForRunning polls until the job reaches RUNNING state.
// Returns the SSH IP and port.
func (c *apiClient) WaitForRunning(ctx context.Context, jobID string, tick func()) (string, int, error) {
	var lastTransientErr error
	for i := 0; i < 120; i++ {
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(waitForRunningPollInterval):
		}
		tick()

		status, err := c.GetJobStatus(ctx, jobID)
		if err != nil {
			if !retryableJobStatusError(ctx, err) {
				return "", 0, err
			}
			lastTransientErr = err
			continue
		}
		lastTransientErr = nil
		if status.State == "FINISHED" {
			return "", 0, core.Exit(5, "job %s finished before reaching RUNNING state", jobID)
		}
		if status.State == "RUNNING" {
			if status.IP != "" && status.SSHPort > 0 {
				return status.IP, status.SSHPort, nil
			}
			continue
		}
	}
	if lastTransientErr != nil {
		return "", 0, core.Exit(5, "job %s did not reach RUNNING state within timeout: last status error: %v", jobID, lastTransientErr)
	}
	return "", 0, core.Exit(5, "job %s did not reach RUNNING state within timeout", jobID)
}

func retryableJobStatusError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	var apiErr *semaphoreAPIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return false
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return false
	}
	return true
}

// GetJobStatus returns the job metadata, state, IP, and SSH port.
func (c *apiClient) GetJobStatus(ctx context.Context, jobID string) (jobStatus, error) {
	var result struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			State string `json:"state"`
			Agent struct {
				IP    string `json:"ip"`
				Ports []struct {
					Name   string `json:"name"`
					Number int    `json:"number"`
				} `json:"ports"`
			} `json:"agent"`
		} `json:"status"`
	}
	if err := c.get(ctx, "/api/v1alpha/jobs/"+jobID, &result); err != nil {
		return jobStatus{}, err
	}
	port := 0
	for _, p := range result.Status.Agent.Ports {
		if p.Name == "ssh" {
			port = p.Number
		}
	}
	return jobStatus{
		Name:    result.Metadata.Name,
		State:   result.Status.State,
		IP:      result.Status.Agent.IP,
		SSHPort: port,
	}, nil
}

// GetSSHKey returns the SSH private key for a job.
func (c *apiClient) GetSSHKey(ctx context.Context, jobID string) (string, error) {
	var result struct {
		Key string `json:"key"`
	}
	if err := c.get(ctx, "/api/v1alpha/jobs/"+jobID+"/debug_ssh_key", &result); err != nil {
		return "", err
	}
	if result.Key == "" {
		return "", fmt.Errorf("no SSH key returned for job %s", jobID)
	}
	return result.Key, nil
}

// StopJob stops a running job.
func (c *apiClient) StopJob(ctx context.Context, jobID string) error {
	return c.post(ctx, "/api/v1alpha/jobs/"+jobID+"/stop", nil, nil)
}

// ListRunningJobs returns currently running jobs.
func (c *apiClient) ListRunningJobs(ctx context.Context) ([]jobInfo, error) {
	var result struct {
		Jobs []struct {
			Metadata struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"jobs"`
	}
	if err := c.get(ctx, "/api/v1alpha/jobs?states=RUNNING", &result); err != nil {
		return nil, err
	}
	var jobs []jobInfo
	for _, j := range result.Jobs {
		jobs = append(jobs, jobInfo{
			ID:    j.Metadata.ID,
			Name:  j.Metadata.Name,
			State: j.Status.State,
		})
	}
	return jobs, nil
}

func (c *apiClient) resolveProjectID(ctx context.Context, name string) (string, error) {
	// Try direct GET by name
	var project struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	err := c.get(ctx, "/api/v1alpha/projects/"+name, &project)
	if err == nil && project.Metadata.ID != "" {
		return project.Metadata.ID, nil
	}

	// Fallback: paginate through all projects and match by name
	type projectEntry struct {
		Metadata struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"metadata"`
	}
	path := "/api/v1alpha/projects"
	for path != "" {
		var projects []projectEntry
		resp, headers, err := c.getWithHeaders(ctx, path)
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(resp, &projects); err != nil {
			return "", err
		}
		for _, p := range projects {
			if p.Metadata.Name == name {
				return p.Metadata.ID, nil
			}
		}
		path, err = c.nextLinkPath(headers)
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("project %q not found", name)
}

// nextLinkPath returns the next-page request path from a Link header, or "" when
// pagination is finished. References are resolved against the configured host and
// rejected when they would change scheme, hostname, port, or introduce userinfo
// (which previously let `@host/...` relative links turn the configured host into
// URL userinfo and attach the API token to an unrelated hostname).
func (c *apiClient) nextLinkPath(headers http.Header) (string, error) {
	for _, part := range strings.Split(headers.Get("Link"), ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 || !strings.Contains(part, `rel="next"`) {
			continue
		}
		raw := strings.TrimSpace(sections[0])
		raw = strings.TrimPrefix(raw, "<")
		raw = strings.TrimSuffix(raw, ">")
		path, err := c.resolvePaginationRef(raw)
		if err != nil {
			return "", err
		}
		if path != "" {
			return path, nil
		}
	}
	return "", nil
}

func (c *apiClient) resolvePaginationRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	base, err := c.configuredBaseURL()
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("semaphore pagination link: %w", err)
	}
	resolved := base.ResolveReference(parsed)
	if resolved.User != nil {
		return "", fmt.Errorf("semaphore pagination link contains userinfo")
	}
	if !sameOriginURL(base, resolved) {
		return "", fmt.Errorf("semaphore pagination link points outside configured host")
	}
	return resolved.RequestURI(), nil
}

func (c *apiClient) configuredBaseURL() (*url.URL, error) {
	base, err := url.Parse("https://" + c.host)
	if err != nil {
		return nil, fmt.Errorf("semaphore configured host: %w", err)
	}
	if base.Path == "" {
		base.Path = "/"
	}
	return base, nil
}

// apiURL builds an absolute request URL for a same-origin request path or URI.
// It rejects values that would select a different authority than c.host.
func (c *apiClient) apiURL(path string) (string, error) {
	base, err := c.configuredBaseURL()
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("semaphore request path: %w", err)
	}
	resolved := base.ResolveReference(parsed)
	if resolved.User != nil {
		return "", fmt.Errorf("semaphore request URL contains userinfo")
	}
	if !sameOriginURL(base, resolved) {
		return "", fmt.Errorf("semaphore request URL points outside configured host")
	}
	return resolved.String(), nil
}

func sameOriginURL(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveURLPort(a) == effectiveURLPort(b)
}

func effectiveURLPort(value *url.URL) string {
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

func (c *apiClient) getWithHeaders(ctx context.Context, path string) ([]byte, http.Header, error) {
	endpoint, err := c.apiURL(path)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, c.responseError(path, resp.StatusCode, body)
	}
	return body, resp.Header, nil
}

type semaphoreAPIError struct {
	Path       string
	StatusCode int
	Body       string
}

func (e *semaphoreAPIError) Error() string {
	return fmt.Sprintf("semaphore API %s returned %d: %s", e.Path, e.StatusCode, e.Body)
}

func (c *apiClient) responseError(path string, statusCode int, body []byte) *semaphoreAPIError {
	return &semaphoreAPIError{
		Path:       path,
		StatusCode: statusCode,
		Body:       redactSemaphoreSecrets(strings.TrimSpace(string(body)), c.token),
	}
}

func redactSemaphoreSecrets(value string, secrets ...string) string {
	redacted := value
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[redacted]")
		}
	}
	return redacted
}

func (c *apiClient) get(ctx context.Context, path string, target any) error {
	endpoint, err := c.apiURL(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.responseError(path, resp.StatusCode, body)
	}
	if target != nil {
		return json.Unmarshal(body, target)
	}
	return nil
}

func (c *apiClient) post(ctx context.Context, path string, payload any, target any) error {
	var bodyReader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	endpoint, err := c.apiURL(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.responseError(path, resp.StatusCode, body)
	}
	if target != nil {
		return json.Unmarshal(body, target)
	}
	return nil
}
