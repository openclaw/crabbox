package vultr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

const vultrAPIBaseURL = "https://api.vultr.com/v2"

type vultrAPI interface {
	AccountID(context.Context) (string, error)
	ListCrabboxInstances(context.Context) ([]vultrInstance, error)
	GetInstance(context.Context, string) (vultrInstance, error)
	CreateInstance(context.Context, core.Config, string, string, string, bool, time.Time) (vultrInstance, error)
	DeleteInstance(context.Context, string) error
	FindSSHKeyByID(context.Context, string) (vultrSSHKey, bool, error)
	FindSSHKey(context.Context, string, string) (vultrSSHKey, bool, error)
	DeleteSSHKey(context.Context, string) error
	UpdateInstanceTags(context.Context, string, []string) error
}

type vultrClient struct {
	token   string
	client  *http.Client
	baseURL string
	sleep   func(context.Context, time.Duration) error
}

type vultrAPIError struct {
	Operation string
	Status    int
	Body      string
}

func (e *vultrAPIError) Error() string {
	return fmt.Sprintf("vultr %s: http %d: %s", e.Operation, e.Status, e.Body)
}

type ambiguousInstanceCreateError struct {
	err        error
	keyID      string
	keyCreated bool
}

func (e *ambiguousInstanceCreateError) Error() string {
	return fmt.Sprintf("vultr instance creation remains indeterminate; preserving SSH credentials for recovery: %v", e.err)
}

func (e *ambiguousInstanceCreateError) Unwrap() error { return e.err }

type ambiguousSSHKeyCreateError struct {
	err error
}

func (e *ambiguousSSHKeyCreateError) Error() string {
	return fmt.Sprintf("vultr SSH-key creation remains indeterminate; preserving credentials for recovery: %v", e.err)
}

func (e *ambiguousSSHKeyCreateError) Unwrap() error { return e.err }

type vultrSSHKeyConflictError struct {
	err error
}

func (e *vultrSSHKeyConflictError) Error() string { return e.err.Error() }
func (e *vultrSSHKeyConflictError) Unwrap() error { return e.err }

type vultrSSHKeyCleanupError struct {
	cause   error
	cleanup error
	keyID   string
}

func (e *vultrSSHKeyCleanupError) Error() string {
	return errors.Join(e.cause, e.cleanup).Error()
}

func (e *vultrSSHKeyCleanupError) Unwrap() []error {
	return []error{e.cause, e.cleanup}
}

type vultrInstance struct {
	ID              string   `json:"id"`
	MainIP          string   `json:"main_ip"`
	Status          string   `json:"status"`
	PowerStatus     string   `json:"power_status"`
	ServerStatus    string   `json:"server_status"`
	Label           string   `json:"label"`
	Hostname        string   `json:"hostname"`
	Region          string   `json:"region"`
	Plan            string   `json:"plan"`
	OSID            int      `json:"os_id"`
	ImageID         string   `json:"image_id"`
	SnapshotID      string   `json:"snapshot_id"`
	FirewallGroupID string   `json:"firewall_group_id"`
	Features        []string `json:"features"`
	Tags            []string `json:"tags"`
	UserScheme      string   `json:"user_scheme"`
	DefaultPassword string   `json:"default_password"`
	SSHKeyID        string   `json:"-"`
	SSHKeyCreated   bool     `json:"-"`
}

type vultrSSHKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SSHKey    string `json:"ssh_key"`
	PublicKey string `json:"public_key"`
}

type vultrOS struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Family string `json:"family"`
	Arch   string `json:"arch"`
}

type vultrAccount struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	UUID  string `json:"uuid"`
}

