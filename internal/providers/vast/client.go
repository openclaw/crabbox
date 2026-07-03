package vast

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
	"regexp"
	"strconv"
	"strings"
)

type vastAPI interface {
	CheckAuth(context.Context) (vastUser, error)
	SearchOffers(context.Context, vastOfferSearchInput) ([]vastOffer, error)
	CreateInstance(context.Context, int, vastCreateInstanceInput) (vastCreateInstanceResponse, error)
	GetInstance(context.Context, int) (vastInstance, error)
	ListInstances(context.Context) ([]vastInstance, error)
	ManageInstance(context.Context, int, vastManageInstanceInput) (vastInstance, error)
	DestroyInstance(context.Context, int) error
	ListInstanceSSHKeys(context.Context, int) ([]vastInstanceSSHKey, error)
	AttachInstanceSSHKey(context.Context, int, string) (vastAttachSSHKeyResponse, error)
	DetachInstanceSSHKey(context.Context, int, string) error
}

type vastClient struct {
	apiKey     string
	apiURL     string
	httpClient *http.Client
}

const vastMaxResponseBytes = 4 << 20

var (
	errVastInstanceNotFound = errors.New("vast instance not found")
	errVastInstanceMissing  = errors.New("Vast response contained no instance")
)

type vastAPIError struct {
	Operation  string
	StatusCode int
	Status     string
	Body       string
}

func (e *vastAPIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("vast %s: %s", e.Operation, e.Status)
	}
	return fmt.Sprintf("vast %s: %s: %s", e.Operation, e.Status, e.Body)
}

type vastUser struct {
	ID       int    `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

type vastOffer struct {
	ID          int     `json:"id"`
	AskID       int     `json:"ask_contract_id"`
	MachineID   int     `json:"machine_id"`
	GPUName     string  `json:"gpu_name"`
	GPUCount    int     `json:"num_gpus"`
	Reliability float64 `json:"reliability2"`
	DphTotal    float64 `json:"dph_total"`
	SSHHost     string  `json:"ssh_host"`
	SSHPort     int     `json:"ssh_port"`
	Rentable    bool    `json:"rentable"`
	Rented      bool    `json:"rented"`
	Verified    bool    `json:"verified"`
}

type vastInstance struct {
	ID             int     `json:"id"`
	ContractID     int     `json:"contract_id"`
	Label          string  `json:"label"`
	Status         string  `json:"actual_status"`
	IntendedStatus string  `json:"intended_status"`
	SSHHost        string  `json:"ssh_host"`
	SSHPort        int     `json:"ssh_port"`
	GPUName        string  `json:"gpu_name"`
	GPUCount       int     `json:"num_gpus"`
	DphTotal       float64 `json:"dph_total"`
	Image          string  `json:"image_uuid"`
	InstanceAPIKey string  `json:"instance_api_key,omitempty"`
}

type vastCreateInstanceResponse struct {
	NewContract int          `json:"new_contract"`
	Instance    vastInstance `json:"instance"`
	Success     bool         `json:"success"`
}

type vastInstanceSSHKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"ssh_key"`
}

type vastAttachSSHKeyResponse struct {
	Success bool                 `json:"success"`
	Key     vastInstanceSSHKey   `json:"key"`
	Keys    []vastInstanceSSHKey `json:"keys"`
}

type vastOfferSearchInput struct {
	Config VastConfig
}

type vastCreateInstanceInput struct {
	Config      VastConfig
	Label       string
	SSHKey      string
	Environment map[string]string
	OnStart     string
}

type vastManageInstanceInput struct {
	State string `json:"state,omitempty"`
	Label string `json:"label,omitempty"`
}

func newVastClient(cfg VastConfig, rt Runtime) (vastAPI, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=%s requires CRABBOX_VAST_API_KEY or VAST_API_KEY", providerName)
	}
	apiURL := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	if apiURL == "" {
		apiURL = "https://console.vast.ai/api/v0"
	}
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return nil, exit(2, "vast.apiUrl must be an absolute URL without credentials")
	}
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return nil, exit(2, "vast.apiUrl must use https unless it targets localhost")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &vastClient{apiKey: apiKey, apiURL: apiURL, httpClient: secureVastHTTPClient(httpClient, apiURL)}, nil
}

