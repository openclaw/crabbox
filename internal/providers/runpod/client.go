package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// runpodAPI covers RunPod pod REST operations and GraphQL account identity. The
// provider keeps the surface intentionally small: deploy a pod, look it up,
// list pods, and terminate it. The pod's full SSH coordinates come back as
// ssh.direct once the pod reaches RUNNING.
type runpodAPI interface {
	Whoami(ctx context.Context) (runpodMyself, error)
	DeployPod(ctx context.Context, input runpodDeployInput) (runpodPod, error)
	GetPod(ctx context.Context, podID string) (runpodPod, error)
	ListPods(ctx context.Context) ([]runpodPod, error)
	TerminatePod(ctx context.Context, podID string) error
}

type runpodClient struct {
	apiKey     string
	apiURL     string
	httpClient *http.Client
}

const runpodMaxResponseBytes = 16 << 20

type runpodAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *runpodAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

type runpodMyself struct {
	ID                   string `json:"id"`
	Email                string `json:"email"`
	ClientBalance        any    `json:"clientBalance"`
	SignedTermsOfService bool   `json:"signedTermsOfService"`
}

type runpodDirectSSH struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}

type runpodSSH struct {
	Direct *runpodDirectSSH `json:"direct"`
}

type runpodPod struct {
	Env    map[string]string `json:"env"`
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	Status string            `json:"status"`
	SSH    runpodSSH         `json:"ssh"`
}

// Only the direct endpoint supports rsync. The proxy is shell-only.
func (p runpodPod) SSHEndpoint() runpodSSHEndpoint {
	if direct := p.SSH.Direct; direct != nil && direct.Host != "" && direct.Port > 0 {
		return runpodSSHEndpoint{Host: direct.Host, Port: direct.Port, Kind: "public-tcp", Public: true}
	}
	return runpodSSHEndpoint{}
}

type runpodDeployInput struct {
	Env               map[string]string
	Name              string
	ImageName         string
	InstanceID        string
	CloudType         string
	TemplateID        string
	ContainerDiskInGb int
	Ports             string
	PublicKey         string
}

func newRunpodClient(cfg core.Config, rt core.Runtime) (runpodAPI, error) {
	apiKey := strings.TrimSpace(cfg.Runpod.APIKey)
	if apiKey == "" {
		return nil, core.Exit(2, "provider=%s requires RUNPOD_API_KEY", providerName)
	}
	apiURL := strings.TrimRight(strings.TrimSpace(cfg.Runpod.APIURL), "/")
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, core.Exit(2, "%s url %q is invalid", providerName, apiURL)
	}
	if (strings.EqualFold(parsed.Hostname(), "rest.runpod.io") || strings.EqualFold(parsed.Hostname(), "api.runpod.io")) && parsed.Path == "/v1" {
		return nil, core.Exit(2, "RunPod REST v1 retires on 2026-11-15; remove the API URL override or set it to %s (fixed leases bound to the old endpoint require manual recovery)", core.RunpodConfigDefaultAPIURL)
	}
	if parsed.Scheme != "https" && !shared.IsLoopbackHTTPURL(parsed) {
		return nil, core.Exit(2, "%s url %q must use https unless it targets localhost", providerName, apiURL)
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &runpodClient{apiKey: apiKey, apiURL: apiURL, httpClient: httpClient}, nil
}

func runpodRedirectError(destination *url.URL) error {
	return fmt.Errorf("%s refused cross-origin redirect to %s", providerName, destination.Redacted())
}

