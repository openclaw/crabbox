package railway

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

	"github.com/openclaw/crabbox/internal/providers/shared"
)

// railwayAPI is the minimal Railway GraphQL surface the provider needs.
//
// Railway has no synchronous exec endpoint, so the provider models a "sandbox"
// as a Railway service inside a project. Run redeploys the latest deployment
// via deploymentRedeploy and surfaces deployment logs; List enumerates
// services across visible projects; Status fetches the latest deployment for a
// service; Stop calls deploymentStop on that latest deployment.
type railwayAPI interface {
	TriggerDeploy(ctx context.Context, projectID, environmentID, serviceID string) (string, error)
	BuildLogs(ctx context.Context, deploymentID string, limit int) ([]string, error)
	DeploymentLogs(ctx context.Context, deploymentID string, limit int) ([]string, error)
	LatestDeployment(ctx context.Context, projectID, environmentID, serviceID string) (railwayDeployment, error)
	Deployment(ctx context.Context, deploymentID string) (railwayDeployment, error)
	StopDeployment(ctx context.Context, deploymentID string) error
	ListServices(ctx context.Context) ([]railwayService, error)
	GetService(ctx context.Context, serviceID string) (railwayService, error)
}

type railwayClient struct {
	apiToken   string
	apiURL     string
	httpClient *http.Client
}

const railwayMaxGraphQLResponseBytes = 16 << 20

type railwayAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *railwayAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

type railwayService struct {
	ID        string
	Name      string
	ProjectID string
}

type railwayDeployment struct {
	ID        string                  `json:"id"`
	Status    railwayDeploymentStatus `json:"status"`
	URL       string                  `json:"url"`
	CreatedAt string                  `json:"createdAt"`
}

// railwayDeploymentStatus mirrors Railway's GraphQL DeploymentStatus enum.
// Values: https://docs.railway.com (verified via docs.railway.com/integrations/api/manage-deployments).
// Terminal states represent a completed deployment (SUCCESS/SLEEPING/FAILED/CRASHED/REMOVED/SKIPPED);
// the remaining values are still progressing and must be polled.
type railwayDeploymentStatus string

const (
	railwayStatusQueued        railwayDeploymentStatus = "QUEUED"
	railwayStatusInitializing  railwayDeploymentStatus = "INITIALIZING"
	railwayStatusBuilding      railwayDeploymentStatus = "BUILDING"
	railwayStatusDeploying     railwayDeploymentStatus = "DEPLOYING"
	railwayStatusWaiting       railwayDeploymentStatus = "WAITING"
	railwayStatusNeedsApproval railwayDeploymentStatus = "NEEDS_APPROVAL"
	railwayStatusRemoving      railwayDeploymentStatus = "REMOVING"
	railwayStatusSleeping      railwayDeploymentStatus = "SLEEPING"
	railwayStatusSuccess       railwayDeploymentStatus = "SUCCESS"
	railwayStatusFailed        railwayDeploymentStatus = "FAILED"
	railwayStatusCrashed       railwayDeploymentStatus = "CRASHED"
	railwayStatusRemoved       railwayDeploymentStatus = "REMOVED"
	railwayStatusSkipped       railwayDeploymentStatus = "SKIPPED"
)

// UnmarshalJSON normalizes the incoming string (trim + upper-case) so callers
// can compare against the typed constants without worrying about whitespace or
// casing quirks in upstream responses.
func (s *railwayDeploymentStatus) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = railwayDeploymentStatus(strings.ToUpper(strings.TrimSpace(raw)))
	return nil
}

// Normalized returns the trim+upper-case form of s so ad-hoc string values
// (e.g. plumbed from configuration) compare equal to the typed constants.
func (s railwayDeploymentStatus) Normalized() railwayDeploymentStatus {
	return railwayDeploymentStatus(strings.ToUpper(strings.TrimSpace(string(s))))
}

// IsTerminal reports whether the deployment has reached a final state and will
// not transition further without a new trigger. Non-terminal statuses must be
// polled.
func (s railwayDeploymentStatus) IsTerminal() bool {
	switch s.Normalized() {
	case railwayStatusSuccess,
		railwayStatusSleeping,
		railwayStatusFailed,
		railwayStatusCrashed,
		railwayStatusRemoved,
		railwayStatusSkipped:
		return true
	}
	return false
}

// IsReady reports whether the latest deployment represents a usable service.
func (s railwayDeploymentStatus) IsReady() bool {
	switch s.Normalized() {
	case railwayStatusSuccess, railwayStatusSleeping:
		return true
	}
	return false
}