func secureVastHTTPClient(source *http.Client, apiURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(apiURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameVastOrigin(trusted, req.URL) {
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

func (c *vastClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	endpoint := c.apiURL + path
	if parsed, err := url.Parse(path); err == nil && parsed.IsAbs() {
		endpoint = path
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
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
		return redactVastString(err.Error(), c.apiKey)
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, vastMaxResponseBytes+1))
	operation := method + " " + path
	if len(data) > vastMaxResponseBytes {
		return fmt.Errorf("vast %s response exceeds %d bytes", operation, vastMaxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.decodeAPIError(operation, resp.StatusCode, resp.Status, data, readErr)
	}
	if readErr != nil {
		return fmt.Errorf("vast %s response body: %w", operation, readErr)
	}
	if out != nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode vast %s response: %w", operation, err)
		}
	}
	return nil
}

func (c *vastClient) decodeAPIError(operation string, statusCode int, status string, data []byte, readErr error) error {
	body := strings.TrimSpace(string(data))
	if len(body) > 1600 {
		body = body[:1600]
	}
	body = redactVastText(body, c.apiKey)
	if readErr != nil {
		if body != "" {
			body += "; "
		}
		body += "response body read failed: " + readErr.Error()
	}
	return &vastAPIError{Operation: operation, StatusCode: statusCode, Status: status, Body: body}
}

func (c *vastClient) CheckAuth(ctx context.Context) (vastUser, error) {
	var out vastUser
	err := c.do(ctx, http.MethodGet, "/users/current/", nil, &out)
	return out, err
}

func (c *vastClient) SearchOffers(ctx context.Context, input vastOfferSearchInput) ([]vastOffer, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/bundles/", buildVastOfferSearchPayload(input.Config), &raw); err != nil {
		return nil, err
	}
	return decodeVastOffers(raw)
}

func (c *vastClient) CreateInstance(ctx context.Context, offerID int, input vastCreateInstanceInput) (vastCreateInstanceResponse, error) {
	var raw json.RawMessage
	path := "/asks/" + strconv.Itoa(offerID) + "/"
	if err := c.do(ctx, http.MethodPut, path, buildVastCreatePayload(input), &raw); err != nil {
		return vastCreateInstanceResponse{}, err
	}
	return decodeVastCreateInstanceResponse(raw)
}

func (c *vastClient) GetInstance(ctx context.Context, id int) (vastInstance, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/instances/"+strconv.Itoa(id)+"/", nil, &raw); err != nil {
		return vastInstance{}, err
	}
	return decodeVastInstance(raw)
}

func (c *vastClient) ListInstances(ctx context.Context) ([]vastInstance, error) {
	var out []vastInstance
	var afterToken string
	for {
		params := url.Values{}
		params.Set("limit", "25")
		if afterToken != "" {
			params.Set("after_token", afterToken)
		}
		var raw json.RawMessage
		endpoint := vastAPIURLForVersion(c.apiURL, "v1") + "/instances/?" + params.Encode()
		if err := c.do(ctx, http.MethodGet, endpoint, nil, &raw); err != nil {
			return nil, err
		}
		page, nextToken, err := decodeVastInstancesPage(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if strings.TrimSpace(nextToken) == "" {
			return out, nil
		}
		afterToken = nextToken
	}
}

func (c *vastClient) ManageInstance(ctx context.Context, id int, input vastManageInstanceInput) (vastInstance, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPut, "/instances/"+strconv.Itoa(id)+"/", input, &raw); err != nil {
		return vastInstance{}, err
	}
	instance, err := decodeVastInstance(raw)
	if !errors.Is(err, errVastInstanceMissing) {
		return instance, err
	}
	var result struct {
		Success bool `json:"success"`
	}
	if json.Unmarshal(raw, &result) == nil && result.Success {
		return vastInstance{}, nil
	}
	return vastInstance{}, err
}

func (c *vastClient) DestroyInstance(ctx context.Context, id int) error {
	return c.do(ctx, http.MethodDelete, "/instances/"+strconv.Itoa(id)+"/", nil, nil)
}