type vultrOrganization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func newVultrClient(rt core.Runtime) (*vultrClient, error) {
	token := strings.TrimSpace(os.Getenv("VULTR_API_KEY"))
	if token == "" {
		return nil, core.Exit(3, "VULTR_API_KEY is required")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &vultrClient{
		token:   token,
		client:  httpClient,
		baseURL: vultrAPIBaseURL,
		sleep: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}, nil
}

func (c *vultrClient) do(ctx context.Context, method, path string, body any, out any) error {
	return c.doAttempt(ctx, method, path, body, out, true)
}

func (c *vultrClient) doAttempt(ctx context.Context, method, path string, body any, out any, allowRetry bool) error {
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
	if resp.StatusCode == http.StatusTooManyRequests && allowRetry {
		delay := retryAfter(resp.Header.Get("Retry-After"))
		if delay > 0 {
			if err := c.sleep(ctx, delay); err != nil {
				return err
			}
			return c.doAttempt(ctx, method, path, body, out, false)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		body := strings.TrimSpace(string(data))
		if readErr != nil {
			if body != "" {
				body += "; "
			}
			body += "response body read failed: " + readErr.Error()
		}
		return &vultrAPIError{Operation: method + " " + redactPath(path), Status: resp.StatusCode, Body: redactVultr(body, c.token)}
	}
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("vultr %s %s response body: %w", method, redactPath(path), readErr)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("vultr %s %s decode: %w", method, redactPath(path), err)
	}
	return nil
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil {
		if seconds > 5*time.Second {
			return 5 * time.Second
		}
		return seconds
	}
	if when, err := http.ParseTime(value); err == nil {
		d := time.Until(when)
		if d < 0 {
			return 0
		}
		if d > 5*time.Second {
			return 5 * time.Second
		}
		return d
	}
	return 0
}

func (c *vultrClient) AccountID(ctx context.Context) (string, error) {
	var accountRes struct {
		Account vultrAccount `json:"account"`
	}
	if err := c.do(ctx, http.MethodGet, "/account", nil, &accountRes); err != nil {
		return "", err
	}
	if id := strings.TrimSpace(accountRes.Account.UUID); id != "" {
		return "account:" + id, nil
	}
	emailIdentity := ""
	if accountRes.Account.Email != "" {
		emailIdentity = "account-email-sha256:" + shortHash(accountRes.Account.Email)
	}
	orgs, err := c.listOrganizations(ctx)
	if err != nil {
		if emailIdentity != "" {
			return emailIdentity, nil
		}
		return "", err
	}
	if len(orgs) == 1 && strings.TrimSpace(orgs[0].ID) != "" {
		return "org:" + orgs[0].ID, nil
	}
	if emailIdentity != "" {
		return emailIdentity, nil
	}
	return "", core.Exit(3, "vultr account API returned no account identity")
}

func (c *vultrClient) listOrganizations(ctx context.Context) ([]vultrOrganization, error) {
	var orgs []vultrOrganization
	err := c.paginate(ctx, "/organizations", func(path string) (string, error) {
		var res struct {
			Organizations []vultrOrganization `json:"organizations"`
			Meta          vultrMeta           `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
			return "", err
		}
		orgs = append(orgs, res.Organizations...)
		return res.Meta.Links.Next, nil
	})
	return orgs, err
}

func (c *vultrClient) ListCrabboxInstances(ctx context.Context) ([]vultrInstance, error) {
	instances, err := c.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]vultrInstance, 0, len(instances))
	for _, item := range instances {
		if isOwnedInstance(item) {
			out = append(out, item)
		}
	}
	return out, nil
}

func (c *vultrClient) listInstances(ctx context.Context) ([]vultrInstance, error) {
	var out []vultrInstance
	err := c.paginate(ctx, "/instances", func(path string) (string, error) {
		var res struct {
			Instances []vultrInstance `json:"instances"`
			Meta      vultrMeta       `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
			return "", err
		}
		out = append(out, res.Instances...)
		return res.Meta.Links.Next, nil
	})
	return out, err
}

func (c *vultrClient) GetInstance(ctx context.Context, id string) (vultrInstance, error) {
	var res struct {
		Instance vultrInstance `json:"instance"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(id), nil, &res); err != nil {
		return vultrInstance{}, err
	}
	return res.Instance, nil
}

func (c *vultrClient) CreateInstance(ctx context.Context, cfg core.Config, publicKey, leaseID, slug string, keep bool, now time.Time) (vultrInstance, error) {
	keyName := providerKeyForLease(leaseID)
	key, keyCreated, err := c.EnsureSSHKey(ctx, keyName, publicKey)
	if err != nil {
		return vultrInstance{}, err
	}
	rollbackKey := func(cause error) error {
		if !keyCreated {
			return cause
		}
		if cleanupErr := c.DeleteSSHKey(context.Background(), key.ID); cleanupErr != nil {
			return &vultrSSHKeyCleanupError{
				cause:   cause,
				cleanup: fmt.Errorf("rollback vultr ssh key %s: %w", key.ID, cleanupErr),
				keyID:   key.ID,
			}
		}
		return cause
	}
	name := core.LeaseProviderName(leaseID, slug)
	body, err := c.createInstanceBody(ctx, cfg, publicKey, key.ID, leaseID, slug, keep, now)
	if err != nil {
		return vultrInstance{}, rollbackKey(err)
	}
	var res struct {
		Instance vultrInstance `json:"instance"`
	}
	if err := c.do(ctx, http.MethodPost, "/instances", body, &res); err != nil {
		if shouldReconcileVultrMutation(err) {
			return vultrInstance{}, &ambiguousInstanceCreateError{err: err, keyID: key.ID, keyCreated: keyCreated}
		}
		return vultrInstance{}, rollbackKey(err)
	}
	if strings.TrimSpace(res.Instance.ID) == "" || res.Instance.Label != name {
		return vultrInstance{}, &ambiguousInstanceCreateError{
			err:        fmt.Errorf("vultr create returned incomplete instance identity: id=%q label=%q, want label=%q", res.Instance.ID, res.Instance.Label, name),
			keyID:      key.ID,
			keyCreated: keyCreated,
		}
	}
	res.Instance.SSHKeyID = key.ID
	res.Instance.SSHKeyCreated = keyCreated
	return res.Instance, nil
}

func (c *vultrClient) createInstanceBody(ctx context.Context, cfg core.Config, publicKey, keyID, leaseID, slug string, keep bool, now time.Time) (map[string]any, error) {
	boot, err := c.bootSource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := core.LeaseProviderName(leaseID, slug)
	body := map[string]any{
		"region":           vultrRegion(cfg),
		"plan":             cfg.ServerType,
		"sshkey_id":        []string{keyID},
		"user_data":        base64.StdEncoding.EncodeToString([]byte(core.CloudInitUserData(cfg, publicKey))),
		"label":            name,
		"hostname":         name,
		"tags":             leaseTags(cfg, leaseID, slug, "provisioning", keep, now),
		"activation_email": false,
		"user_scheme":      vultrUserScheme(cfg),
	}
	for k, v := range boot {
		body[k] = v
	}
	if cfg.Vultr.FirewallGroup != "" {
		body["firewall_group_id"] = cfg.Vultr.FirewallGroup
	}
	if len(cfg.Vultr.VPCIDs) > 0 {
		body["attach_vpc"] = cfg.Vultr.VPCIDs
	}
	return body, nil
}

func (c *vultrClient) bootSource(ctx context.Context, cfg core.Config) (map[string]any, error) {
	count := 0
	out := map[string]any{}
	if cfg.Vultr.OS != "" {
		count++
		osID, err := strconv.Atoi(cfg.Vultr.OS)
		if err != nil || osID <= 0 {
			return nil, core.Exit(2, "provider=vultr invalid os id %q", cfg.Vultr.OS)
		}
		out["os_id"] = osID
	}
	if cfg.Vultr.Image != "" {
		count++
		out["image_id"] = cfg.Vultr.Image
	}
	if cfg.Vultr.Snapshot != "" {
		count++
		out["snapshot_id"] = cfg.Vultr.Snapshot
	}
	if count > 1 {
		return nil, core.Exit(2, "provider=vultr requires exactly one boot source; set only one of vultr.os, vultr.image, or vultr.snapshot")
	}
	if count == 1 {
		return out, nil
	}
	osID, err := c.resolveUbuntuOS(ctx)
	if err != nil {
		return nil, err
	}
	out["os_id"] = osID
	return out, nil
}

func (c *vultrClient) resolveUbuntuOS(ctx context.Context) (int, error) {
	var systems []vultrOS
	err := c.paginate(ctx, "/os", func(path string) (string, error) {
		var res struct {
			OS   []vultrOS `json:"os"`
			Meta vultrMeta `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
			return "", err
		}
		systems = append(systems, res.OS...)
		return res.Meta.Links.Next, nil
	})
	if err != nil {
		return 0, err
	}
	for _, os := range systems {
		name := strings.ToLower(os.Name)
		if strings.Contains(name, "ubuntu") && (strings.Contains(name, "24.04") || strings.Contains(name, "24 x64")) {
			return os.ID, nil
		}
	}
	return 0, core.Exit(3, "provider=vultr could not resolve a portable Ubuntu OS; set vultr.os, vultr.image, or vultr.snapshot")
}

