package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const apiBaseURL = "https://api.digitalocean.com/v2"

type digitalOceanClient struct {
	token   string
	client  *http.Client
	baseURL string
}

type digitalOceanAPIError struct {
	Operation string
	Status    int
	Body      string
}

func (e *digitalOceanAPIError) Error() string {
	return fmt.Sprintf("digitalocean %s: http %d: %s", e.Operation, e.Status, e.Body)
}

type droplet struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
	Networks struct {
		V4 []struct {
			IPAddress string `json:"ip_address"`
			Type      string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
	Size struct {
		Slug string `json:"slug"`
	} `json:"size"`
	Region struct {
		Slug string `json:"slug"`
	} `json:"region"`
}

type sshKey struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

func newDigitalOceanClient(rt core.Runtime) (*digitalOceanClient, error) {
	token := strings.TrimSpace(os.Getenv("DIGITALOCEAN_TOKEN"))
	if token == "" {
		return nil, core.Exit(3, "DIGITALOCEAN_TOKEN is required")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &digitalOceanClient{token: token, client: httpClient, baseURL: apiBaseURL}, nil
}

func (c *digitalOceanClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		reader = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(data) > 400 {
			data = data[:400]
		}
		return &digitalOceanAPIError{Operation: method + " " + path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("digitalocean %s %s decode: %w", method, path, err)
	}
	return nil
}

func (c *digitalOceanClient) ListCrabboxDroplets(ctx context.Context) ([]droplet, error) {
	var out []droplet
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("tag_name", tagCrabbox)
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", "200")
		var res struct {
			Droplets []droplet `json:"droplets"`
			Links    struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := c.do(ctx, http.MethodGet, "/droplets?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		for _, item := range res.Droplets {
			if isOwnedDroplet(item) {
				out = append(out, item)
			}
		}
		if res.Links.Pages.Next == "" {
			return out, nil
		}
	}
}

func (c *digitalOceanClient) GetDroplet(ctx context.Context, id int64) (droplet, error) {
	var res struct {
		Droplet droplet `json:"droplet"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/droplets/%d", id), nil, &res); err != nil {
		return droplet{}, err
	}
	return res.Droplet, nil
}

func (c *digitalOceanClient) CreateDroplet(ctx context.Context, cfg core.Config, publicKey, leaseID, slug string, keep bool, now time.Time) (droplet, error) {
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	key, _, err := c.EnsureSSHKey(ctx, providerKeyForLease(leaseID), publicKey)
	if err != nil {
		return droplet{}, err
	}
	tags := leaseTags(cfg, leaseID, slug, "provisioning", keep, now)
	for _, tag := range tags {
		if err := c.EnsureTag(ctx, tag); err != nil {
			return droplet{}, err
		}
	}
	body := map[string]any{
		"name":       core.LeaseProviderName(leaseID, slug),
		"region":     digitalOceanRegion(cfg),
		"size":       cfg.ServerType,
		"image":      digitalOceanImage(cfg),
		"ssh_keys":   []any{key.ID},
		"tags":       tags,
		"user_data":  core.CloudInitUserData(cfg, publicKey),
		"monitoring": true,
		"ipv6":       false,
	}
	if cfg.DigitalOcean.VPCUUID != "" {
		body["vpc_uuid"] = cfg.DigitalOcean.VPCUUID
	}
	var res struct {
		Droplet droplet `json:"droplet"`
	}
	if err := c.do(ctx, http.MethodPost, "/droplets", body, &res); err != nil {
		return droplet{}, err
	}
	return res.Droplet, nil
}

func (c *digitalOceanClient) DeleteDroplet(ctx context.Context, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/droplets/%d", id), nil, nil)
	if isDigitalOceanNotFound(err) {
		return nil
	}
	return err
}

func (c *digitalOceanClient) ListSSHKeys(ctx context.Context) ([]sshKey, error) {
	var out []sshKey
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", "200")
		var res struct {
			SSHKeys []sshKey `json:"ssh_keys"`
			Links   struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := c.do(ctx, http.MethodGet, "/account/keys?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		out = append(out, res.SSHKeys...)
		if res.Links.Pages.Next == "" {
			return out, nil
		}
	}
}

func (c *digitalOceanClient) EnsureSSHKey(ctx context.Context, name, publicKey string) (sshKey, bool, error) {
	keys, err := c.ListSSHKeys(ctx)
	if err != nil {
		return sshKey{}, false, err
	}
	for _, key := range keys {
		if key.Name == name {
			return key, false, nil
		}
	}
	body := map[string]any{"name": name, "public_key": publicKey}
	var res struct {
		SSHKey sshKey `json:"ssh_key"`
	}
	if err := c.do(ctx, http.MethodPost, "/account/keys", body, &res); err != nil {
		return sshKey{}, false, err
	}
	return res.SSHKey, true, nil
}

func (c *digitalOceanClient) DeleteSSHKeyByName(ctx context.Context, name string) error {
	keys, err := c.ListSSHKeys(ctx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.Name == name {
			err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/account/keys/%d", key.ID), nil, nil)
			if isDigitalOceanNotFound(err) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (c *digitalOceanClient) EnsureTag(ctx context.Context, tag string) error {
	err := c.do(ctx, http.MethodPost, "/tags", map[string]any{"name": tag}, nil)
	if apiErr, ok := err.(*digitalOceanAPIError); ok && apiErr.Status == http.StatusUnprocessableEntity {
		return nil
	}
	return err
}

func (c *digitalOceanClient) TagDroplet(ctx context.Context, id int64, tags []string) error {
	resources := make([]map[string]any, 0, 1)
	resources = append(resources, map[string]any{"resource_id": strconv.FormatInt(id, 10), "resource_type": "droplet"})
	for _, tag := range tags {
		if err := c.EnsureTag(ctx, tag); err != nil {
			return err
		}
		body := map[string]any{"resources": resources}
		if err := c.do(ctx, http.MethodPost, "/tags/"+url.PathEscape(tag)+"/resources", body, nil); err != nil {
			return err
		}
	}
	return nil
}

func isDigitalOceanNotFound(err error) bool {
	apiErr, ok := err.(*digitalOceanAPIError)
	return ok && apiErr.Status == http.StatusNotFound
}

func digitalOceanRegion(cfg core.Config) string {
	if cfg.DigitalOcean.Region != "" {
		return cfg.DigitalOcean.Region
	}
	if cfg.Location != "" {
		return cfg.Location
	}
	return "nyc3"
}

func digitalOceanImage(cfg core.Config) string {
	if cfg.DigitalOcean.Image != "" {
		return cfg.DigitalOcean.Image
	}
	if cfg.Image != "" {
		return cfg.Image
	}
	return "ubuntu-24-04-x64"
}

func providerKeyForLease(leaseID string) string {
	return "crabbox-" + strings.ReplaceAll(leaseID, "_", "-")
}