func (c *vastClient) ListInstanceSSHKeys(ctx context.Context, id int) ([]vastInstanceSSHKey, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/instances/"+strconv.Itoa(id)+"/ssh/", nil, &raw); err != nil {
		return nil, err
	}
	return decodeVastSSHKeys(raw)
}

func (c *vastClient) AttachInstanceSSHKey(ctx context.Context, id int, publicKey string) (vastAttachSSHKeyResponse, error) {
	var raw json.RawMessage
	body := map[string]string{"ssh_key": publicKey}
	if err := c.do(ctx, http.MethodPost, "/instances/"+strconv.Itoa(id)+"/ssh/", body, &raw); err != nil {
		return vastAttachSSHKeyResponse{}, err
	}
	return decodeVastAttachSSHKeyResponse(raw)
}

func (c *vastClient) DetachInstanceSSHKey(ctx context.Context, id int, keyID string) error {
	return c.do(ctx, http.MethodDelete, "/instances/"+strconv.Itoa(id)+"/ssh/"+url.PathEscape(keyID)+"/", nil, nil)
}

func buildVastOfferSearchPayload(cfg VastConfig) map[string]any {
	payload := map[string]any{
		"verified":          vastFilter("eq", true),
		"rentable":          vastFilter("eq", true),
		"rented":            vastFilter("eq", false),
		"direct_port_count": vastFilter("gte", 1),
	}
	if cfg.GPUName != "" {
		payload["gpu_name"] = vastFilter("eq", cfg.GPUName)
	}
	if cfg.GPUCount > 0 {
		payload["num_gpus"] = vastFilter("gte", cfg.GPUCount)
	}
	if cfg.MinReliability > 0 {
		payload["reliability"] = vastFilter("gte", cfg.MinReliability)
	}
	if cfg.MaxDphTotal > 0 {
		payload["dph_total"] = vastFilter("lte", cfg.MaxDphTotal)
	}
	instanceType := vastAPIInstanceType(cfg.InstanceType)
	if instanceType == "" {
		instanceType = "ondemand"
	}
	payload["type"] = instanceType
	if order := strings.TrimSpace(cfg.Order); order != "" {
		payload["order"] = vastOrderTuples(order)
	}
	return payload
}

func vastFilter(operator string, value any) map[string]any {
	return map[string]any{operator: value}
}

func vastAPIInstanceType(value string) string {
	switch normalizeInstanceType(value) {
	case "interruptible":
		return "bid"
	default:
		return normalizeInstanceType(value)
	}
}

func vastOrderTuples(order string) [][]string {
	parts := strings.Split(order, ",")
	out := make([][]string, 0, len(parts))
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 {
			continue
		}
		direction := "desc"
		if len(fields) > 1 {
			switch strings.ToLower(fields[1]) {
			case "asc", "desc":
				direction = strings.ToLower(fields[1])
			}
		}
		out = append(out, []string{fields[0], direction})
	}
	return out
}

func buildVastCreatePayload(input vastCreateInstanceInput) map[string]any {
	cfg := input.Config
	payload := map[string]any{
		"runtype":        strings.TrimSpace(cfg.Runtype),
		"target_state":   "running",
		"cancel_unavail": true,
		"vm":             false,
	}
	if cfg.Image != "" {
		payload["image"] = cfg.Image
	}
	if cfg.TemplateID != "" {
		payload["template_hash_id"] = cfg.TemplateID
	}
	if cfg.DiskGB > 0 {
		payload["disk"] = cfg.DiskGB
	}
	if input.Label != "" {
		payload["label"] = input.Label
	}
	if input.SSHKey != "" {
		payload["ssh_key"] = input.SSHKey
	}
	if len(input.Environment) > 0 {
		payload["env"] = input.Environment
	}
	if input.OnStart != "" {
		payload["onstart"] = input.OnStart
	}
	return payload
}

