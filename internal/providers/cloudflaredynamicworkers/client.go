package cloudflaredynamicworkers

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
	"time"
)

type loaderAPI interface {
	Readiness(context.Context) (readinessResponse, error)
	Run(context.Context, runRequest) (runResponse, error)
	Status(context.Context, string) (runStatus, error)
	Delete(context.Context, string) error
	DeleteAcknowledgedComplete(context.Context, string) error
}

type client struct {
	baseURL             string
	token               string
	http                *http.Client
	responseBodyTimeout time.Duration
}

type readinessResponse struct {
	OK                 bool              `json:"ok"`
	Runner             string            `json:"runner"`
	LoaderBinding      bool              `json:"loaderBinding"`
	CoordinatorBinding bool              `json:"coordinatorBinding"`
	DurableRunMetadata bool              `json:"durableRunMetadata"`
	CompatibilityDate  string            `json:"compatibilityDate,omitempty"`
	Egress             string            `json:"egress,omitempty"`
	Limits             limits            `json:"limits,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

type runRequest struct {
	ID                 string            `json:"id,omitempty"`
	WorkerID           string            `json:"workerId,omitempty"`
	CacheMode          string            `json:"cacheMode"`
	RetainMetadata     bool              `json:"retainMetadata"`
	RetainOnFailure    bool              `json:"retainOnFailure,omitempty"`
	Module             moduleSource      `json:"module"`
	CompatibilityDate  string            `json:"compatibilityDate,omitempty"`
	CompatibilityFlags []string          `json:"compatibilityFlags,omitempty"`
	Egress             string            `json:"egress"`
	Limits             limits            `json:"limits,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	TimeoutMS          int64             `json:"timeoutMs,omitempty"`
}

type moduleSource struct {
	Name   string `json:"name,omitempty"`
	Source string `json:"source"`
}

type limits struct {
	CPUMs       int `json:"cpuMs,omitempty"`
	Subrequests int `json:"subrequests,omitempty"`
}

type runResponse struct {
	ID                 string            `json:"id"`
	WorkerID           string            `json:"workerId,omitempty"`
	Status             string            `json:"status"`
	ExitCode           int               `json:"exitCode"`
	Stdout             string            `json:"stdout,omitempty"`
	Stderr             string            `json:"stderr,omitempty"`
	Body               string            `json:"body,omitempty"`
	Logs               string            `json:"logs,omitempty"`
	Timing             map[string]int64  `json:"timing,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	LifecycleUncertain bool              `json:"lifecycleUncertain,omitempty"`
	LifecycleMessage   string            `json:"lifecycleMessage,omitempty"`
}

type runStatus struct {
	ID        string            `json:"id"`
	WorkerID  string            `json:"workerId,omitempty"`
	Status    string            `json:"status"`
	CreatedAt string            `json:"createdAt,omitempty"`
	UpdatedAt string            `json:"updatedAt,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type apiError struct {
	StatusCode int
	Status     string
	Body       string
}

type responseContractError struct {
	message string
	cause   error
}

func (e *responseContractError) Error() string {
	return e.message
}

func (e *responseContractError) Unwrap() error {
	return e.cause
}

