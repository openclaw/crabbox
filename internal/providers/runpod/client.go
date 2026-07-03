package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

// runpodAPI is the minimal RunPod REST surface the provider needs. The
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
	apiURL := strings.TrimRight(strings.TrimSpace(blank(cfg.Runpod.APIURL, "https://rest.runpod.io/v1")), "/")
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
	return &runpodClient{apiKey: apiKey, apiURL: apiURL, httpClient: secureRunpodHTTPClient(httpClient, apiURL)}, nil
}

func secureRunpodHTTPClient(source *http.Client, apiURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(apiURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameRunpodOrigin(trusted, req.URL) {
			return fmt.Errorf("%s refused cross-origin redirect to %s", providerName, req.URL.Redacted())
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

func sameRunpodOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveRunpodPort(a) == effectiveRunpodPort(b)
}

func effectiveRunpodPort(value *url.URL) string {
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

func (c *runpodClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, runpodMaxResponseBytes+1))
	if readErr != nil {
		return readErr
	}
	if len(data) > runpodMaxResponseBytes {
		return fmt.Errorf("runpod response exceeds %d bytes", runpodMaxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &runpodAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: shared.RedactErrorSecrets(strings.TrimSpace(string(data)), c.apiKey)}
	}
	if out != nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode runpod data: %w", err)
		}
	}
	return nil
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
	var pods []runpodPod
	if err := c.do(ctx, http.MethodGet, "/pods", nil, &pods); err != nil {
		return runpodMyself{}, err
	}
	return runpodMyself{ID: "authenticated"}, nil
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
		"cloudType":         blank(input.CloudType, "SECURE"),
		"containerDiskInGb": input.ContainerDiskInGb,
		"ports":             []string{input.Ports},
		"supportPublicIp":   true,
	}
	if input.TemplateID != "" {
		payload["templateId"] = input.TemplateID
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

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
