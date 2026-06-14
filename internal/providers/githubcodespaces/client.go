package githubcodespaces

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type client struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

type createCodespaceRequest struct {
	Repo             string
	Ref              string
	Machine          string
	DevcontainerPath string
	WorkingDirectory string
	Geo              string
	IdleTimeout      time.Duration
	RetentionPeriod  time.Duration
	DisplayName      string
}

type codespace struct {
	Name          string        `json:"name"`
	DisplayName   string        `json:"display_name"`
	State         string        `json:"state"`
	EnvironmentID string        `json:"environment_id"`
	Repository    repositoryRef `json:"repository"`
	Machine       machineRef    `json:"machine"`
}

type repositoryRef struct {
	FullName string
}

type machineRef struct {
	Name string
}

func (r *repositoryRef) UnmarshalJSON(data []byte) error {
	var object struct {
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(data, &object); err == nil && object.FullName != "" {
		r.FullName = object.FullName
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	r.FullName = value
	return nil
}

func (m *machineRef) UnmarshalJSON(data []byte) error {
	var object struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &object); err == nil && object.Name != "" {
		m.Name = object.Name
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	m.Name = value
	return nil
}

func newClient(cfg GitHubCodespacesConfig, rt Runtime, token string) client {
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	if baseURL == "" {
		baseURL = defaultAPIURL
	}
	return client{httpClient: httpClient, baseURL: baseURL, token: token}
}

func (c client) createCodespace(ctx context.Context, req createCodespaceRequest) (codespace, error) {
	owner, repo, ok := strings.Cut(strings.TrimSpace(req.Repo), "/")
	if !ok || owner == "" || repo == "" {
		return codespace{}, exit(2, "github-codespaces repo must be owner/name")
	}
	body := map[string]any{}
	if req.Ref != "" {
		body["ref"] = req.Ref
	}
	if req.Machine != "" {
		body["machine"] = req.Machine
	}
	if req.DevcontainerPath != "" {
		body["devcontainer_path"] = req.DevcontainerPath
	}
	if req.WorkingDirectory != "" {
		body["working_directory"] = req.WorkingDirectory
	}
	if req.Geo != "" {
		body["location"] = req.Geo
	}
	if req.IdleTimeout > 0 {
		body["idle_timeout_minutes"] = durationMinutesCeil(req.IdleTimeout)
	}
	if req.RetentionPeriod > 0 {
		body["retention_period_minutes"] = durationMinutesCeil(req.RetentionPeriod)
	}
	if req.DisplayName != "" {
		body["display_name"] = req.DisplayName
	}
	return c.doJSON(ctx, http.MethodPost, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/codespaces", body)
}

func (c client) doJSON(ctx context.Context, method, path string, body any) (codespace, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return codespace{}, err
		}
		reader = bytes.NewReader(data)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return codespace{}, err
	}
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(c.token) != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return codespace{}, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return codespace{}, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return codespace{}, githubAPIError(resp.StatusCode, resp.Header.Get("Retry-After"), string(data))
	}
	var out codespace
	if err := json.Unmarshal(data, &out); err != nil {
		return codespace{}, err
	}
	return out, nil
}

func githubAPIError(status int, retryAfter, body string) error {
	message := http.StatusText(status)
	if strings.TrimSpace(body) != "" {
		var parsed struct {
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(body), &parsed) == nil && parsed.Message != "" {
			message = parsed.Message
		}
	}
	message = redactSecretText(message)
	action := "check GitHub Codespaces access"
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		action = "check gh auth or GH_TOKEN/GITHUB_TOKEN scopes"
	case http.StatusNotFound:
		action = "check repository and Codespaces availability"
	case http.StatusConflict:
		action = "wait for the pending Codespaces operation to finish"
	case http.StatusUnprocessableEntity:
		action = "check Codespaces repo/ref/machine/devcontainer settings"
	case http.StatusServiceUnavailable:
		action = "retry after GitHub service recovery"
	}
	if retryAfter != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil {
			return fmt.Errorf("github-codespaces API status=%d retry_after=%s: %s; %s", status, (time.Duration(seconds) * time.Second).String(), message, action)
		}
		return fmt.Errorf("github-codespaces API status=%d retry_after=%s: %s; %s", status, retryAfter, message, action)
	}
	return fmt.Errorf("github-codespaces API status=%d: %s; %s", status, message, action)
}

func durationMinutesCeil(value time.Duration) int {
	if value <= 0 {
		return 0
	}
	minutes := int(value / time.Minute)
	if value%time.Minute != 0 {
		minutes++
	}
	if minutes < 1 {
		return 1
	}
	return minutes
}
