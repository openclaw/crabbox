package digitalocean

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
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const apiBaseURL = "https://api.digitalocean.com/v2"

const (
	createReconcileTimeout  = 30 * time.Second
	createReconcileInterval = 2 * time.Second
)

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
	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(data) > 400 {
			data = data[:400]
		}
		body := strings.TrimSpace(string(data))
		if readErr != nil {
			if body != "" {
				body += "; "
			}
			body += "response body read failed: " + readErr.Error()
		}
		return &digitalOceanAPIError{Operation: method + " " + path, Status: resp.StatusCode, Body: body}
	}
	if readErr != nil {
		return fmt.Errorf("digitalocean %s %s response body: %w", method, path, readErr)
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
	droplets, err := c.listDropletsByTag(ctx, tagCrabbox)
	if err != nil {
		return nil, err
	}
	var out []droplet
	for _, item := range droplets {
		if isOwnedDroplet(item) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (c *digitalOceanClient) listDropletsByTag(ctx context.Context, tag string) ([]droplet, error) {
	var out []droplet
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("tag_name", tag)
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
		out = append(out, res.Droplets...)
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
	keyName := providerKeyForLease(leaseID)
	key, createdKey, err := c.EnsureSSHKey(ctx, keyName, publicKey)
	if err != nil {
		return droplet{}, err
	}
	rollbackKey := func(cause error) error {
		if !createdKey {
			return cause
		}
		return c.rollbackCreatedSSHKey(keyName, cause)
	}
	tags := leaseTags(cfg, leaseID, slug, "provisioning", keep, now)
	name := core.LeaseProviderName(leaseID, slug)
	body := map[string]any{
		"name":       name,
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
		if shouldReconcileMutation(err) {
			if item, reconcileErr := c.reconcileDropletCreate(leaseID, name); reconcileErr == nil {
				return item, nil
			} else {
				err = errors.Join(err, reconcileErr)
			}
		}
		return droplet{}, rollbackKey(err)
	}
	return res.Droplet, nil
}

func shouldReconcileMutation(err error) bool {
	var apiErr *digitalOceanAPIError
	return !errors.As(err, &apiErr) || apiErr.Status >= http.StatusInternalServerError
}

func (c *digitalOceanClient) reconcileDropletCreate(leaseID, name string) (droplet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), createReconcileTimeout)
	defer cancel()

	leaseTag := encodeTagKV("lease", leaseID)
	var lastErr error
	for {
		droplets, err := c.listDropletsByTag(ctx, leaseTag)
		if err == nil {
			var matches []droplet
			for _, item := range droplets {
				if item.Name == name && isOwnedDroplet(item) && labelsFromTags(item.Tags)["lease"] == leaseID {
					matches = append(matches, item)
				}
			}
			switch len(matches) {
			case 1:
				return matches[0], nil
			case 0:
				lastErr = core.Exit(4, "digitalocean create reconciliation did not find lease=%s", leaseID)
			default:
				return droplet{}, core.Exit(2, "digitalocean create reconciliation found multiple droplets for lease=%s", leaseID)
			}
		} else {
			lastErr = err
		}

		timer := time.NewTimer(createReconcileInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return droplet{}, errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func (c *digitalOceanClient) rollbackCreatedSSHKey(keyName string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cleanupErr := c.DeleteSSHKeyByName(ctx, keyName); cleanupErr != nil {
		return errors.Join(cause, fmt.Errorf("rollback digitalocean ssh key %s: %w", keyName, cleanupErr))
	}
	return cause
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
			if strings.TrimSpace(key.PublicKey) != strings.TrimSpace(publicKey) {
				return sshKey{}, false, core.Exit(3, "digitalocean ssh key %q exists with different public key", name)
			}
			return key, false, nil
		}
	}
	body := map[string]any{"name": name, "public_key": publicKey}
	var res struct {
		SSHKey sshKey `json:"ssh_key"`
	}
	if err := c.do(ctx, http.MethodPost, "/account/keys", body, &res); err != nil {
		if shouldReconcileMutation(err) {
			if key, reconcileErr := c.reconcileSSHKey(name, publicKey); reconcileErr == nil {
				return key, true, nil
			} else {
				err = errors.Join(err, reconcileErr)
			}
		}
		return sshKey{}, false, err
	}
	return res.SSHKey, true, nil
}

func (c *digitalOceanClient) reconcileSSHKey(name, publicKey string) (sshKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), createReconcileTimeout)
	defer cancel()

	var lastErr error
	for {
		keys, err := c.ListSSHKeys(ctx)
		if err == nil {
			for _, key := range keys {
				if key.Name != name {
					continue
				}
				if strings.TrimSpace(key.PublicKey) != strings.TrimSpace(publicKey) {
					return sshKey{}, core.Exit(3, "digitalocean ssh key %q exists with different public key", name)
				}
				return key, nil
			}
			lastErr = core.Exit(4, "digitalocean ssh key reconciliation did not find %q", name)
		} else {
			lastErr = err
		}

		timer := time.NewTimer(createReconcileInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return sshKey{}, errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
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

func (c *digitalOceanClient) ReplaceDropletTags(ctx context.Context, id int64, currentTags, desiredTags []string) error {
	currentTags = normalizeTags(currentTags)
	desiredTags = normalizeTags(desiredTags)
	current := make(map[string]bool, len(currentTags))
	for _, tag := range currentTags {
		current[tag] = true
	}
	desired := make(map[string]bool, len(desiredTags))
	for _, tag := range desiredTags {
		desired[tag] = true
	}
	resources := make([]map[string]any, 0, 1)
	resources = append(resources, map[string]any{"resource_id": strconv.FormatInt(id, 10), "resource_type": "droplet"})
	for _, tag := range desiredTags {
		if current[tag] {
			continue
		}
		if err := c.EnsureTag(ctx, tag); err != nil {
			return err
		}
		body := map[string]any{"resources": resources}
		if err := c.do(ctx, http.MethodPost, "/tags/"+url.PathEscape(tag)+"/resources", body, nil); err != nil {
			return err
		}
	}
	for _, tag := range currentTags {
		if tag == tagCrabbox || !strings.HasPrefix(tag, tagPrefix) || desired[tag] {
			continue
		}
		body := map[string]any{"resources": resources}
		if err := c.do(ctx, http.MethodDelete, "/tags/"+url.PathEscape(tag)+"/resources", body, nil); err != nil {
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
