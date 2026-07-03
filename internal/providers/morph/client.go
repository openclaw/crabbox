package morph

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

type morphAPI interface {
	BootSnapshot(ctx context.Context, snapshotID string, req morphBootSnapshotRequest) (morphInstance, error)
	GetSnapshot(ctx context.Context, snapshotID string) (morphSnapshot, error)
	GetInstance(ctx context.Context, instanceID string) (morphInstance, error)
	ListInstances(ctx context.Context, metadata map[string]string) ([]morphInstance, error)
	GetSSHKey(ctx context.Context, instanceID string) (morphSSHKey, error)
	SetInstanceMetadata(ctx context.Context, instanceID string, metadata map[string]string) error
	UpdateInstanceTTL(ctx context.Context, instanceID string, ttlSeconds int, ttlAction string) error
	UpdateInstanceWakeOn(ctx context.Context, instanceID string, wakeOnSSH, wakeOnHTTP *bool) error
	PauseInstance(ctx context.Context, instanceID string) error
	ResumeInstance(ctx context.Context, instanceID string) error
	DeleteInstance(ctx context.Context, instanceID string) error
}

var newMorphClient = func(cfg Config, rt Runtime) (morphAPI, error) {
	apiURL, err := normalizeMorphAPIURL(cfg.Morph.APIURL)
	if err != nil {
		return nil, exit(2, "provider=morph has invalid apiUrl %q: %v", cfg.Morph.APIURL, err)
	}
	apiKey := strings.TrimSpace(cfg.Morph.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=morph requires CRABBOX_MORPH_API_KEY, MORPH_API_KEY, or morph.apiKey")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &morphClient{
		apiURL:     apiURL,
		apiKey:     apiKey,
		httpClient: httpClient,
	}, nil
}

type morphClient struct {
	apiURL     string
	apiKey     string
	httpClient *http.Client
}

type morphAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *morphAPIError) Error() string {
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("morph api %s", e.Status)
	}
	return fmt.Sprintf("morph api %s: %s", e.Status, e.Body)
}

type morphMetadata map[string]string

func (m *morphMetadata) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*m = nil
		return nil
	}
	raw := map[string]any{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	normalized := make(map[string]string, len(raw))
	for key, value := range raw {
		switch typed := value.(type) {
		case nil:
			normalized[key] = ""
		case string:
			normalized[key] = typed
		default:
			normalized[key] = fmt.Sprint(typed)
		}
	}
	*m = normalized
	return nil
}

func (m morphMetadata) Clone() map[string]string {
	if len(m) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(m))
	for key, value := range m {
		cloned[key] = value
	}
	return cloned
}

type morphInstance struct {
	ID         string              `json:"id"`
	Status     string              `json:"status"`
	Metadata   morphMetadata       `json:"metadata"`
	Refs       morphInstanceRefs   `json:"refs"`
	Networking morphNetworking     `json:"networking"`
	WakeOn     morphWakeOnSettings `json:"wake_on"`
	TTL        morphTTLState       `json:"ttl"`
}

type morphInstanceRefs struct {
	SnapshotID string `json:"snapshot_id"`
}

type morphNetworking struct {
	Hostname   string `json:"hostname"`
	InternalIP string `json:"internal_ip"`
	ExternalIP string `json:"external_ip"`
}

type morphWakeOnSettings struct {
	WakeOnSSH  bool `json:"wake_on_ssh"`
	WakeOnHTTP bool `json:"wake_on_http"`
}

type morphTTLState struct {
	TTLSeconds int    `json:"ttl_seconds"`
	TTLAction  string `json:"ttl_action"`
}

type morphSnapshot struct {
	ID       string        `json:"id"`
	Metadata morphMetadata `json:"metadata"`
}

type morphSSHKey struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
	Password   string `json:"password"`
}

type morphBootSnapshotRequest struct {
	Metadata   map[string]string `json:"metadata,omitempty"`
	TTLSeconds *int              `json:"ttl_seconds,omitempty"`
	TTLAction  string            `json:"ttl_action,omitempty"`
}

type morphTTLRequest struct {
	TTLSeconds *int   `json:"ttl_seconds,omitempty"`
	TTLAction  string `json:"ttl_action,omitempty"`
}

type morphWakeOnRequest struct {
	WakeOnSSH  *bool `json:"wake_on_ssh,omitempty"`
	WakeOnHTTP *bool `json:"wake_on_http,omitempty"`
}

func (c *morphClient) BootSnapshot(ctx context.Context, snapshotID string, req morphBootSnapshotRequest) (morphInstance, error) {
	body, err := c.doRaw(ctx, http.MethodPost, "/snapshot/"+url.PathEscape(strings.TrimSpace(snapshotID))+"/boot", nil, req)
	if err != nil {
		return morphInstance{}, err
	}
	var instance morphInstance
	if err := decodeMorphResource(body, &instance); err != nil {
		return morphInstance{}, err
	}
	return instance, nil
}

func (c *morphClient) GetSnapshot(ctx context.Context, snapshotID string) (morphSnapshot, error) {
	body, err := c.doRaw(ctx, http.MethodGet, "/snapshot/"+url.PathEscape(strings.TrimSpace(snapshotID)), nil, nil)
	if err != nil {
		return morphSnapshot{}, err
	}
	var snapshot morphSnapshot
	if err := decodeMorphResource(body, &snapshot); err != nil {
		return morphSnapshot{}, err
	}
	return snapshot, nil
}