func decodeVastOffers(raw json.RawMessage) ([]vastOffer, error) {
	var direct []vastOffer
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, nil
	}
	var envelope struct {
		Offers  []vastOffer `json:"offers"`
		Bundles []vastOffer `json:"bundles"`
		Data    []vastOffer `json:"data"`
		Results []vastOffer `json:"results"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	switch {
	case envelope.Offers != nil:
		return envelope.Offers, nil
	case envelope.Bundles != nil:
		return envelope.Bundles, nil
	case envelope.Data != nil:
		return envelope.Data, nil
	default:
		return envelope.Results, nil
	}
}

func decodeVastCreateInstanceResponse(raw json.RawMessage) (vastCreateInstanceResponse, error) {
	var out vastCreateInstanceResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	if out.Instance.ID == 0 {
		inst, err := decodeVastInstance(raw)
		if err == nil {
			out.Instance = inst
		}
	}
	if out.NewContract == 0 {
		out.NewContract = firstNonZero(out.Instance.ContractID, out.Instance.ID)
	}
	return out, nil
}

func decodeVastInstance(raw json.RawMessage) (vastInstance, error) {
	var direct vastInstance
	if err := json.Unmarshal(raw, &direct); err != nil {
		return vastInstance{}, err
	}
	if direct.ID != 0 || direct.ContractID != 0 || direct.SSHHost != "" {
		return normalizeVastInstance(direct), nil
	}
	var envelope struct {
		Instance  vastInstance    `json:"instance"`
		Instances json.RawMessage `json:"instances"`
		Data      vastInstance    `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return vastInstance{}, err
	}
	if envelope.Instance.ID != 0 || envelope.Instance.ContractID != 0 || envelope.Instance.SSHHost != "" {
		return normalizeVastInstance(envelope.Instance), nil
	}
	if len(envelope.Instances) > 0 {
		var instance vastInstance
		if err := json.Unmarshal(envelope.Instances, &instance); err == nil && (instance.ID != 0 || instance.ContractID != 0 || instance.SSHHost != "") {
			return normalizeVastInstance(instance), nil
		}
		var instances []vastInstance
		if err := json.Unmarshal(envelope.Instances, &instances); err == nil {
			if len(instances) == 0 {
				return vastInstance{}, errVastInstanceNotFound
			}
			if len(instances) == 1 {
				return normalizeVastInstance(instances[0]), nil
			}
			return vastInstance{}, fmt.Errorf("decode Vast instance: expected one instance, got %d", len(instances))
		}
	}
	if envelope.Data.ID != 0 || envelope.Data.ContractID != 0 || envelope.Data.SSHHost != "" {
		return normalizeVastInstance(envelope.Data), nil
	}
	return vastInstance{}, errVastInstanceMissing
}