func (e *apiError) Error() string {
	if strings.TrimSpace(e.Body) == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

const (
	defaultResponseHeaderTimeout  = 30 * time.Second
	responseHeaderTimeoutOverhead = 5 * time.Second
)

var newLoaderAPI = func(cfg Config, rt Runtime) (loaderAPI, error) {
	baseURL, err := loaderURL(cfg)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(cfg.CloudflareDynamicWorkers.Token)
	if token == "" {
		return nil, exit(2, "%s requires cloudflareDynamicWorkers.token or CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN", providerName)
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = defaultHTTPClient(cfg)
	}
	httpClient = noRedirectHTTPClient(httpClient)
	return &client{
		baseURL:             baseURL,
		token:               token,
		http:                httpClient,
		responseBodyTimeout: responseHeaderTimeout(cfg),
	}, nil
}

func loaderURL(cfg Config) (string, error) {
	raw := strings.TrimSpace(cfg.CloudflareDynamicWorkers.LoaderURL)
	if raw == "" {
		return "", exit(2, "%s requires cloudflareDynamicWorkers.loaderUrl or CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL", providerName)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", exit(2, "%s loader URL %q is invalid", providerName, loaderURLForError(raw))
	}
	if parsed.User != nil {
		return "", exit(2, "%s loader URL must not include userinfo", providerName)
	}
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return "", exit(2, "%s loader URL %q must use https unless it targets localhost", providerName, loaderURLForError(raw))
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "%s loader URL %q must not include query or fragment components", providerName, loaderURLForError(raw))
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func loaderURLForError(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil {
		if parsed.Opaque != "" || parsed.Host == "" {
			return "<redacted>"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	}
	return "<redacted>"
}

func loaderClaimScope(cfg Config) (string, error) {
	raw, err := loaderURL(cfg)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", exit(2, "%s loader URL is invalid", providerName)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	parsed.Host = host
	if port != "" {
		parsed.Host += ":" + port
	}
	escapedPath := canonicalPercentEscapes(strings.TrimRight(parsed.EscapedPath(), "/"))
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return "", exit(2, "%s loader URL path is invalid", providerName)
	}
	parsed.Path = decodedPath
	if escapedPath == decodedPath {
		parsed.RawPath = ""
	} else {
		parsed.RawPath = escapedPath
	}
	return "endpoint:" + strings.TrimRight(parsed.String(), "/"), nil
}

func canonicalPercentEscapes(value string) string {
	var canonical strings.Builder
	canonical.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '%' && i+2 < len(value) {
			decoded, ok := percentEncodedByte(value[i+1], value[i+2])
			if ok && isURIUnreserved(decoded) {
				canonical.WriteByte(decoded)
				i += 2
				continue
			}
			canonical.WriteByte('%')
			canonical.WriteByte(asciiUpperHex(value[i+1]))
			canonical.WriteByte(asciiUpperHex(value[i+2]))
			i += 2
			continue
		}
		canonical.WriteByte(value[i])
	}
	return canonical.String()
}

func percentEncodedByte(high, low byte) (byte, bool) {
	highValue, highOK := asciiHexValue(high)
	lowValue, lowOK := asciiHexValue(low)
	if !highOK || !lowOK {
		return 0, false
	}
	return highValue<<4 | lowValue, true
}

func asciiHexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func isURIUnreserved(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '-' || value == '.' || value == '_' || value == '~'
}

func asciiUpperHex(value byte) byte {
	if value >= 'a' && value <= 'f' {
		return value - ('a' - 'A')
	}
	return value
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func defaultHTTPClient(cfg Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout(cfg)
	return &http.Client{Transport: transport}
}

func noRedirectHTTPClient(httpClient *http.Client) *http.Client {
	cloned := *httpClient
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &cloned
}

func responseHeaderTimeout(cfg Config) time.Duration {
	runTimeout := time.Duration(cfg.CloudflareDynamicWorkers.TimeoutSecs) * time.Second
	if runTimeout <= 0 {
		return defaultResponseHeaderTimeout
	}
	timeout := runTimeout + responseHeaderTimeoutOverhead
	if timeout < defaultResponseHeaderTimeout {
		return defaultResponseHeaderTimeout
	}
	return timeout
}

func (c *client) Readiness(ctx context.Context) (readinessResponse, error) {
	var out readinessResponse
	err := c.doJSON(ctx, http.MethodGet, "/v1/readiness", nil, &out)
	return out, err
}

func (c *client) Run(ctx context.Context, req runRequest) (runResponse, error) {
	var out runResponse
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(req); err != nil {
		return out, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/runs", &body)
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		err := c.decodeJSONResponse(ctx, resp.Body, &out)
		if err != nil {
			return out, &responseContractError{message: "loader response is not valid JSON", cause: err}
		}
		err = validateRunResponse(req, out)
		if strings.EqualFold(strings.TrimSpace(resp.Header.Get("X-Crabbox-Lifecycle-Uncertain")), "true") {
			out.LifecycleUncertain = true
		}
		return out, err
	}
	responseBody, readErr := c.readResponseBody(ctx, resp.Body, 64*1024)
	responseErr := c.responseErrorBody(resp.StatusCode, resp.Status, responseBody)
	decoded, lifecycleResponse, decodeErr := decodeRunErrorResponse(responseBody)
	if lifecycleResponse {
		out = decoded
	}
	if readErr != nil {
		contractErr := &responseContractError{message: "loader error response body is incomplete", cause: readErr}
		return out, errors.Join(responseErr, contractErr, readErr)
	}
	if lifecycleResponse {
		if decodeErr != nil {
			contractErr := &responseContractError{message: "loader lifecycle error response is not valid JSON", cause: decodeErr}
			return out, errors.Join(responseErr, contractErr)
		}
		if contractErr := validateRunResponse(req, out); contractErr != nil {
			return out, errors.Join(responseErr, contractErr)
		}
		if strings.EqualFold(strings.TrimSpace(resp.Header.Get("X-Crabbox-Lifecycle-Uncertain")), "true") {
			out.LifecycleUncertain = true
		}
		return out, nil
	}
	if decodeErr != nil && looksLikeRunResponse(responseBody) {
		contractErr := &responseContractError{message: "loader lifecycle error response is not valid JSON", cause: decodeErr}
		return out, errors.Join(responseErr, contractErr)
	}
	return out, responseErr
}

func decodeRunErrorResponse(body []byte) (runResponse, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		out, lifecycleResponse := decodeRunResponsePrefix(body)
		return out, lifecycleResponse, err
	}
	if !hasRunResponseField(fields) {
		return runResponse{}, false, nil
	}
	var out runResponse
	err := json.Unmarshal(body, &out)
	return out, true, err
}