func (c *vultrClient) DeleteInstance(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/instances/"+url.PathEscape(id), nil, nil)
	if isVultrNotFound(err) {
		return nil
	}
	return err
}

func (c *vultrClient) ListSSHKeys(ctx context.Context) ([]vultrSSHKey, error) {
	var out []vultrSSHKey
	err := c.paginate(ctx, "/ssh-keys", func(path string) (string, error) {
		var res struct {
			SSHKeys []vultrSSHKey `json:"ssh_keys"`
			Meta    vultrMeta     `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
			return "", err
		}
		out = append(out, res.SSHKeys...)
		return res.Meta.Links.Next, nil
	})
	return out, err
}

func (c *vultrClient) EnsureSSHKey(ctx context.Context, name, publicKey string) (vultrSSHKey, bool, error) {
	keys, err := c.ListSSHKeys(ctx)
	if err != nil {
		return vultrSSHKey{}, false, err
	}
	if key, found, err := selectVultrSSHKey(keys, name, publicKey); err != nil {
		return vultrSSHKey{}, false, err
	} else if found {
		return key, false, nil
	}
	body := map[string]any{"name": name, "ssh_key": publicKey}
	var res struct {
		SSHKey vultrSSHKey `json:"ssh_key"`
	}
	if err := c.do(ctx, http.MethodPost, "/ssh-keys", body, &res); err != nil {
		if shouldReconcileVultrMutation(err) {
			return vultrSSHKey{}, false, &ambiguousSSHKeyCreateError{err: err}
		}
		return vultrSSHKey{}, false, err
	}
	if res.SSHKey.ID == "" || res.SSHKey.Name != name || normalizedSSHKey(res.SSHKey) != strings.TrimSpace(publicKey) {
		return vultrSSHKey{}, false, &ambiguousSSHKeyCreateError{err: fmt.Errorf("vultr ssh key creation returned incomplete identity: id=%q name=%q", res.SSHKey.ID, res.SSHKey.Name)}
	}
	return res.SSHKey, true, nil
}

func (c *vultrClient) FindSSHKey(ctx context.Context, name, publicKey string) (vultrSSHKey, bool, error) {
	keys, err := c.ListSSHKeys(ctx)
	if err != nil {
		return vultrSSHKey{}, false, err
	}
	return selectVultrSSHKey(keys, name, publicKey)
}

func (c *vultrClient) FindSSHKeyByID(ctx context.Context, id string) (vultrSSHKey, bool, error) {
	keys, err := c.ListSSHKeys(ctx)
	if err != nil {
		return vultrSSHKey{}, false, err
	}
	for _, key := range keys {
		if key.ID == id {
			return key, true, nil
		}
	}
	return vultrSSHKey{}, false, nil
}

func selectVultrSSHKey(keys []vultrSSHKey, name, publicKey string) (vultrSSHKey, bool, error) {
	publicKey = strings.TrimSpace(publicKey)
	var match vultrSSHKey
	nameMatches := 0
	publicKeyMatches := 0
	for _, key := range keys {
		if key.Name != name {
			continue
		}
		nameMatches++
		if publicKey == "" {
			continue
		}
		if normalizedSSHKey(key) != publicKey {
			continue
		}
		match = key
		publicKeyMatches++
	}
	switch {
	case publicKey == "" && nameMatches > 0:
		return vultrSSHKey{}, true, nil
	case publicKeyMatches == 1:
		return match, true, nil
	case publicKeyMatches > 1:
		return vultrSSHKey{}, false, core.Exit(4, "vultr SSH key %q has multiple entries matching the retained public key", name)
	case nameMatches > 0:
		return vultrSSHKey{}, false, &vultrSSHKeyConflictError{err: core.Exit(3, "vultr ssh key %q exists with different public key", name)}
	default:
		return vultrSSHKey{}, false, nil
	}
}

func normalizedSSHKey(key vultrSSHKey) string {
	if key.PublicKey != "" {
		return strings.TrimSpace(key.PublicKey)
	}
	return strings.TrimSpace(key.SSHKey)
}

func (c *vultrClient) DeleteSSHKey(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/ssh-keys/"+url.PathEscape(id), nil, nil)
	if isVultrNotFound(err) {
		return nil
	}
	return err
}

func (c *vultrClient) UpdateInstanceTags(ctx context.Context, id string, tags []string) error {
	body := map[string]any{"tags": normalizeTags(tags)}
	return c.do(ctx, http.MethodPatch, "/instances/"+url.PathEscape(id), body, nil)
}

type vultrMeta struct {
	Links struct {
		Next string `json:"next"`
		Prev string `json:"prev"`
	} `json:"links"`
}

func (c *vultrClient) paginate(ctx context.Context, path string, fetch func(string) (string, error)) error {
	for {
		next, err := fetch(path)
		if err != nil {
			return err
		}
		if strings.TrimSpace(next) == "" {
			return nil
		}
		parsed, err := url.Parse(next)
		if err != nil {
			return fmt.Errorf("vultr pagination next %q: %w", next, err)
		}
		path = parsed.RequestURI()
		if path == "" {
			return fmt.Errorf("vultr pagination next %q has no request URI", next)
		}
	}
}

func shouldReconcileVultrMutation(err error) bool {
	var apiErr *vultrAPIError
	return !errors.As(err, &apiErr) || apiErr.Status >= http.StatusInternalServerError
}

func isVultrNotFound(err error) bool {
	var apiErr *vultrAPIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func redactPath(path string) string {
	if strings.Contains(path, "user_data") {
		return "<redacted-path>"
	}
	return path
}

func redactVultr(value, token string) string {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "default_password") || strings.Contains(lower, "user_data") {
		return "[redacted]"
	}
	for _, secret := range []string{token, "default_password", "user_data"} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])[:16]
}

func vultrRegion(cfg core.Config) string {
	if cfg.Vultr.Region != "" {
		return cfg.Vultr.Region
	}
	if cfg.Location != "" {
		return cfg.Location
	}
	return "ewr"
}

func vultrUserScheme(cfg core.Config) string {
	if cfg.Vultr.UserScheme != "" {
		return cfg.Vultr.UserScheme
	}
	return "root"
}

func providerKeyForLease(leaseID string) string {
	return "crabbox-" + strings.ReplaceAll(leaseID, "_", "-")
}