// State returns the generic Crabbox status state. Railway has several terminal
// failure statuses; expose them as "failed" so status --wait can stop promptly.
func (s railwayDeploymentStatus) State() string {
	switch s.Normalized() {
	case railwayStatusFailed, railwayStatusCrashed, railwayStatusRemoved, railwayStatusSkipped:
		return "failed"
	default:
		return strings.ToLower(string(s.Normalized()))
	}
}

// ExitCode maps a terminal deployment status to a process exit code: 0 for a
// usable deployment, 1 for every other terminal state. Non-terminal statuses
// also map to 1 because they only reach this function when the polling loop
// bails out (for example on context cancellation).
func (s railwayDeploymentStatus) ExitCode() int {
	if s.IsReady() {
		return 0
	}
	return 1
}

func newRailwayClient(cfg Config, rt Runtime) (railwayAPI, error) {
	apiToken := strings.TrimSpace(cfg.Railway.APIToken)
	if apiToken == "" {
		return nil, exit(2, "provider=%s requires RAILWAY_API_TOKEN", providerName)
	}
	apiURL := strings.TrimRight(strings.TrimSpace(blank(cfg.Railway.APIURL, "https://backboard.railway.com/graphql/v2")), "/")
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
	return &railwayClient{apiToken: apiToken, apiURL: apiURL, httpClient: secureRailwayHTTPClient(httpClient, apiURL)}, nil
}

func secureRailwayHTTPClient(source *http.Client, apiURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(apiURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameRailwayOrigin(trusted, req.URL) {
			return &railwayRedirectError{origin: railwayRedirectOrigin(req.URL)}
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

func sameRailwayOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveRailwayPort(a) == effectiveRailwayPort(b)
}

func effectiveRailwayPort(value *url.URL) string {
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

type railwayRedirectError struct {
	origin string
}

func (e *railwayRedirectError) Error() string {
	return fmt.Sprintf("%s refused cross-origin redirect to %s", providerName, e.origin)
}

func railwayRedirectOrigin(value *url.URL) string {
	if value == nil || value.Scheme == "" || value.Host == "" {
		return "<redacted>"
	}
	return value.Scheme + "://" + value.Host
}

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphqlError struct {
	Message string `json:"message"`
}

type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors,omitempty"`
}

func (c *railwayClient) do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(graphqlRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// net/http wraps CheckRedirect failures with the untrusted Location URL.
		var redirectErr *railwayRedirectError
		if errors.As(err, &redirectErr) {
			return redirectErr
		}
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, railwayMaxGraphQLResponseBytes+1))
	if readErr != nil {
		return readErr
	}
	if len(data) > railwayMaxGraphQLResponseBytes {
		return fmt.Errorf("railway response exceeds %d bytes", railwayMaxGraphQLResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &railwayAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: shared.RedactErrorSecrets(strings.TrimSpace(string(data)), c.apiToken)}
	}
	var envelope graphqlResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode railway response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return &railwayAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: shared.RedactErrorSecrets(strings.Join(msgs, "; "), c.apiToken)}
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode railway data: %w", err)
		}
	}
	return nil
}

const redeployMutation = `mutation crabboxRedeploy($id: String!, $usePreviousImageTag: Boolean) {
  deploymentRedeploy(id: $id, usePreviousImageTag: $usePreviousImageTag) {
    id
    status
    url
    createdAt
  }
}`

