package railway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// railwayAPI is the minimal Railway GraphQL surface the provider needs.
//
// Railway has no synchronous exec endpoint, so the provider models a "sandbox"
// as a Railway service inside a project. Run triggers a redeploy via
// environmentTriggersDeploy and surfaces deployment logs; List enumerates
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
	return &railwayClient{apiToken: apiToken, apiURL: apiURL, httpClient: httpClient}, nil
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
		return &railwayAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(data))}
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
		return &railwayAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: strings.Join(msgs, "; ")}
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode railway data: %w", err)
		}
	}
	return nil
}

const triggerDeployMutation = `mutation crabboxTriggerDeploy($input: EnvironmentTriggersDeployInput!) {
  environmentTriggersDeploy(input: $input)
}`

func (c *railwayClient) TriggerDeploy(ctx context.Context, projectID, environmentID, serviceID string) (string, error) {
	if projectID == "" || environmentID == "" || serviceID == "" {
		return "", fmt.Errorf("triggerDeploy: projectId, environmentId, and serviceId are required")
	}
	var out struct {
		EnvironmentTriggersDeploy json.RawMessage `json:"environmentTriggersDeploy"`
	}
	vars := map[string]any{
		"input": map[string]any{
			"projectId":     projectID,
			"environmentId": environmentID,
			"serviceId":     serviceID,
		},
	}
	if err := c.do(ctx, triggerDeployMutation, vars, &out); err != nil {
		return "", err
	}
	// Railway's environmentTriggersDeploy can return the new deployment id as a
	// bare string, wrap it in a Deployment object, or return a boolean success
	// marker. The boolean case falls back to LatestDeployment in Run().
	var maybeID string
	if err := json.Unmarshal(out.EnvironmentTriggersDeploy, &maybeID); err == nil {
		return strings.TrimSpace(maybeID), nil
	}
	var maybeObject struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.EnvironmentTriggersDeploy, &maybeObject); err == nil {
		return strings.TrimSpace(maybeObject.ID), nil
	}
	var maybeOK bool
	if err := json.Unmarshal(out.EnvironmentTriggersDeploy, &maybeOK); err == nil {
		if !maybeOK {
			return "", fmt.Errorf("environmentTriggersDeploy returned false")
		}
		return "", nil
	}
	return "", nil
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

// railwayListServicesPageSize bounds the number of projects (and the services
// inside each project) returned by ListServices so a long-lived token does not
// pull megabytes of unrelated metadata in a single call. Railway's GraphQL
// connections accept `first: Int` on the projects edge as well as on the
// nested services edge; if a future schema rev drops support there, the inner
// `first:` argument becomes inert and we fall back to a client-side cap below.
const railwayListServicesPageSize = 50

const projectsQuery = `query crabboxProjects($first: Int) {
  projects(first: $first) {
    edges {
      node {
        id
        name
        services(first: $first) {
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

func (c *railwayClient) ListServices(ctx context.Context) ([]railwayService, error) {
	var out struct {
		Projects struct {
			Edges []struct {
				Node struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					Services struct {
						Edges []struct {
							Node struct {
								ID   string `json:"id"`
								Name string `json:"name"`
							} `json:"node"`
						} `json:"edges"`
					} `json:"services"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"projects"`
	}
	if err := c.do(ctx, projectsQuery, map[string]any{"first": railwayListServicesPageSize}, &out); err != nil {
		return nil, err
	}
	var services []railwayService
	for _, p := range out.Projects.Edges {
		for _, s := range p.Node.Services.Edges {
			services = append(services, railwayService{
				ID:        s.Node.ID,
				Name:      s.Node.Name,
				ProjectID: p.Node.ID,
			})
			// Client-side cap: defends against a Railway schema rev that ignores
			// `first:` on the services connection, which would otherwise let a
			// single project flood the result.
			if len(services) >= railwayListServicesPageSize*railwayListServicesPageSize {
				return services, nil
			}
		}
	}
	return services, nil
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
