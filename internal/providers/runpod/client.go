package runpod

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

// runpodAPI is the minimal RunPod GraphQL surface the provider needs. The
// provider keeps the surface intentionally small: deploy a CPU pod, look it
// up, list pods, and terminate it. The pod's SSH coordinates come back via
// the pod query's runtime.ports[] structure once the pod is in RUNNING state.
type runpodAPI interface {
	Whoami(ctx context.Context) (runpodMyself, error)
	DeployCpuPod(ctx context.Context, input runpodDeployInput) (runpodPod, error)
	GetPod(ctx context.Context, podID string) (runpodPod, error)
	ListPods(ctx context.Context) ([]runpodPod, error)
	TerminatePod(ctx context.Context, podID string) error
}

type runpodClient struct {
	apiKey     string
	apiURL     string
	httpClient *http.Client
}

const runpodMaxGraphQLResponseBytes = 16 << 20

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

type runpodMachine struct {
	PodHostID string `json:"podHostId"`
}

type runpodRuntimePort struct {
	IP          string `json:"ip"`
	PrivatePort int    `json:"privatePort"`
	PublicPort  int    `json:"publicPort"`
	IsIPPublic  bool   `json:"isIpPublic"`
	Type        string `json:"type"`
}

type runpodRuntime struct {
	Ports []runpodRuntimePort `json:"ports"`
}

type runpodPod struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	ImageName     string         `json:"imageName"`
	DesiredStatus string         `json:"desiredStatus"`
	MachineID     string         `json:"machineId"`
	Machine       runpodMachine  `json:"machine"`
	Runtime       *runpodRuntime `json:"runtime"`
	CostPerHr     any            `json:"costPerHr"`
}

// SSHEndpoint returns the public TCP host:port mapping for the pod's port 22,
// or empty values if the pod has not exposed an SSH endpoint yet.
func (p runpodPod) SSHEndpoint() (host string, port int) {
	if p.Runtime == nil {
		return "", 0
	}
	for _, prt := range p.Runtime.Ports {
		if prt.PrivatePort == 22 && prt.IsIPPublic && prt.IP != "" && prt.PublicPort != 0 && strings.EqualFold(prt.Type, "tcp") {
			return prt.IP, prt.PublicPort
		}
	}
	return "", 0
}

type runpodDeployInput struct {
	Name              string
	ImageName         string
	InstanceID        string
	CloudType         string
	TemplateID        string
	ContainerDiskInGb int
	Ports             string
	StartSSH          bool
}

func newRunpodClient(cfg Config, rt Runtime) (runpodAPI, error) {
	apiKey := strings.TrimSpace(cfg.Runpod.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=%s requires RUNPOD_API_KEY", providerName)
	}
	apiURL := strings.TrimRight(strings.TrimSpace(blank(cfg.Runpod.APIURL, "https://api.runpod.io/graphql")), "/")
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
	return &runpodClient{apiKey: apiKey, apiURL: apiURL, httpClient: httpClient}, nil
}

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphqlError struct {
	Message    string         `json:"message"`
	Path       []any          `json:"path,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors,omitempty"`
}

func (c *runpodClient) do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(graphqlRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, runpodMaxGraphQLResponseBytes+1))
	if readErr != nil {
		return readErr
	}
	if len(data) > runpodMaxGraphQLResponseBytes {
		return fmt.Errorf("runpod response exceeds %d bytes", runpodMaxGraphQLResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &runpodAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(data))}
	}
	var envelope graphqlResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode runpod response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return &runpodAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: strings.Join(msgs, "; ")}
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode runpod data: %w", err)
		}
	}
	return nil
}

const myselfQuery = `query crabboxRunpodMyself {
  myself {
    id
    email
    clientBalance
    signedTermsOfService
  }
}`

func (c *runpodClient) Whoami(ctx context.Context) (runpodMyself, error) {
	var out struct {
		Myself runpodMyself `json:"myself"`
	}
	if err := c.do(ctx, myselfQuery, nil, &out); err != nil {
		return runpodMyself{}, err
	}
	if strings.TrimSpace(out.Myself.ID) == "" {
		return runpodMyself{}, fmt.Errorf("runpod myself query returned empty id")
	}
	return out.Myself, nil
}