func hasRunResponseField(fields map[string]json.RawMessage) bool {
	for field := range fields {
		if isRunResponseField(field) {
			return true
		}
	}
	return false
}

func decodeRunResponsePrefix(body []byte) (runResponse, bool) {
	var out runResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return out, false
	}
	lifecycleResponse := false
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return out, lifecycleResponse
		}
		field, ok := token.(string)
		if !ok {
			return out, lifecycleResponse
		}
		lifecycleResponse = lifecycleResponse || isRunResponseField(field)
		switch field {
		case "id":
			if err := decoder.Decode(&out.ID); err != nil {
				return out, lifecycleResponse
			}
		case "workerId":
			if err := decoder.Decode(&out.WorkerID); err != nil {
				return out, lifecycleResponse
			}
		case "status":
			if err := decoder.Decode(&out.Status); err != nil {
				return out, lifecycleResponse
			}
		default:
			var discard json.RawMessage
			if err := decoder.Decode(&discard); err != nil {
				return out, lifecycleResponse
			}
		}
	}
	return out, lifecycleResponse
}

func isRunResponseField(field string) bool {
	switch field {
	case "id", "workerId", "status", "exitCode", "stdout", "stderr", "body", "logs",
		"timing", "metadata", "lifecycleUncertain", "lifecycleMessage":
		return true
	default:
		return false
	}
}

func looksLikeRunResponse(body []byte) bool {
	for _, field := range [][]byte{
		[]byte(`"id"`),
		[]byte(`"workerId"`),
		[]byte(`"status"`),
		[]byte(`"exitCode"`),
		[]byte(`"lifecycleUncertain"`),
	} {
		if bytes.Contains(body, field) {
			return true
		}
	}
	return false
}

func (c *client) Status(ctx context.Context, id string) (runStatus, error) {
	var out runStatus
	err := c.doJSON(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, &out)
	if err == nil {
		err = validateRunStatus(id, out)
	}
	return out, err
}

func validateRunResponse(req runRequest, out runResponse) error {
	if strings.TrimSpace(out.ID) == "" {
		return &responseContractError{message: "loader response is missing run id"}
	}
	if strings.TrimSpace(req.ID) != "" && out.ID != req.ID {
		return &responseContractError{message: "loader response run id does not match request"}
	}
	if strings.TrimSpace(out.Status) == "" {
		return &responseContractError{message: "loader response is missing run status"}
	}
	if !validRunResponseStatus(out.Status) {
		return &responseContractError{message: "loader response has invalid run status"}
	}
	return nil
}

