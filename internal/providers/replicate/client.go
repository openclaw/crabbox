package replicate

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
	"strings"
)

type replicateAPI interface {
	CreatePrediction(context.Context, replicateCreatePredictionRequest) (replicatePrediction, error)
	GetPrediction(context.Context, string) (replicatePrediction, error)
	ListPredictions(context.Context) (replicatePredictionList, error)
	CancelPrediction(context.Context, string) (replicatePrediction, error)
}

type replicateClient struct {
	http    *http.Client
	baseURL string
	token   string
}

type replicateCreatePredictionRequest struct {
	Deployment      string
	Version         string
	Input           map[string]any
	WaitSecs        int
	CancelAfterSecs int
}

type replicatePrediction struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	Logs        string          `json:"logs,omitempty"`
	Output      json.RawMessage `json:"output,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
	URLs        predictionURLs  `json:"urls,omitempty"`
	Metrics     map[string]any  `json:"metrics,omitempty"`
	CreatedAt   string          `json:"created_at,omitempty"`
	StartedAt   string          `json:"started_at,omitempty"`
	CompletedAt string          `json:"completed_at,omitempty"`
}

type predictionURLs struct {
	Get    string `json:"get,omitempty"`
	Cancel string `json:"cancel,omitempty"`
	Stream string `json:"stream,omitempty"`
}

type replicatePredictionList struct {
	Results []replicatePrediction `json:"results"`
	Next    string                `json:"next,omitempty"`
}

func newReplicateClient(cfg Config, rt Runtime) (*replicateClient, error) {
	baseURL, err := validateReplicateAPIURL(blank(strings.TrimSpace(cfg.Replicate.APIURL), defaultAPIURL))
	if err != nil {
		return nil, err
	}
	token, _, ok := ResolveAPIToken()
	if !ok {
		return nil, exit(2, "provider=replicate needs an API token; load CRABBOX_REPLICATE_API_TOKEN from a secret manager or set REPLICATE_API_TOKEN")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &replicateClient{http: secureReplicateHTTPClient(httpClient, baseURL), baseURL: baseURL, token: token}, nil
}

func validateReplicateAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=replicate API URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=replicate API URL must not contain userinfo, query parameters, or a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isReplicateLoopbackHost(parsed.Hostname())) {
		return "", exit(2, "provider=replicate API URL must use HTTPS except for loopback development endpoints")
	}
	host := canonicalReplicateHostname(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func canonicalReplicateHostname(host string) string {
	if zoneAt := strings.Index(host, "%"); zoneAt > 0 && strings.Contains(host[:zoneAt], ":") {
		return strings.ToLower(host[:zoneAt]) + host[zoneAt:]
	}
	return strings.ToLower(host)
}

func isReplicateLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func secureReplicateHTTPClient(source *http.Client, baseURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(baseURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameReplicateOrigin(trusted, req.URL) {
			return fmt.Errorf("replicate refused cross-origin redirect to %s", req.URL.Redacted())
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

func sameReplicateOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveReplicatePort(a) == effectiveReplicatePort(b)
}

func effectiveReplicatePort(value *url.URL) string {
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

func (c *replicateClient) CreatePrediction(ctx context.Context, req replicateCreatePredictionRequest) (replicatePrediction, error) {
	path, body, err := createPredictionPathAndBody(req)
	if err != nil {
		return replicatePrediction{}, err
	}
	headers := map[string]string{}
	if req.WaitSecs > 0 {
		headers["Prefer"] = fmt.Sprintf("wait=%d", req.WaitSecs)
	}
	if req.CancelAfterSecs > 0 {
		headers["Cancel-After"] = fmt.Sprintf("%ds", req.CancelAfterSecs)
	}
	var out replicatePrediction
	if err := c.doJSON(ctx, http.MethodPost, path, body, headers, &out); err != nil {
		return replicatePrediction{}, err
	}
	return out, nil
}

func createPredictionPathAndBody(req replicateCreatePredictionRequest) (string, any, error) {
	deployment := strings.TrimSpace(req.Deployment)
	version := strings.TrimSpace(req.Version)
	if deployment == "" && version == "" {
		return "", nil, exit(2, "replicate prediction create requires deployment or version")
	}
	if deployment != "" && version != "" {
		return "", nil, exit(2, "replicate prediction create accepts deployment or version, not both")
	}
	if deployment != "" {
		owner, name, ok := strings.Cut(deployment, "/")
		if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" || strings.Contains(name, "/") {
			return "", nil, exit(2, "replicate deployment must use owner/name")
		}
		path := "/deployments/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/predictions"
		return path, map[string]any{"input": blankInput(req.Input)}, nil
	}
	return "/predictions", map[string]any{"version": version, "input": blankInput(req.Input)}, nil
}

func blankInput(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	return input
}

func (c *replicateClient) GetPrediction(ctx context.Context, id string) (replicatePrediction, error) {
	path, err := predictionPath(id)
	if err != nil {
		return replicatePrediction{}, err
	}
	var out replicatePrediction
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return replicatePrediction{}, err
	}
	return out, nil
}

func (c *replicateClient) ListPredictions(ctx context.Context) (replicatePredictionList, error) {
	var out replicatePredictionList
	if err := c.doJSON(ctx, http.MethodGet, "/predictions", nil, nil, &out); err != nil {
		return replicatePredictionList{}, err
	}
	return out, nil
}

func (c *replicateClient) CancelPrediction(ctx context.Context, id string) (replicatePrediction, error) {
	path, err := predictionPath(id)
	if err != nil {
		return replicatePrediction{}, err
	}
	var out replicatePrediction
	if err := c.doJSON(ctx, http.MethodPost, path+"/cancel", nil, nil, &out); err != nil {
		return replicatePrediction{}, err
	}
	return out, nil
}

func predictionPath(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", exit(2, "replicate prediction id is required")
	}
	return "/predictions/" + url.PathEscape(id), nil
}

func (c *replicateClient) doJSON(ctx context.Context, method, path string, body any, headers map[string]string, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("replicate marshal %s: %w", path, err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("replicate request %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("replicate %s %s: %s", method, path, redactReplicateSecrets(err.Error(), c.token))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.responseError(method, path, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("replicate decode %s: %w", path, err)
	}
	return nil
}

func (c *replicateClient) responseError(method, path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = resp.Status
	}
	detail = redactReplicateSecrets(detail, c.token)
	return fmt.Errorf("replicate %s %s failed: status=%d body=%s", method, path, resp.StatusCode, detail)
}

func redactReplicateSecrets(text, token string) string {
	out := text
	if token != "" {
		out = strings.ReplaceAll(out, token, "[REDACTED]")
		out = strings.ReplaceAll(out, "Bearer "+token, "Bearer [REDACTED]")
	}
	out = redactBearerValues(out)
	return out
}

func redactBearerValues(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		if strings.EqualFold(strings.Trim(field, `"'`), "Bearer") && i+1 < len(fields) {
			fields[i+1] = "[REDACTED]"
		}
		if strings.HasPrefix(strings.ToLower(field), "bearer=") {
			fields[i] = "bearer=[REDACTED]"
		}
	}
	return strings.Join(fields, " ")
}

func predictionTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "failed", "canceled":
		return true
	default:
		return false
	}
}

func parsePredictionOutput(pred replicatePrediction) (RunnerOutput, error) {
	if len(pred.Output) == 0 || string(pred.Output) == "null" {
		return RunnerOutput{}, fmt.Errorf("replicate prediction %s has no runner output", pred.ID)
	}
	return ParseRunnerOutput(pred.Output)
}