func (c *morphClient) GetInstance(ctx context.Context, instanceID string) (morphInstance, error) {
	body, err := c.doRaw(ctx, http.MethodGet, "/instance/"+url.PathEscape(strings.TrimSpace(instanceID)), nil, nil)
	if err != nil {
		return morphInstance{}, err
	}
	var instance morphInstance
	if err := decodeMorphResource(body, &instance); err != nil {
		return morphInstance{}, err
	}
	return instance, nil
}

func (c *morphClient) ListInstances(ctx context.Context, metadata map[string]string) ([]morphInstance, error) {
	query := url.Values{}
	for key, value := range metadata {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		query.Set("metadata["+key+"]", value)
	}
	body, err := c.doRaw(ctx, http.MethodGet, "/instance", query, nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data []morphInstance `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Data != nil {
		return envelope.Data, nil
	}
	var instances []morphInstance
	if err := json.Unmarshal(body, &instances); err != nil {
		return nil, err
	}
	return instances, nil
}

func (c *morphClient) GetSSHKey(ctx context.Context, instanceID string) (morphSSHKey, error) {
	body, err := c.doRaw(ctx, http.MethodGet, "/instance/"+url.PathEscape(strings.TrimSpace(instanceID))+"/ssh/key", nil, nil)
	if err != nil {
		return morphSSHKey{}, err
	}
	var sshKey morphSSHKey
	if err := decodeMorphResource(body, &sshKey); err != nil {
		return morphSSHKey{}, err
	}
	return sshKey, nil
}

func (c *morphClient) SetInstanceMetadata(ctx context.Context, instanceID string, metadata map[string]string) error {
	_, err := c.doRaw(ctx, http.MethodPost, "/instance/"+url.PathEscape(strings.TrimSpace(instanceID))+"/metadata", nil, metadata)
	return err
}

func (c *morphClient) UpdateInstanceTTL(ctx context.Context, instanceID string, ttlSeconds int, ttlAction string) error {
	req := morphTTLRequest{TTLSeconds: &ttlSeconds, TTLAction: strings.TrimSpace(ttlAction)}
	_, err := c.doRaw(ctx, http.MethodPost, "/instance/"+url.PathEscape(strings.TrimSpace(instanceID))+"/ttl", nil, req)
	return err
}

func (c *morphClient) UpdateInstanceWakeOn(ctx context.Context, instanceID string, wakeOnSSH, wakeOnHTTP *bool) error {
	req := morphWakeOnRequest{WakeOnSSH: wakeOnSSH, WakeOnHTTP: wakeOnHTTP}
	_, err := c.doRaw(ctx, http.MethodPost, "/instance/"+url.PathEscape(strings.TrimSpace(instanceID))+"/wake-on", nil, req)
	return err
}

func (c *morphClient) PauseInstance(ctx context.Context, instanceID string) error {
	_, err := c.doRaw(ctx, http.MethodPost, "/instance/"+url.PathEscape(strings.TrimSpace(instanceID))+"/pause", nil, nil)
	return err
}

func (c *morphClient) ResumeInstance(ctx context.Context, instanceID string) error {
	_, err := c.doRaw(ctx, http.MethodPost, "/instance/"+url.PathEscape(strings.TrimSpace(instanceID))+"/resume", nil, nil)
	return err
}

func (c *morphClient) DeleteInstance(ctx context.Context, instanceID string) error {
	_, err := c.doRaw(ctx, http.MethodDelete, "/instance/"+url.PathEscape(strings.TrimSpace(instanceID)), nil, nil)
	return err
}

func (c *morphClient) doRaw(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	endpoint, err := url.Parse(c.apiURL + path)
	if err != nil {
		return nil, err
	}
	if len(query) > 0 {
		endpoint.RawQuery = query.Encode()
	}
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &morphAPIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       shared.RedactErrorSecrets(summarizeMorphResponse(data), c.apiKey),
		}
	}
	return data, nil
}

func normalizeMorphAPIURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "https://cloud.morph.so"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("expected absolute http(s) URL")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isMorphLoopbackHost(parsed.Hostname())) {
		return "", fmt.Errorf("expected https URL or http loopback URL")
	}
	if strings.HasSuffix(parsed.Path, "/") {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	if parsed.Path == "" {
		parsed.Path = "/api"
	} else if !strings.HasSuffix(parsed.Path, "/api") {
		parsed.Path += "/api"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isMorphLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func decodeMorphResource(data []byte, out any) error {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && len(bytes.TrimSpace(envelope.Data)) > 0 {
		return json.Unmarshal(envelope.Data, out)
	}
	return json.Unmarshal(data, out)
}

func summarizeMorphResponse(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return ""
	}
	if data[0] == '{' {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err == nil {
			for _, key := range []string{"detail", "message", "error"} {
				if value, ok := payload[key]; ok {
					return strings.TrimSpace(fmt.Sprint(value))
				}
			}
		}
	}
	text := strings.TrimSpace(string(data))
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

func isMorphNotFound(err error) bool {
	var apiErr *morphAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