func validateRunStatus(id string, out runStatus) error {
	if strings.TrimSpace(out.ID) == "" {
		return &responseContractError{message: "loader status response is missing run id"}
	}
	if out.ID != id {
		return &responseContractError{message: "loader status response run id does not match request"}
	}
	if strings.TrimSpace(out.Status) == "" {
		return &responseContractError{message: "loader status response is missing run status"}
	}
	if !validRunStatus(out.Status) {
		return &responseContractError{message: "loader status response has invalid run status"}
	}
	return nil
}

func validRunResponseStatus(status string) bool {
	switch status {
	case "succeeded", "failed":
		return true
	default:
		return false
	}
}

func validRunStatus(status string) bool {
	switch status {
	case "running", "succeeded", "failed", "stopped":
		return true
	default:
		return false
	}
}

func (c *client) Delete(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/runs/"+url.PathEscape(id), nil, nil)
}

func (c *client) DeleteAcknowledgedComplete(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/runs/"+url.PathEscape(id)+"?acknowledgedComplete=true", nil, nil)
}

func (c *client) doJSON(ctx context.Context, method, endpoint string, input any, output any) error {
	var body io.Reader
	if input != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(input); err != nil {
			return err
		}
		body = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.responseError(ctx, resp)
	}
	if output == nil {
		return nil
	}
	return c.decodeJSONResponse(ctx, resp.Body, output)
}

func (c *client) responseError(ctx context.Context, resp *http.Response) error {
	body, err := c.readResponseBody(ctx, resp.Body, 64*1024)
	responseErr := c.responseErrorBody(resp.StatusCode, resp.Status, body)
	if err != nil {
		return fmt.Errorf("%w: reading response body: %w", responseErr, err)
	}
	return responseErr
}

func (c *client) decodeJSONResponse(ctx context.Context, body io.ReadCloser, output any) error {
	return c.withResponseBodyDeadline(ctx, body, func() error {
		decoder := json.NewDecoder(body)
		if err := decoder.Decode(output); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return fmt.Errorf("response contains multiple JSON values")
			}
			return err
		}
		return nil
	})
}

func (c *client) readResponseBody(ctx context.Context, body io.ReadCloser, limit int64) ([]byte, error) {
	var out []byte
	err := c.withResponseBodyDeadline(ctx, body, func() error {
		var err error
		out, err = io.ReadAll(io.LimitReader(body, limit))
		return err
	})
	return out, err
}

func (c *client) withResponseBodyDeadline(ctx context.Context, body io.ReadCloser, read func() error) error {
	bodyCtx, cancel := context.WithTimeout(ctx, c.responseBodyTimeout)
	defer cancel()
	stopClose := context.AfterFunc(bodyCtx, func() {
		_ = body.Close()
	})
	defer stopClose()
	err := read()
	if bodyCtx.Err() != nil {
		return bodyCtx.Err()
	}
	return err
}

func (c *client) responseErrorBody(statusCode int, status string, body []byte) error {
	text := strings.TrimSpace(string(body))
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		switch {
		case strings.TrimSpace(payload.Error) != "":
			text = payload.Error
		case strings.TrimSpace(payload.Message) != "":
			text = payload.Message
		}
	}
	if text == "" {
		text = status
	}
	text = redactSensitive(text, c.token)
	return &apiError{StatusCode: statusCode, Status: providerName + " API " + status, Body: text}
}

func redactSensitive(text, token string) string {
	out := text
	if token = strings.TrimSpace(token); token != "" {
		out = strings.ReplaceAll(out, token, "<redacted>")
	}
	fields := strings.Fields(out)
	for i, field := range fields {
		lower := strings.ToLower(strings.Trim(field, `"'`))
		if strings.HasPrefix(lower, "bearer") && i+1 < len(fields) {
			fields[i+1] = "<redacted>"
		}
	}
	out = strings.Join(fields, " ")
	if strings.Contains(strings.ToLower(out), "authorization: bearer ") {
		out = redactBearerLine(out)
	}
	return out
}

func redactBearerLine(text string) string {
	parts := strings.Split(text, "\n")
	for i, part := range parts {
		lower := strings.ToLower(part)
		if idx := strings.Index(lower, "authorization: bearer "); idx >= 0 {
			parts[i] = part[:idx] + "Authorization: Bearer <redacted>"
		}
	}
	return strings.Join(parts, "\n")
}

func providerError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", providerName, action, err)
}
