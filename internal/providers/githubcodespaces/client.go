package githubcodespaces

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type githubAPIResponseError struct {
	status  int
	message string
}

const (
	githubCodespacesListPageSize    = 30
	githubCodespacesMaxResponseSize = 16 << 20
)

func (e *githubAPIResponseError) Error() string { return e.message }

type client struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

type githubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
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
	RetentionSet     bool
	DisplayName      string
}

type codespace struct {
	ID                     int64         `json:"id"`
	Name                   string        `json:"name"`
	DisplayName            string        `json:"display_name"`
	State                  string        `json:"state"`
	EnvironmentID          string        `json:"environment_id"`
	RetentionPeriodMinutes *int          `json:"retention_period_minutes"`
	Owner                  githubUser    `json:"owner"`
	Repository             repositoryRef `json:"repository"`
	Machine                machineRef    `json:"machine"`
	GitStatus              gitStatus     `json:"git_status"`
}

func (c client) currentUser(ctx context.Context) (githubUser, error) {
	var user githubUser
	if err := c.do(ctx, http.MethodGet, "/user", nil, &user, nil); err != nil {
		return githubUser{}, err
	}
	user.Login = strings.TrimSpace(user.Login)
	if user.ID <= 0 || user.Login == "" {
		return githubUser{}, exit(4, "github-codespaces API returned incomplete authenticated user identity")
	}
	return user, nil
}

type gitStatus struct {
	Ahead                 int    `json:"ahead"`
	Behind                int    `json:"behind"`
	HasUnpushedChanges    bool   `json:"has_unpushed_changes"`
	HasUncommittedChanges bool   `json:"has_uncommitted_changes"`
	Ref                   string `json:"ref"`
	aheadPresent          bool
	unpushedPresent       bool
	uncommittedPresent    bool
}

func (s *gitStatus) UnmarshalJSON(data []byte) error {
	var raw struct {
		Ahead                 *int   `json:"ahead"`
		Behind                int    `json:"behind"`
		HasUnpushedChanges    *bool  `json:"has_unpushed_changes"`
		HasUncommittedChanges *bool  `json:"has_uncommitted_changes"`
		Ref                   string `json:"ref"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = gitStatus{Behind: raw.Behind, Ref: raw.Ref}
	if raw.Ahead != nil {
		s.Ahead = *raw.Ahead
		s.aheadPresent = true
	}
	if raw.HasUnpushedChanges != nil {
		s.HasUnpushedChanges = *raw.HasUnpushedChanges
		s.unpushedPresent = true
	}
	if raw.HasUncommittedChanges != nil {
		s.HasUncommittedChanges = *raw.HasUncommittedChanges
		s.uncommittedPresent = true
	}
	return nil
}

func (s gitStatus) deletionSafetyKnown() bool {
	return s.aheadPresent && s.unpushedPresent && s.uncommittedPresent
}

type codespaceMachine struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

type repositoryRef struct {
	ID       int64
	FullName string
}

type machineRef struct {
	Name string
}

func (r *repositoryRef) UnmarshalJSON(data []byte) error {
	var object struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(data, &object); err == nil && object.FullName != "" {
		r.ID = object.ID
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
	privateClient := *httpClient
	// GitHub API requests carry a bearer token. API redirects are not required
	// for this client, so fail closed before net/http can forward credentials.
	privateClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	if baseURL == "" {
		baseURL = defaultAPIURL
	}
	return client{httpClient: &privateClient, baseURL: baseURL, token: token}
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
		body["geo"] = req.Geo
	}
	if req.IdleTimeout > 0 {
		body["idle_timeout_minutes"] = durationMinutesCeil(req.IdleTimeout)
	}
	if req.RetentionPeriod > 0 || req.RetentionSet {
		body["retention_period_minutes"] = durationMinutesCeil(req.RetentionPeriod)
	}
	if req.DisplayName != "" {
		body["display_name"] = req.DisplayName
	}
	return c.doJSON(ctx, http.MethodPost, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/codespaces", body)
}

func (c client) listCodespaces(ctx context.Context) ([]codespace, error) {
	path := fmt.Sprintf("/user/codespaces?per_page=%d", githubCodespacesListPageSize)
	var all []codespace
	for path != "" {
		var out struct {
			Codespaces []codespace `json:"codespaces"`
		}
		header, err := c.doWithHeader(ctx, http.MethodGet, path, nil, &out, nil)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Codespaces...)
		path, err = nextLinkPath(header.Get("Link"), c.baseURL)
		if err != nil {
			return nil, err
		}
	}
	return all, nil
}

func (c client) getCodespace(ctx context.Context, name string) (codespace, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return codespace{}, exit(2, "github-codespaces codespace name is required")
	}
	var out codespace
	if err := c.do(ctx, http.MethodGet, "/user/codespaces/"+url.PathEscape(name), nil, &out, nil); err != nil {
		return codespace{}, err
	}
	return out, nil
}

func (c client) startCodespace(ctx context.Context, name string) (codespace, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return codespace{}, exit(2, "github-codespaces codespace name is required")
	}
	var out codespace
	if err := c.do(ctx, http.MethodPost, "/user/codespaces/"+url.PathEscape(name)+"/start", nil, &out, map[int]bool{http.StatusNotModified: true}); err != nil {
		return codespace{}, err
	}
	if out.Name == "" {
		out.Name = name
	}
	return out, nil
}

func (c client) stopCodespace(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return exit(2, "github-codespaces codespace name is required")
	}
	return c.do(ctx, http.MethodPost, "/user/codespaces/"+url.PathEscape(name)+"/stop", nil, nil, map[int]bool{http.StatusNotModified: true})
}

func (c client) deleteCodespace(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return exit(2, "github-codespaces codespace name is required")
	}
	return c.do(ctx, http.MethodDelete, "/user/codespaces/"+url.PathEscape(name), nil, nil, map[int]bool{http.StatusNotModified: true})
}

func (c client) listMachines(ctx context.Context, repo, ref string) ([]codespaceMachine, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" {
		return nil, exit(2, "github-codespaces repo must be owner/name")
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/codespaces/machines"
	if strings.TrimSpace(ref) != "" {
		path += "?ref=" + url.QueryEscape(strings.TrimSpace(ref))
	}
	var out struct {
		Machines []codespaceMachine `json:"machines"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out, nil); err != nil {
		return nil, err
	}
	return out.Machines, nil
}

