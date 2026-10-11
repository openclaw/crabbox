package azuresandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// ACA Sandboxes are separate from the Azure Dynamic Sessions pool API.
// Wire contract: azure-containerapps-sandbox 0.1.0b4, 2026-02-01-preview.
const apiVersion = "2026-02-01-preview"
const tokenScope = "https://dynamicsessions.io/.default"

type sandbox struct {
	ID           string            `json:"id"`
	State        string            `json:"state"`
	Labels       map[string]string `json:"labels"`
	StateDetails struct {
		StoppedReason string `json:"stoppedReason"`
	} `json:"stateDetails"`
}

type execResult struct {
	ExitCode *int   `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type createRequest struct {
	SourcesRef struct {
		DiskImage struct {
			ID       string `json:"id,omitempty"`
			Name     string `json:"name,omitempty"`
			IsPublic bool   `json:"isPublic,omitempty"`
		} `json:"diskImage"`
	} `json:"sourcesRef"`
	Resources map[string]string `json:"resources"`
	Labels    map[string]string `json:"labels"`
	Lifecycle map[string]any    `json:"lifecycle"`
}

type apiError struct{ Status int }

func (e *apiError) Error() string { return fmt.Sprintf("ACA Sandbox API returned HTTP %d", e.Status) }

type client struct {
	endpoint   string
	groupPath  string
	credential azcore.TokenCredential
	http       *http.Client
}

var resourceSegment = regexp.MustCompile(`^[A-Za-z0-9._()-]+$`)

func newClient(region, subscription, resourceGroup, group string, credential azcore.TokenCredential, transport http.RoundTripper) (*client, error) {
	for _, value := range []string{region, subscription, resourceGroup, group} {
		if !resourceSegment.MatchString(value) || value == "." || value == ".." {
			return nil, fmt.Errorf("ACA Sandbox requires explicit region, subscription, resource group and sandbox group")
		}
	}
	if !regexp.MustCompile(`^[a-z0-9]+$`).MatchString(region) || credential == nil {
		return nil, fmt.Errorf("ACA Sandbox region or credential is invalid")
	}
	return &client{
		endpoint:   "https://management." + region + ".azuredevcompute.io",
		groupPath:  "/subscriptions/" + subscription + "/resourceGroups/" + resourceGroup + "/sandboxGroups/" + group,
		credential: credential,
		http:       &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (c *client) request(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string, result any) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("api-version", apiVersion)
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path+"?"+query.Encode(), body)
	if err != nil {
		return fmt.Errorf("ACA Sandbox request is invalid")
	}
	// The SDK credential owns caching and renewal. Never cache a bearer in a lease.
	token, err := c.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{tokenScope}})
	if err != nil {
		return fmt.Errorf("ACA Sandbox managed-identity authentication failed")
	}
	if token.Token == "" || !token.ExpiresOn.After(time.Now()) {
		return fmt.Errorf("ACA Sandbox credential is expired")
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// No automatic retry: create and command outcomes may be uncertain. The fixed
	// lease journal reconciles creation before another mutation is considered.
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ACA Sandbox request failed: %w", ctxOrTransportError(ctx))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &apiError{Status: response.StatusCode}
	}
	if result == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(result); err != nil {
		return fmt.Errorf("ACA Sandbox response is invalid")
	}
	return nil
}

func ctxOrTransportError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("transport outcome unknown")
}

func (c *client) json(ctx context.Context, method, path string, body, result any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	return c.request(ctx, method, path, nil, input, "application/json", result)
}

func (c *client) sandboxPath(id string) (string, error) {
	if !resourceSegment.MatchString(id) || id == "." || id == ".." {
		return "", fmt.Errorf("ACA Sandbox ID is invalid")
	}
	return c.groupPath + "/sandboxes/" + id, nil
}

func (c *client) Create(ctx context.Context, body createRequest) (sandbox, error) {
	var result sandbox
	err := c.json(ctx, http.MethodPut, c.groupPath+"/sandboxes", body, &result)
	if err == nil && result.ID == "" {
		err = fmt.Errorf("ACA Sandbox create returned no resource identity; outcome unknown")
	}
	return result, err
}

func (c *client) Get(ctx context.Context, id string) (sandbox, error) {
	var result sandbox
	path, err := c.sandboxPath(id)
	if err != nil {
		return result, err
	}
	err = c.json(ctx, http.MethodGet, path, nil, &result)
	if err == nil && result.ID != id {
		err = fmt.Errorf("ACA Sandbox identity changed")
	}
	return result, err
}

func (c *client) List(ctx context.Context) ([]sandbox, error) {
	path := c.groupPath + "/sandboxes"
	query := url.Values{}
	var all []sandbox
	seen := map[string]bool{}
	for {
		var raw json.RawMessage
		if err := c.request(ctx, http.MethodGet, path, query, nil, "", &raw); err != nil {
			return nil, err
		}
		var page struct {
			Value    []sandbox `json:"value"`
			NextLink string    `json:"nextLink"`
		}
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			if err := json.Unmarshal(raw, &page.Value); err != nil {
				return nil, err
			}
		} else if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		if page.Value == nil {
			return nil, fmt.Errorf("ACA Sandbox inventory returned no resource array; outcome unknown")
		}
		all = append(all, page.Value...)
		if page.NextLink == "" {
			return all, nil
		}
		next, err := url.Parse(page.NextLink)
		origin, _ := url.Parse(c.endpoint)
		if err != nil || next.Scheme != origin.Scheme || next.Host != origin.Host || next.User != nil || next.Fragment != "" || next.Path != c.groupPath+"/sandboxes" || seen[page.NextLink] {
			return nil, fmt.Errorf("ACA Sandbox continuation is outside the selected group or repeats")
		}
		seen[page.NextLink] = true
		path, query = next.Path, next.Query()
	}
}

func (c *client) Delete(ctx context.Context, id string) error {
	path, err := c.sandboxPath(id)
	if err != nil {
		return err
	}
	return c.json(ctx, http.MethodDelete, path, nil, nil)
}

func (c *client) Resume(ctx context.Context, id string) error {
	path, err := c.sandboxPath(id)
	if err != nil {
		return err
	}
	return c.json(ctx, http.MethodPost, path+"/resume", nil, nil)
}

func (c *client) Exec(ctx context.Context, id, command, workdir string) (execResult, error) {
	var result execResult
	path, err := c.sandboxPath(id)
	if err != nil {
		return result, err
	}
	body := map[string]string{"command": command}
	if workdir != "" {
		body["workingDirectory"] = workdir
	}
	err = c.json(ctx, http.MethodPost, path+"/executeShellCommand", body, &result)
	if err == nil && result.ExitCode == nil {
		err = fmt.Errorf("ACA Sandbox command returned no exit status; outcome unknown")
	}
	return result, err
}

func (c *client) Upload(ctx context.Context, id, destination string, input io.Reader) error {
	path, err := c.sandboxPath(id)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(destination, "/") {
		return fmt.Errorf("ACA Sandbox upload requires an absolute path")
	}
	return c.request(ctx, http.MethodPut, path+"/files", url.Values{"path": {destination}, "createDirs": {"true"}}, input, "application/octet-stream", nil)
}