func (c *railwayClient) TriggerDeploy(ctx context.Context, projectID, environmentID, serviceID string) (string, error) {
	if projectID == "" || environmentID == "" || serviceID == "" {
		return "", fmt.Errorf("triggerDeploy: projectId, environmentId, and serviceId are required")
	}
	latest, err := c.LatestDeployment(ctx, projectID, environmentID, serviceID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(latest.ID) == "" {
		return "", fmt.Errorf("triggerDeploy: latest deployment not found for service %s", serviceID)
	}
	var out struct {
		DeploymentRedeploy railwayDeployment `json:"deploymentRedeploy"`
	}
	if err := c.do(ctx, redeployMutation, map[string]any{"id": latest.ID, "usePreviousImageTag": true}, &out); err != nil {
		return "", err
	}
	deploymentID := strings.TrimSpace(out.DeploymentRedeploy.ID)
	if deploymentID == "" {
		return "", fmt.Errorf("deploymentRedeploy returned empty deployment id")
	}
	return deploymentID, nil
}

const latestDeploymentQuery = `query crabboxLatestDeployment($input: DeploymentListInput!) {
  deployments(input: $input, first: 1) {
    edges {
      node {
        id
        status
        url
        createdAt
      }
    }
  }
}`

// deploymentQuery fetches a single deployment by ID. Verified against
// docs.railway.com/integrations/api/manage-deployments: the GraphQL schema
// exposes `deployment(id: String!): Deployment`.
const deploymentQuery = `query crabboxDeployment($id: String!) {
  deployment(id: $id) {
    id
    status
    url
    createdAt
  }
}`

func (c *railwayClient) Deployment(ctx context.Context, deploymentID string) (railwayDeployment, error) {
	if strings.TrimSpace(deploymentID) == "" {
		return railwayDeployment{}, fmt.Errorf("deployment: deploymentId is required")
	}
	var out struct {
		Deployment railwayDeployment `json:"deployment"`
	}
	if err := c.do(ctx, deploymentQuery, map[string]any{"id": deploymentID}, &out); err != nil {
		return railwayDeployment{}, err
	}
	if out.Deployment.ID == "" {
		return railwayDeployment{}, fmt.Errorf("deployment %s not found", deploymentID)
	}
	return out.Deployment, nil
}

func (c *railwayClient) LatestDeployment(ctx context.Context, projectID, environmentID, serviceID string) (railwayDeployment, error) {
	vars := map[string]any{
		"input": map[string]any{
			"projectId":     projectID,
			"environmentId": environmentID,
			"serviceId":     serviceID,
		},
	}
	var out struct {
		Deployments struct {
			Edges []struct {
				Node railwayDeployment `json:"node"`
			} `json:"edges"`
		} `json:"deployments"`
	}
	if err := c.do(ctx, latestDeploymentQuery, vars, &out); err != nil {
		return railwayDeployment{}, err
	}
	if len(out.Deployments.Edges) == 0 {
		return railwayDeployment{}, nil
	}
	return out.Deployments.Edges[0].Node, nil
}

const deploymentLogsQuery = `query crabboxDeploymentLogs($deploymentId: String!, $limit: Int) {
  deploymentLogs(deploymentId: $deploymentId, limit: $limit) {
    message
  }
}`

const buildLogsQuery = `query crabboxBuildLogs($deploymentId: String!, $limit: Int) {
  buildLogs(deploymentId: $deploymentId, limit: $limit) {
    message
  }
}`

func (c *railwayClient) BuildLogs(ctx context.Context, deploymentID string, limit int) ([]string, error) {
	if deploymentID == "" {
		return nil, fmt.Errorf("buildLogs: deploymentId is required")
	}
	if limit <= 0 {
		limit = 500
	}
	vars := map[string]any{
		"deploymentId": deploymentID,
		"limit":        limit,
	}
	var out struct {
		BuildLogs []struct {
			Message string `json:"message"`
		} `json:"buildLogs"`
	}
	if err := c.do(ctx, buildLogsQuery, vars, &out); err != nil {
		return nil, err
	}
	messages := make([]string, 0, len(out.BuildLogs))
	for _, l := range out.BuildLogs {
		messages = append(messages, l.Message)
	}
	return messages, nil
}

func (c *railwayClient) DeploymentLogs(ctx context.Context, deploymentID string, limit int) ([]string, error) {
	if deploymentID == "" {
		return nil, fmt.Errorf("deploymentLogs: deploymentId is required")
	}
	if limit <= 0 {
		limit = 500
	}
	vars := map[string]any{
		"deploymentId": deploymentID,
		"limit":        limit,
	}
	var out struct {
		DeploymentLogs []struct {
			Message string `json:"message"`
		} `json:"deploymentLogs"`
	}
	if err := c.do(ctx, deploymentLogsQuery, vars, &out); err != nil {
		return nil, err
	}
	messages := make([]string, 0, len(out.DeploymentLogs))
	for _, l := range out.DeploymentLogs {
		messages = append(messages, l.Message)
	}
	return messages, nil
}

const deploymentStopMutation = `mutation crabboxDeploymentStop($id: String!) {
  deploymentStop(id: $id)
}`

func (c *railwayClient) StopDeployment(ctx context.Context, deploymentID string) error {
	if deploymentID == "" {
		return fmt.Errorf("stopDeployment: deploymentId is required")
	}
	var out struct {
		DeploymentStop bool `json:"deploymentStop"`
	}
	if err := c.do(ctx, deploymentStopMutation, map[string]any{"id": deploymentID}, &out); err != nil {
		return err
	}
	if !out.DeploymentStop {
		return fmt.Errorf("deploymentStop returned false")
	}
	return nil
}

// railwayListServicesPageSize bounds each Railway GraphQL connection page so a
// long-lived token does not pull megabytes of unrelated metadata in one call.
const railwayListServicesPageSize = 50

const projectsQuery = `query crabboxProjects($first: Int!, $after: String, $serviceFirst: Int!) {
  projects(first: $first, after: $after) {
    pageInfo {
      hasNextPage
      endCursor
    }
    edges {
      node {
        id
        name
        services(first: $serviceFirst) {
          pageInfo {
            hasNextPage
            endCursor
          }
          edges {
            node {
              id
              name
            }
          }
        }
      }
    }
  }
}`

const projectServicesQuery = `query crabboxProjectServices($projectId: String!, $first: Int!, $after: String) {
  project(id: $projectId) {
    services(first: $first, after: $after) {
      pageInfo {
        hasNextPage
        endCursor
      }
      edges {
        node {
          id
          name
        }
      }
    }
  }
}`

type railwayPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type railwayServiceConnection struct {
	PageInfo railwayPageInfo `json:"pageInfo"`
	Edges    []struct {
		Node struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"node"`
	} `json:"edges"`
}

func (c *railwayClient) ListServices(ctx context.Context) ([]railwayService, error) {
	var services []railwayService
	var projectAfter string
	for {
		var out struct {
			Projects struct {
				PageInfo railwayPageInfo `json:"pageInfo"`
				Edges    []struct {
					Node struct {
						ID       string                   `json:"id"`
						Name     string                   `json:"name"`
						Services railwayServiceConnection `json:"services"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"projects"`
		}
		vars := map[string]any{
			"first":        railwayListServicesPageSize,
			"serviceFirst": railwayListServicesPageSize,
		}
		if projectAfter != "" {
			vars["after"] = projectAfter
		}
		if err := c.do(ctx, projectsQuery, vars, &out); err != nil {
			return nil, err
		}
		for _, p := range out.Projects.Edges {
			services = appendRailwayServices(services, p.Node.ID, p.Node.Services)
			if p.Node.Services.PageInfo.HasNextPage {
				if p.Node.Services.PageInfo.EndCursor == "" {
					return nil, fmt.Errorf("railway services pagination for project %s missing endCursor", p.Node.ID)
				}
				more, err := c.listProjectServices(ctx, p.Node.ID, p.Node.Services.PageInfo.EndCursor)
				if err != nil {
					return nil, err
				}
				services = append(services, more...)
			}
		}
		if !out.Projects.PageInfo.HasNextPage {
			return services, nil
		}
		if out.Projects.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("railway projects pagination missing endCursor")
		}
		if out.Projects.PageInfo.EndCursor == projectAfter {
			return nil, fmt.Errorf("railway projects pagination did not advance")
		}
		projectAfter = out.Projects.PageInfo.EndCursor
	}
}

func (c *railwayClient) listProjectServices(ctx context.Context, projectID, after string) ([]railwayService, error) {
	var services []railwayService
	for after != "" {
		var out struct {
			Project *struct {
				Services railwayServiceConnection `json:"services"`
			} `json:"project"`
		}
		vars := map[string]any{
			"projectId": projectID,
			"first":     railwayListServicesPageSize,
			"after":     after,
		}
		if err := c.do(ctx, projectServicesQuery, vars, &out); err != nil {
			return nil, err
		}
		if out.Project == nil {
			return nil, fmt.Errorf("railway project %s not found while paginating services", projectID)
		}
		services = appendRailwayServices(services, projectID, out.Project.Services)
		if !out.Project.Services.PageInfo.HasNextPage {
			return services, nil
		}
		next := out.Project.Services.PageInfo.EndCursor
		if next == "" {
			return nil, fmt.Errorf("railway services pagination for project %s missing endCursor", projectID)
		}
		if next == after {
			return nil, fmt.Errorf("railway services pagination for project %s did not advance", projectID)
		}
		after = next
	}
	return services, nil
}

func appendRailwayServices(dst []railwayService, projectID string, conn railwayServiceConnection) []railwayService {
	for _, s := range conn.Edges {
		dst = append(dst, railwayService{
			ID:        s.Node.ID,
			Name:      s.Node.Name,
			ProjectID: projectID,
		})
	}
	return dst
}

const serviceQuery = `query crabboxService($id: String!) {
  service(id: $id) {
    id
    name
    projectId
  }
}`

func (c *railwayClient) GetService(ctx context.Context, serviceID string) (railwayService, error) {
	if serviceID == "" {
		return railwayService{}, fmt.Errorf("getService: serviceId is required")
	}
	var out struct {
		Service struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			ProjectID string `json:"projectId"`
		} `json:"service"`
	}
	if err := c.do(ctx, serviceQuery, map[string]any{"id": serviceID}, &out); err != nil {
		return railwayService{}, err
	}
	if out.Service.ID == "" {
		return railwayService{}, fmt.Errorf("service %s not found", serviceID)
	}
	return railwayService{ID: out.Service.ID, Name: out.Service.Name, ProjectID: out.Service.ProjectID}, nil
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