func (c client) doJSON(ctx context.Context, method, path string, body any) (codespace, error) {
	var out codespace
	if err := c.do(ctx, method, path, body, &out, nil); err != nil {
		return codespace{}, err
	}
	return out, nil
}

func (c client) do(ctx context.Context, method, path string, body any, out any, accepted map[int]bool) error {
	_, err := c.doWithHeader(ctx, method, path, body, out, accepted)
	return err
}

func (c client) doWithHeader(ctx context.Context, method, path string, body any, out any, accepted map[int]bool) (http.Header, error) {
	if err := validateGitHubCodespacesAPIBase(c.baseURL); err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	reqURL := path
	if !strings.HasPrefix(reqURL, "http://") && !strings.HasPrefix(reqURL, "https://") {
		reqURL = c.baseURL + path
	}
	parsedRequestURL, err := url.Parse(reqURL)
	if err != nil {
		return nil, err
	}
	allowed, err := sameAPIBase(parsedRequestURL, c.baseURL)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf("github-codespaces request outside configured API base: %s://%s%s", parsedRequestURL.Scheme, parsedRequestURL.Host, parsedRequestURL.EscapedPath())
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, reqURL, reader)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, githubCodespacesMaxResponseSize+1))
	if readErr != nil {
		return nil, readErr
	}
	if len(data) > githubCodespacesMaxResponseSize {
		return nil, exit(4, "github-codespaces API response exceeds %d-byte limit", githubCodespacesMaxResponseSize)
	}
	if accepted == nil {
		accepted = map[int]bool{}
	}
	if accepted[resp.StatusCode] || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		if out == nil || len(strings.TrimSpace(string(data))) == 0 {
			return resp.Header, nil
		}
		return resp.Header, json.Unmarshal(data, out)
	}
	return nil, githubAPIError(resp.StatusCode, resp.Header.Get("Retry-After"), string(data))
}

func validateGitHubCodespacesAPIBase(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid github-codespaces API URL: %w", err)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return exit(2, "github-codespaces API URL must be an origin with an optional path")
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return nil
	}
	host := strings.TrimSpace(parsed.Hostname())
	ip := net.ParseIP(host)
	if strings.EqualFold(parsed.Scheme, "http") && (strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return exit(2, "github-codespaces API URL must use https; http is allowed only for loopback testing")
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
		action = "check gh auth or GH_TOKEN/GITHUB_TOKEN scopes; " +
			"GitHub Codespaces requires the codespace scope (for gh, run gh auth refresh -h github.com -s codespace)"
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
			return &githubAPIResponseError{status: status, message: fmt.Sprintf("github-codespaces API status=%d retry_after=%s: %s; %s", status, (time.Duration(seconds) * time.Second).String(), message, action)}
		}
		return &githubAPIResponseError{status: status, message: fmt.Sprintf("github-codespaces API status=%d retry_after=%s: %s; %s", status, retryAfter, message, action)}
	}
	return &githubAPIResponseError{status: status, message: fmt.Sprintf("github-codespaces API status=%d: %s; %s", status, message, action)}
}

func isGitHubNotFound(err error) bool {
	var apiErr *githubAPIResponseError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound
}

func githubAPIStatus(err error) (int, bool) {
	var apiErr *githubAPIResponseError
	if !errors.As(err, &apiErr) {
		return 0, false
	}
	return apiErr.status, true
}

func nextLinkPath(linkHeader, baseURL string) (string, error) {
	for _, part := range strings.Split(linkHeader, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 {
			continue
		}
		if !strings.Contains(strings.Join(sections[1:], ";"), `rel="next"`) {
			continue
		}
		raw := strings.TrimSpace(sections[0])
		raw = strings.TrimPrefix(raw, "<")
		raw = strings.TrimSuffix(raw, ">")
		if raw == "" {
			return "", nil
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			return raw, nil
		}
		if parsed.IsAbs() {
			allowed, err := sameAPIBase(parsed, baseURL)
			if err != nil {
				return "", err
			}
			if !allowed {
				return "", fmt.Errorf("github-codespaces pagination link outside configured API base: %s://%s%s", parsed.Scheme, parsed.Host, parsed.EscapedPath())
			}
			return raw, nil
		}
		return raw, nil
	}
	return "", nil
}

func sameAPIBase(next *url.URL, baseURL string) (bool, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(next.Scheme, base.Scheme) || !strings.EqualFold(next.Host, base.Host) {
		return false, nil
	}
	basePath := strings.TrimRight(base.Path, "/")
	if basePath == "" || basePath == "/" {
		return true, nil
	}
	return next.Path == basePath || strings.HasPrefix(next.Path, basePath+"/"), nil
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