// deployCpuPodMutation deploys a CPU pod. Verified against the runpod-python
// SDK's pod_mutations.generate_pod_deployment_mutation source: the upstream
// SDK accepts `instanceId`, `cloudType`, `startSsh`, `templateId`,
// `containerDiskInGb`, and `ports`. SSH is requested via startSsh + exposing
// port 22/tcp; the pod query later reports the public mapping in
// runtime.ports[].publicPort.
const deployCpuPodMutation = `mutation crabboxRunpodDeployCpuPod($input: deployCpuPodInput!) {
  deployCpuPod(input: $input) {
    id
    imageName
    machineId
    desiredStatus
    machine {
      podHostId
    }
  }
}`

func (c *runpodClient) DeployCpuPod(ctx context.Context, input runpodDeployInput) (runpodPod, error) {
	payload := map[string]any{
		"name":              input.Name,
		"imageName":         input.ImageName,
		"instanceId":        input.InstanceID,
		"cloudType":         input.CloudType,
		"startSsh":          input.StartSSH,
		"containerDiskInGb": input.ContainerDiskInGb,
		"ports":             input.Ports,
	}
	if strings.TrimSpace(input.TemplateID) != "" {
		payload["templateId"] = input.TemplateID
	}
	var out struct {
		DeployCpuPod runpodPod `json:"deployCpuPod"`
	}
	if err := c.do(ctx, deployCpuPodMutation, map[string]any{"input": payload}, &out); err != nil {
		return runpodPod{}, err
	}
	if strings.TrimSpace(out.DeployCpuPod.ID) == "" {
		return runpodPod{}, fmt.Errorf("deployCpuPod returned empty pod id")
	}
	return out.DeployCpuPod, nil
}

// podQuery fetches a single pod by ID with the runtime.ports[] field needed to
// recover the public SSH endpoint. The runtime field is null until the pod
// reaches RUNNING.
const podQuery = `query crabboxRunpodPod($input: PodFilter!) {
  pod(input: $input) {
    id
    name
    imageName
    desiredStatus
    machineId
    costPerHr
    machine {
      podHostId
    }
    runtime {
      ports {
        ip
        privatePort
        publicPort
        isIpPublic
        type
      }
    }
  }
}`

func (c *runpodClient) GetPod(ctx context.Context, podID string) (runpodPod, error) {
	if strings.TrimSpace(podID) == "" {
		return runpodPod{}, fmt.Errorf("getPod: podId is required")
	}
	var out struct {
		Pod runpodPod `json:"pod"`
	}
	if err := c.do(ctx, podQuery, map[string]any{"input": map[string]any{"podId": podID}}, &out); err != nil {
		return runpodPod{}, err
	}
	if strings.TrimSpace(out.Pod.ID) == "" {
		return runpodPod{}, fmt.Errorf("pod %s not found", podID)
	}
	return out.Pod, nil
}

const listPodsQuery = `query crabboxRunpodPods {
  myself {
    pods {
      id
      name
      imageName
      desiredStatus
      machineId
      costPerHr
      machine {
        podHostId
      }
      runtime {
        ports {
          ip
          privatePort
          publicPort
          isIpPublic
          type
        }
      }
    }
  }
}`

func (c *runpodClient) ListPods(ctx context.Context) ([]runpodPod, error) {
	var out struct {
		Myself struct {
			Pods []runpodPod `json:"pods"`
		} `json:"myself"`
	}
	if err := c.do(ctx, listPodsQuery, nil, &out); err != nil {
		return nil, err
	}
	return out.Myself.Pods, nil
}

const terminatePodMutation = `mutation crabboxRunpodTerminate($input: PodTerminateInput!) {
  podTerminate(input: $input)
}`

func (c *runpodClient) TerminatePod(ctx context.Context, podID string) error {
	if strings.TrimSpace(podID) == "" {
		return fmt.Errorf("terminatePod: podId is required")
	}
	if err := c.do(ctx, terminatePodMutation, map[string]any{"input": map[string]any{"podId": podID}}, nil); err != nil {
		return err
	}
	return nil
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