func (c *runpodClient) do(ctx context.Context, method, path string, body any, out any) error {
	req, err := shared.NewCompactJSONRequest(ctx, method, c.apiURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := shared.SecureHTTPClient(c.httpClient, req.URL, runpodRedirectError).Do(req)
	if err != nil {
		return shared.ErrorWithMessage(shared.RedactErrorSecrets(err.Error(), c.apiKey, url.QueryEscape(c.apiKey)), err)
	}
	return shared.DecodeBoundedJSONResponse(resp, runpodMaxResponseBytes, out, providerName, func(code int, status, body string) error {
		apiErr := &runpodAPIError{StatusCode: code, Status: status, Body: shared.RedactErrorSecrets(body, c.apiKey)}
		var problem struct {
			Title  string   `json:"title"`
			Detail string   `json:"detail"`
			Errors []string `json:"errors"`
		}
		if json.Unmarshal([]byte(body), &problem) == nil && (problem.Title != "" || problem.Detail != "") {
			message := strings.TrimSpace(problem.Title + ": " + problem.Detail)
			if len(problem.Errors) > 0 {
				message += "; " + strings.Join(problem.Errors, "; ")
			}
			apiErr.Body = shared.RedactErrorSecrets(message, c.apiKey)
		}
		return apiErr
	})
}

func (c *runpodClient) Whoami(ctx context.Context) (runpodMyself, error) {
	identity := *c
	identity.apiURL = strings.TrimSuffix(strings.TrimSuffix(c.apiURL, "/v2"), "/v1")
	if c.apiURL == core.RunpodConfigDefaultAPIURL {
		identity.apiURL = "https://api.runpod.io"
	}
	var result struct {
		Data struct {
			Myself runpodMyself `json:"myself"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := identity.do(ctx, http.MethodPost, "/graphql", map[string]string{"query": "query { myself { id } }"}, &result); err != nil {
		return runpodMyself{}, err
	}
	if len(result.Errors) != 0 || strings.TrimSpace(result.Data.Myself.ID) == "" {
		return runpodMyself{}, core.Exit(2, "RunPod GraphQL account identity is missing or unavailable")
	}
	return result.Data.Myself, nil
}

func runpodInstanceIDs(instanceID string) []string {
	parts := strings.FieldsFunc(instanceID, func(r rune) bool {
		return r == ',' || r == '\n'
	})
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		if id := strings.TrimSpace(part); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return []string{strings.TrimSpace(instanceID)}
	}
	return ids
}

func runpodDeployPayload(input runpodDeployInput) map[string]any {
	payload := map[string]any{
		"name":     input.Name,
		"image":    input.ImageName,
		"cloud":    input.CloudType,
		"disk":     input.ContainerDiskInGb,
		"ports":    []string{input.Ports},
		"startSsh": true,
	}
	if input.CloudType == "" {
		payload["cloud"] = "SECURE"
	}
	if input.TemplateID != "" {
		payload["templateId"] = input.TemplateID
	}
	env := maps.Clone(input.Env)
	if publicKey := strings.TrimSpace(input.PublicKey); publicKey != "" {
		if env == nil {
			env = map[string]string{}
		}
		env["PUBLIC_KEY"] = publicKey
	}
	if len(env) != 0 {
		payload["env"] = env
	}
	instanceIDs := runpodInstanceIDs(input.InstanceID)
	if strings.HasPrefix(strings.ToLower(instanceIDs[0]), "cpu") {
		flavor, count, hasCount := strings.Cut(instanceIDs[0], "-")
		vcpus := 2
		if hasCount {
			// Validation in deployPod ensures this suffix is a power-of-two count.
			vcpus, _ = strconv.Atoi(count)
		}
		payload["cpu"] = map[string]any{"id": flavor, "vcpuCount": vcpus}
		return payload
	}
	payload["gpu"] = map[string]any{"id": instanceIDs[0], "count": 1}
	// v1 implicitly supplied this volume; preserve the existing GPU storage contract.
	payload["mounts"] = map[string]any{"persistent": map[string]any{"size": 20, "path": "/workspace"}}
	return payload
}

func (c *runpodClient) DeployPod(ctx context.Context, input runpodDeployInput) (runpodPod, error) {
	instanceIDs := runpodInstanceIDs(input.InstanceID)
	if len(instanceIDs) > 1 && !strings.HasPrefix(strings.ToLower(instanceIDs[0]), "cpu") {
		var capacityErr error
		for _, instanceID := range instanceIDs {
			attempt := input
			attempt.InstanceID = instanceID
			pod, err := c.deployPod(ctx, attempt)
			if err == nil {
				return pod, nil
			}
			if isRunpodCapacityError(err) {
				capacityErr = err
				continue
			}
			return runpodPod{}, err
		}
		if capacityErr != nil {
			return runpodPod{}, capacityErr
		}
	}
	return c.deployPod(ctx, input)
}

func (c *runpodClient) deployPod(ctx context.Context, input runpodDeployInput) (runpodPod, error) {
	ids := runpodInstanceIDs(input.InstanceID)
	if strings.HasPrefix(strings.ToLower(ids[0]), "cpu") {
		_, count, hasCount := strings.Cut(ids[0], "-")
		if hasCount {
			n, err := strconv.Atoi(count)
			if err != nil || n < 2 || n&(n-1) != 0 {
				return runpodPod{}, fmt.Errorf("RunPod CPU instance must be a flavor ID or <flavor>-<power-of-two vCPU count>, minimum 2")
			}
		}
	}
	var pod runpodPod
	if err := c.do(ctx, http.MethodPost, "/pods", runpodDeployPayload(input), &pod); err != nil {
		return runpodPod{}, err
	}
	if strings.TrimSpace(pod.ID) == "" {
		return runpodPod{}, fmt.Errorf("create pod returned empty id")
	}
	return pod, nil
}

// Fixed creation submits once; an uncertain response must be reconciled first.
func (c *runpodClient) DeployFixedPod(ctx context.Context, input runpodDeployInput) (runpodPod, error) {
	return c.deployPod(ctx, input)
}

func isRunpodCapacityError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *runpodAPIError
	if ok := errors.As(err, &apiErr); !ok {
		return false
	}
	return apiErr.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(apiErr.Body), "no instances currently available")
}

func (c *runpodClient) GetPod(ctx context.Context, podID string) (runpodPod, error) {
	if strings.TrimSpace(podID) == "" {
		return runpodPod{}, fmt.Errorf("getPod: podId is required")
	}
	var pod runpodPod
	if err := c.do(ctx, http.MethodGet, "/pods/"+url.PathEscape(podID), nil, &pod); err != nil {
		return runpodPod{}, err
	}
	if strings.TrimSpace(pod.ID) == "" {
		return runpodPod{}, fmt.Errorf("pod %s not found", podID)
	}
	return pod, nil
}

func (c *runpodClient) ListPods(ctx context.Context) ([]runpodPod, error) {
	var pods []runpodPod
	cursor := ""
	seen := map[string]bool{}
	for {
		var page struct {
			Pods       *[]runpodPod `json:"pods"`
			Pagination *struct {
				NextCursor  *string `json:"nextCursor"`
				HasNextPage *bool   `json:"hasNextPage"`
			} `json:"pagination"`
		}
		path := "/pods?limit=1000"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		if page.Pods == nil || page.Pagination == nil || page.Pagination.HasNextPage == nil {
			return nil, fmt.Errorf("RunPod list response missing pods or pagination; refusing incomplete inventory")
		}
		for _, pod := range *page.Pods {
			if strings.TrimSpace(pod.ID) == "" || strings.TrimSpace(pod.Name) == "" {
				return nil, fmt.Errorf("RunPod list response contains a pod without identity; refusing incomplete inventory")
			}
		}
		pods = append(pods, (*page.Pods)...)
		if !*page.Pagination.HasNextPage {
			return pods, nil
		}
		if page.Pagination.NextCursor == nil || *page.Pagination.NextCursor == "" || seen[*page.Pagination.NextCursor] {
			return nil, fmt.Errorf("RunPod list response has missing or repeated next cursor; refusing incomplete inventory")
		}
		cursor = *page.Pagination.NextCursor
		seen[cursor] = true
	}
}

func (c *runpodClient) TerminatePod(ctx context.Context, podID string) error {
	if strings.TrimSpace(podID) == "" {
		return fmt.Errorf("terminatePod: podId is required")
	}
	return c.do(ctx, http.MethodDelete, "/pods/"+url.PathEscape(podID), nil, nil)
}
