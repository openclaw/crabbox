package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type HetznerClient struct {
	Token  string
	Client *http.Client
}

type Server struct {
	CloudID   string
	Provider  string
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
}

type SSHKey struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

func (s Server) DisplayID() string {
	if s.CloudID != "" {
		return s.CloudID
	}
	return fmt.Sprint(s.ID)
}

func newHetznerClient() (*HetznerClient, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		token = os.Getenv("HETZNER_TOKEN")
	}
	if token == "" {
		return nil, exit(3, "HCLOUD_TOKEN or HETZNER_TOKEN is required")
	}
	return &HetznerClient{Token: token, Client: &http.Client{Timeout: 60 * time.Second}}, nil
}

func (c *HetznerClient) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		r = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.hetzner.cloud/v1"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("hetzner %s %s: http %d: %s", method, path, resp.StatusCode, summarizeJSON(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return err
		}
	}
	return nil
}

func (c *HetznerClient) ListCrabboxServers(ctx context.Context) ([]Server, error) {
	var res struct {
		Servers []Server `json:"servers"`
	}
	q := url.Values{}
	q.Set("label_selector", "crabbox=true")
	q.Set("per_page", "100")
	err := c.do(ctx, http.MethodGet, "/servers?"+q.Encode(), nil, &res)
	return res.Servers, err
}

func (c *HetznerClient) EnsureSSHKey(ctx context.Context, name, publicKey string) (SSHKey, error) {
	var list struct {
		SSHKeys []SSHKey `json:"ssh_keys"`
	}
	q := url.Values{}
	q.Set("name", name)
	if err := c.do(ctx, http.MethodGet, "/ssh_keys?"+q.Encode(), nil, &list); err != nil {
		return SSHKey{}, err
	}
	for _, key := range list.SSHKeys {
		if key.Name == name {
			if strings.TrimSpace(key.PublicKey) != strings.TrimSpace(publicKey) {
				return SSHKey{}, exit(3, "hetzner ssh key %q exists with different public key", name)
			}
			return key, nil
		}
	}
	q = url.Values{}
	q.Set("per_page", "100")
	if err := c.do(ctx, http.MethodGet, "/ssh_keys?"+q.Encode(), nil, &list); err != nil {
		return SSHKey{}, err
	}
	for _, key := range list.SSHKeys {
		if strings.TrimSpace(key.PublicKey) == strings.TrimSpace(publicKey) {
			return key, nil
		}
	}

	body := map[string]any{
		"name":       name,
		"public_key": publicKey,
		"labels": map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
		},
	}
	var created struct {
		SSHKey SSHKey `json:"ssh_key"`
	}
	if err := c.do(ctx, http.MethodPost, "/ssh_keys", body, &created); err != nil {
		return SSHKey{}, err
	}
	return created.SSHKey, nil
}

func (c *HetznerClient) DeleteSSHKey(ctx context.Context, name string) error {
	var list struct {
		SSHKeys []SSHKey `json:"ssh_keys"`
	}
	q := url.Values{}
	q.Set("name", name)
	if err := c.do(ctx, http.MethodGet, "/ssh_keys?"+q.Encode(), nil, &list); err != nil {
		return err
	}
	for _, key := range list.SSHKeys {
		if key.Name == name {
			return c.do(ctx, http.MethodDelete, fmt.Sprintf("/ssh_keys/%d", key.ID), nil, nil)
		}
	}
	return nil
}

func (c *HetznerClient) CreateServer(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool) (Server, error) {
	name := leaseProviderName(leaseID, slug)
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = renderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	now := time.Now().UTC()
	labels := directLeaseLabels(cfg, leaseID, slug, "hetzner", "", keep, now)
	body := map[string]any{
		"name":               name,
		"server_type":        cfg.ServerType,
		"image":              cfg.Image,
		"location":           cfg.Location,
		"labels":             labels,
		"ssh_keys":           []string{cfg.ProviderKey},
		"user_data":          cloudInit(cfg, publicKey),
		"start_after_create": true,
		"public_net": map[string]any{
			"enable_ipv4": true,
			"enable_ipv6": false,
		},
	}
	var res struct {
		Server Server `json:"server"`
	}
	if err := c.do(ctx, http.MethodPost, "/servers", body, &res); err != nil {
		return Server{}, err
	}
	return res.Server, nil
}

func (c *HetznerClient) CreateServerWithFallback(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, logf func(string, ...any)) (Server, Config, error) {
	candidates := serverTypeCandidatesForClass(cfg.Class)
	if cfg.ServerType != "" && cfg.ServerType != candidates[0] {
		candidates = append([]string{cfg.ServerType}, candidates...)
	}

	var errs []error
	for i, serverType := range candidates {
		next := cfg
		next.ServerType = serverType
		if i > 0 && logf != nil {
			logf("fallback provisioning type=%s after quota/capacity rejection\n", serverType)
		}
		server, err := c.CreateServer(ctx, next, publicKey, leaseID, slug, keep)
		if err == nil {
			return server, next, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", serverType, err))
		if !isRetryableProvisioningError(err) {
			return Server{}, next, joinErrors(errs)
		}
	}
	return Server{}, cfg, joinErrors(errs)
}

func isRetryableProvisioningError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "dedicated_core_limit") ||
		strings.Contains(s, "resource_limit_exceeded") ||
		strings.Contains(s, "server_type_not_available") ||
		strings.Contains(s, "location_not_available")
}

func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	msg := make([]string, 0, len(errs))
	for _, err := range errs {
		msg = append(msg, err.Error())
	}
	return errors.New(strings.Join(msg, "; "))
}

func (c *HetznerClient) GetServer(ctx context.Context, id int64) (Server, error) {
	var res struct {
		Server Server `json:"server"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/servers/%d", id), nil, &res); err != nil {
		return Server{}, err
	}
	return res.Server, nil
}

func (c *HetznerClient) DeleteServer(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/servers/%d", id), nil, nil)
}

func (c *HetznerClient) SetLabels(ctx context.Context, id int64, labels map[string]string) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/servers/%d", id), map[string]any{"labels": labels}, nil)
}

func summarizeJSON(data []byte) string {
	var parsed any
	if json.Unmarshal(data, &parsed) == nil {
		if b, err := json.Marshal(parsed); err == nil {
			data = b
		}
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}
