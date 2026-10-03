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
// publicIp + portMappings["22"] once the pod reaches RUNNING.
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
	Env map[string]string `json:"env"`

	ID            string         `json:"id"`
	Name          string         `json:"name"`
	ImageName     string         `json:"imageName"`
	Image         string         `json:"image"`
	DesiredStatus string         `json:"desiredStatus"`
	Status        string         `json:"status"`
	MachineID     string         `json:"machineId"`
	Machine       runpodMachine  `json:"machine"`
	Runtime       *runpodRuntime `json:"runtime"`
	PublicIP      string         `json:"publicIp"`
	PortMappings  map[string]int `json:"portMappings"`
	CostPerHr     any            `json:"costPerHr"`
}

// SSHEndpoint returns the best SSH endpoint available for the pod.
// Crabbox requires a public TCP port because RunPod's basic SSH proxy does not
// support the SCP/SFTP behavior rsync needs.
func (p runpodPod) SSHEndpoint() runpodSSHEndpoint {
	if port := p.PortMappings["22"]; p.PublicIP != "" && port != 0 {
		return runpodSSHEndpoint{Host: p.PublicIP, Port: port, Kind: "public-tcp", Public: true}
	}
	if p.Runtime == nil {
		return runpodSSHEndpoint{}
	}
	for _, prt := range p.Runtime.Ports {
		if prt.PrivatePort == 22 && prt.IsIPPublic && prt.IP != "" && prt.PublicPort != 0 && strings.EqualFold(prt.Type, "tcp") {
			return runpodSSHEndpoint{Host: prt.IP, Port: prt.PublicPort, Kind: "public-tcp", Public: true}
		}
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
		return &runpodAPIError{StatusCode: code, Status: status, Body: shared.RedactErrorSecrets(body, c.apiKey)}
	})
}

func normalizeRunpodPod(pod runpodPod) runpodPod {
	if pod.ImageName == "" {
		pod.ImageName = pod.Image
	}
	if pod.DesiredStatus == "" {
		pod.DesiredStatus = pod.Status
	}
	if pod.PortMappings == nil {
		pod.PortMappings = map[string]int{}
	}
	return pod
}

func (c *runpodClient) Whoami(ctx context.Context) (runpodMyself, error) {
	identity := *c
	identity.apiURL = strings.TrimSuffix(c.apiURL, "/v1")
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

func cpuFlavorID(instanceID string) string {
	if idx := strings.Index(instanceID, "-"); idx > 0 {
		return instanceID[:idx]
	}
	return instanceID
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
		"name":              input.Name,
		"imageName":         input.ImageName,
		"cloudType":         input.CloudType,
		"containerDiskInGb": input.ContainerDiskInGb,
		"ports":             []string{input.Ports},
		"supportPublicIp":   true,
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
		payload["computeType"] = "CPU"
		payload["cpuFlavorIds"] = []string{cpuFlavorID(instanceIDs[0])}
		payload["cpuFlavorPriority"] = "availability"
		return payload
	}
	payload["computeType"] = "GPU"
	payload["gpuTypeIds"] = instanceIDs
	payload["gpuTypePriority"] = "availability"
	payload["gpuCount"] = 1
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
	var pod runpodPod
	if err := c.do(ctx, http.MethodPost, "/pods", runpodDeployPayload(input), &pod); err != nil {
		return runpodPod{}, err
	}
	pod = normalizeRunpodPod(pod)
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
	return apiErr.StatusCode == http.StatusInternalServerError && strings.Contains(strings.ToLower(apiErr.Body), "no instances currently available")
}

func (c *runpodClient) GetPod(ctx context.Context, podID string) (runpodPod, error) {
	if strings.TrimSpace(podID) == "" {
		return runpodPod{}, fmt.Errorf("getPod: podId is required")
	}
	var pod runpodPod
	if err := c.do(ctx, http.MethodGet, "/pods/"+url.PathEscape(podID), nil, &pod); err != nil {
		return runpodPod{}, err
	}
	pod = normalizeRunpodPod(pod)
	if strings.TrimSpace(pod.ID) == "" {
		return runpodPod{}, fmt.Errorf("pod %s not found", podID)
	}
	return pod, nil
}

func (c *runpodClient) ListPods(ctx context.Context) ([]runpodPod, error) {
	var pods []runpodPod
	if err := c.do(ctx, http.MethodGet, "/pods", nil, &pods); err != nil {
		return nil, err
	}
	for i := range pods {
		pods[i] = normalizeRunpodPod(pods[i])
	}
	return pods, nil
}

func (c *runpodClient) TerminatePod(ctx context.Context, podID string) error {
	if strings.TrimSpace(podID) == "" {
		return fmt.Errorf("terminatePod: podId is required")
	}
	return c.do(ctx, http.MethodDelete, "/pods/"+url.PathEscape(podID), nil, nil)
}

func decodePortMappings(raw map[string]any) map[string]int {
	ports := make(map[string]int, len(raw))
	for key, value := range raw {
		switch v := value.(type) {
		case float64:
			ports[key] = int(v)
		case int:
			ports[key] = v
		case string:
			if parsed, err := strconv.Atoi(v); err == nil {
				ports[key] = parsed
			}
		}
	}
	return ports
}

func (p *runpodPod) UnmarshalJSON(data []byte) error {
	var aux struct {
		Env map[string]string `json:"env"`

		ID            string         `json:"id"`
		Name          string         `json:"name"`
		ImageName     string         `json:"imageName"`
		Image         string         `json:"image"`
		DesiredStatus string         `json:"desiredStatus"`
		Status        string         `json:"status"`
		MachineID     string         `json:"machineId"`
		Machine       runpodMachine  `json:"machine"`
		Runtime       *runpodRuntime `json:"runtime"`
		PublicIP      string         `json:"publicIp"`
		PortMappings  map[string]any `json:"portMappings"`
		CostPerHr     any            `json:"costPerHr"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return fmt.Errorf("decode runpod response: %w", err)
	}
	*p = runpodPod{
		Env:           aux.Env,
		ID:            aux.ID,
		Name:          aux.Name,
		ImageName:     aux.ImageName,
		Image:         aux.Image,
		DesiredStatus: aux.DesiredStatus,
		Status:        aux.Status,
		MachineID:     aux.MachineID,
		Machine:       aux.Machine,
		Runtime:       aux.Runtime,
		PublicIP:      aux.PublicIP,
		CostPerHr:     aux.CostPerHr,
	}
	p.PortMappings = decodePortMappings(aux.PortMappings)
	return nil
}
