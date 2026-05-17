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
	DeploymentLogs(ctx context.Context, deploymentID string, limit int) ([]string, error)
	LatestDeployment(ctx context.Context, projectID, environmentID, serviceID string) (railwayDeployment, error)
	StopDeployment(ctx context.Context, deploymentID string) error
	ListServices(ctx context.Context) ([]railwayService, error)
	GetService(ctx context.Context, serviceID string) (railwayService, error)
}

type railwayClient struct {
	apiToken   string
	apiURL     string
	httpClient *http.Client
}

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
	ID        string
	Status    string
	URL       string
	CreatedAt string
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
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return readErr
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
	// The deployment id is not always returned in the trigger payload; callers
	// resolve the active deployment via LatestDeployment afterwards. Surface the
	// raw response if it happens to be a string so tests can inspect it.
	var maybeID string
	if err := json.Unmarshal(out.EnvironmentTriggersDeploy, &maybeID); err == nil {
		return maybeID, nil
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
	return c.do(ctx, deploymentStopMutation, map[string]any{"id": deploymentID}, nil)
}

const projectsQuery = `query crabboxProjects {
  projects {
    edges {
      node {
        id
        name
        services {
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
	if err := c.do(ctx, projectsQuery, nil, &out); err != nil {
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