func decodeVastInstancesPage(raw json.RawMessage) ([]vastInstance, string, error) {
	var direct []vastInstance
	if err := json.Unmarshal(raw, &direct); err == nil {
		return normalizeVastInstances(direct), "", nil
	}
	var envelope struct {
		Instances []vastInstance `json:"instances"`
		Data      []vastInstance `json:"data"`
		Results   []vastInstance `json:"results"`
		NextToken string         `json:"next_token"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", err
	}
	switch {
	case envelope.Instances != nil:
		return normalizeVastInstances(envelope.Instances), envelope.NextToken, nil
	case envelope.Data != nil:
		return normalizeVastInstances(envelope.Data), envelope.NextToken, nil
	default:
		return normalizeVastInstances(envelope.Results), envelope.NextToken, nil
	}
}

func vastAPIURLForVersion(apiURL, version string) string {
	base := strings.TrimRight(apiURL, "/")
	for _, suffix := range []string{"/api/v0", "/api/v1"} {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(base, suffix) + "/api/" + version
		}
	}
	return base
}

func decodeVastSSHKeys(raw json.RawMessage) ([]vastInstanceSSHKey, error) {
	var direct []vastInstanceSSHKey
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, nil
	}
	var envelope struct {
		Keys    []vastInstanceSSHKey `json:"keys"`
		Data    []vastInstanceSSHKey `json:"data"`
		SSHKeys json.RawMessage      `json:"ssh_keys"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Keys != nil {
		return envelope.Keys, nil
	}
	if envelope.Data != nil {
		return envelope.Data, nil
	}
	if len(envelope.SSHKeys) == 0 || string(envelope.SSHKeys) == "null" {
		return nil, nil
	}
	var encoded string
	if err := json.Unmarshal(envelope.SSHKeys, &encoded); err == nil {
		envelope.SSHKeys = json.RawMessage(encoded)
	}
	if err := json.Unmarshal(envelope.SSHKeys, &direct); err != nil {
		return nil, err
	}
	return direct, nil
}

func decodeVastAttachSSHKeyResponse(raw json.RawMessage) (vastAttachSSHKeyResponse, error) {
	var envelope struct {
		Success bool                 `json:"success"`
		Key     json.RawMessage      `json:"key"`
		Keys    []vastInstanceSSHKey `json:"keys"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return vastAttachSSHKeyResponse{}, err
	}
	out := vastAttachSSHKeyResponse{Success: envelope.Success, Keys: envelope.Keys}
	if len(envelope.Key) == 0 || string(envelope.Key) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(envelope.Key, &out.Key); err == nil {
		return out, nil
	}
	var key string
	if err := json.Unmarshal(envelope.Key, &key); err != nil {
		return vastAttachSSHKeyResponse{}, err
	}
	key = strings.TrimSpace(key)
	if _, err := strconv.Atoi(key); err == nil {
		out.Key.ID = key
	} else {
		out.Key.PublicKey = key
	}
	return out, nil
}

func (k *vastInstanceSSHKey) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID        any    `json:"id"`
		Name      string `json:"name"`
		SSHKey    string `json:"ssh_key"`
		PublicKey string `json:"public_key"`
		Key       string `json:"key"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	k.Name = raw.Name
	k.PublicKey = firstNonBlank(raw.SSHKey, raw.PublicKey, raw.Key)
	switch id := raw.ID.(type) {
	case string:
		k.ID = strings.TrimSpace(id)
	case float64:
		k.ID = strconv.FormatFloat(id, 'f', -1, 64)
	case nil:
		k.ID = ""
	default:
		k.ID = strings.TrimSpace(fmt.Sprint(id))
	}
	return nil
}

func normalizeVastInstances(instances []vastInstance) []vastInstance {
	for i := range instances {
		instances[i] = normalizeVastInstance(instances[i])
	}
	return instances
}

func normalizeVastInstance(instance vastInstance) vastInstance {
	if instance.ContractID == 0 {
		instance.ContractID = instance.ID
	}
	if instance.ID == 0 {
		instance.ID = instance.ContractID
	}
	if instance.Status == "" {
		instance.Status = instance.IntendedStatus
	}
	return instance
}

func sameVastOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveVastPort(a) == effectiveVastPort(b)
}

func effectiveVastPort(value *url.URL) string {
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

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed == nil || parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	ip := net.ParseIP(host)
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || (ip != nil && ip.IsLoopback())
}

func redactVastString(value, apiKey string) error {
	return errors.New(redactVastText(value, apiKey))
}

func redactVastText(value, apiKey string) string {
	out := value
	if apiKey != "" {
		out = strings.ReplaceAll(out, apiKey, "<redacted>")
	}
	for _, field := range []string{
		"authorization", "api_key", "apiKey", "instance_api_key", "instanceApiKey",
		"ssh_key", "private_key", "privateKey", "jupyter_token", "jupyterToken",
		"user_data", "userData", "token",
	} {
		out = redactVastJSONishField(out, field)
		out = redactVastInlineField(out, field)
	}
	out = redactVastPrivateKeyBlock(out)
	out = redactVastTokenURLs(out)
	return out
}

func redactVastJSONishField(body, field string) string {
	pattern := regexp.MustCompile(`(?i)("` + regexp.QuoteMeta(field) + `"\s*:\s*)("[^"]*"|[^,}\s]+)`)
	return pattern.ReplaceAllString(body, `${1}"<redacted>"`)
}

func redactVastInlineField(body, field string) string {
	pattern := regexp.MustCompile(`(?i)(\b` + regexp.QuoteMeta(field) + `\s*[=:]\s*)[^",\s]+`)
	return pattern.ReplaceAllString(body, `${1}<redacted>`)
}

func redactVastPrivateKeyBlock(body string) string {
	pattern := regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
	return pattern.ReplaceAllString(body, "<redacted>")
}

func redactVastTokenURLs(body string) string {
	pattern := regexp.MustCompile(`https?://[^\s"']*(?i:(token|api_key|instance_api_key)=)[^\s"']+`)
	return pattern.ReplaceAllString(body, "<redacted>")
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
