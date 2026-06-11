package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	token             string
	client            *http.Client
	baseURL           string
	reconcileTimeout  time.Duration
	reconcileInterval time.Duration
}

type ambiguousDropletCreateError struct {
	err               error
	keyID             int64
	keyCreated        bool
	keyOwnershipKnown bool
}

func (e *ambiguousDropletCreateError) Error() string {
	return fmt.Sprintf("digitalocean droplet creation remains indeterminate; preserving SSH credentials for recovery: %v", e.err)
}

func (e *ambiguousDropletCreateError) Unwrap() error {
	return e.err
}

type ambiguousSSHKeyCreateError struct {
	err error
}

func (e *ambiguousSSHKeyCreateError) Error() string {
	return fmt.Sprintf("digitalocean SSH-key creation remains indeterminate; preserving credentials for recovery: %v", e.err)
}

func (e *ambiguousSSHKeyCreateError) Unwrap() error {
	return e.err
}

type sshKeyConflictError struct {
	err error
}

func (e *sshKeyConflictError) Error() string {
	return e.err.Error()
}

func (e *sshKeyConflictError) Unwrap() error {
	return e.err
}

type sshKeyCleanupError struct {
	cause   error
	cleanup error
	keyID   int64
}

func (e *sshKeyCleanupError) Error() string {
	return errors.Join(e.cause, e.cleanup).Error()
}

func (e *sshKeyCleanupError) Unwrap() []error {
	return []error{e.cause, e.cleanup}
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
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	Tags          []string `json:"tags"`
	SSHKeyID      int64    `json:"-"`
	SSHKeyCreated bool     `json:"-"`
	Networks      struct {
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

type digitalOceanTag struct {
	Name string `json:"name"`
}

type digitalOceanAccount struct {
	UUID string `json:"uuid"`
	Team struct {
		UUID string `json:"uuid"`
	} `json:"team"`
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
	droplets, err := c.listDroplets(ctx)
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

func (c *digitalOceanClient) listDroplets(ctx context.Context) ([]droplet, error) {
	return c.listDropletsWithTag(ctx, "")
}

func (c *digitalOceanClient) listDropletsByTag(ctx context.Context, tag string) ([]droplet, error) {
	return c.listDropletsWithTag(ctx, tag)
}

func (c *digitalOceanClient) listDropletsWithTag(ctx context.Context, tag string) ([]droplet, error) {
	var out []droplet
	seen := map[int64]bool{}
	for _, dropletType := range []string{"", "gpus"} {
		for page := 1; ; page++ {
			q := url.Values{}
			q.Set("page", strconv.Itoa(page))
			q.Set("per_page", "200")
			if tag != "" && dropletType == "" {
				q.Set("tag_name", tag)
			}
			if dropletType != "" {
				q.Set("type", dropletType)
			}
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
				if tag != "" && dropletType == "gpus" && !containsTagFold(item.Tags, tag) {
					continue
				}
				if !seen[item.ID] {
					seen[item.ID] = true
					out = append(out, item)
				}
			}
			if res.Links.Pages.Next == "" {
				break
			}
		}
	}
	return out, nil
}

func containsTagFold(tags []string, want string) bool {
	for _, tag := range tags {
		if strings.EqualFold(tag, want) {
			return true
		}
	}
	return false
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

func (c *digitalOceanClient) AccountID(ctx context.Context) (string, error) {
	var res struct {
		Account digitalOceanAccount `json:"account"`
	}
	if err := c.do(ctx, http.MethodGet, "/account", nil, &res); err != nil {
		return "", err
	}
	if id := strings.TrimSpace(res.Account.Team.UUID); id != "" {
		return "team:" + id, nil
	}
	if id := strings.TrimSpace(res.Account.UUID); id != "" {
		return "user:" + id, nil
	}
	return "", core.Exit(3, "digitalocean account API returned no account identity")
}

func (c *digitalOceanClient) CreateDroplet(ctx context.Context, cfg core.Config, publicKey, leaseID, slug string, keep bool, now time.Time) (droplet, error) {
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, cfg.Provider)
	}
	tags := leaseTags(cfg, leaseID, slug, "provisioning", keep, now)
	leaseTag := encodeTagKV("lease", leaseID)
	leaseTagResolved := false
	keyName := providerKeyForLease(leaseID)
	key, createdKey, err := c.EnsureSSHKey(ctx, keyName, publicKey)
	if err != nil {
		return droplet{}, err
	}
	rollbackKey := func(cause error) error {
		if !createdKey {
			return cause
		}
		return c.rollbackCreatedSSHKey(key, cause)
	}
	withKeyIdentity := func(item droplet) droplet {
		item.SSHKeyID = key.ID
		item.SSHKeyCreated = createdKey
		return item
	}
	name := core.LeaseProviderName(leaseID, slug)
	body := map[string]any{
		"name":      name,
		"region":    digitalOceanRegion(cfg),
		"size":      cfg.ServerType,
		"image":     digitalOceanImage(cfg),
		"ssh_keys":  []any{key.ID},
		"tags":      tags,
		"user_data": core.CloudInitUserData(cfg, publicKey),
		"ipv6":      false,
	}
	if cfg.DigitalOcean.VPCUUID != "" {
		body["vpc_uuid"] = cfg.DigitalOcean.VPCUUID
	}
	var res struct {
		Droplet droplet `json:"droplet"`
	}
	createErr := c.do(ctx, http.MethodPost, "/droplets", body, &res)
	if isDigitalOceanUnprocessable(createErr) {
		resolvedTags, resolvedLeaseTag, changed, resolveErr := c.resolveCreateTagConflict(ctx, tags, leaseID)
		if resolveErr != nil {
			return droplet{}, rollbackKey(errors.Join(createErr, resolveErr))
		}
		if changed {
			body["tags"] = resolvedTags
			leaseTag = resolvedLeaseTag
			leaseTagResolved = true
			createErr = c.do(ctx, http.MethodPost, "/droplets", body, &res)
		}
	}
	if createErr != nil {
		if shouldReconcileMutation(createErr) {
			if item, reconcileErr := c.reconcileDropletCreate(leaseTag, leaseTagResolved, leaseID, name); reconcileErr == nil {
				return withKeyIdentity(item), nil
			} else {
				return droplet{}, &ambiguousDropletCreateError{
					err:               errors.Join(createErr, reconcileErr),
					keyID:             key.ID,
					keyCreated:        createdKey,
					keyOwnershipKnown: true,
				}
			}
		}
		return droplet{}, rollbackKey(createErr)
	}
	if res.Droplet.ID <= 0 || res.Droplet.Name != name {
		incomplete := fmt.Errorf(
			"digitalocean create returned incomplete droplet identity: id=%d name=%q, want name=%q",
			res.Droplet.ID,
			res.Droplet.Name,
			name,
		)
		if item, reconcileErr := c.reconcileDropletCreate(leaseTag, leaseTagResolved, leaseID, name); reconcileErr == nil {
			return withKeyIdentity(item), nil
		} else {
			return droplet{}, &ambiguousDropletCreateError{
				err:               errors.Join(incomplete, reconcileErr),
				keyID:             key.ID,
				keyCreated:        createdKey,
				keyOwnershipKnown: true,
			}
		}
	}
	return withKeyIdentity(res.Droplet), nil
}

func shouldReconcileMutation(err error) bool {
	var apiErr *digitalOceanAPIError
	return !errors.As(err, &apiErr) || apiErr.Status >= http.StatusInternalServerError
}

func isDigitalOceanUnprocessable(err error) bool {
	var apiErr *digitalOceanAPIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity
}

func (c *digitalOceanClient) reconcileDropletCreate(leaseTag string, leaseTagResolved bool, leaseID, name string) (droplet, error) {
	timeout := c.reconcileTimeout
	if timeout <= 0 {
		timeout = createReconcileTimeout
	}
	interval := c.reconcileInterval
	if interval <= 0 {
		interval = createReconcileInterval
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if !leaseTagResolved {
		canonical, err := c.resolveCanonicalLeaseTag(ctx, leaseID)
		if err != nil {
			return droplet{}, err
		}
		leaseTag = canonical
	}
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

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return droplet{}, errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func (c *digitalOceanClient) rollbackCreatedSSHKey(key sshKey, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cleanupErr := c.DeleteSSHKey(ctx, key.ID); cleanupErr != nil {
		return &sshKeyCleanupError{
			cause:   cause,
			cleanup: fmt.Errorf("rollback digitalocean ssh key %d: %w", key.ID, cleanupErr),
			keyID:   key.ID,
		}
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
	if key, found, err := selectSSHKey(keys, name, publicKey); err != nil {
		return sshKey{}, false, err
	} else if found {
		return key, false, nil
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
				var conflict *sshKeyConflictError
				if errors.As(reconcileErr, &conflict) {
					return sshKey{}, false, conflict
				}
				return sshKey{}, false, &ambiguousSSHKeyCreateError{err: errors.Join(err, reconcileErr)}
			}
		}
		return sshKey{}, false, err
	}
	if res.SSHKey.ID <= 0 ||
		res.SSHKey.Name != name ||
		strings.TrimSpace(res.SSHKey.PublicKey) != strings.TrimSpace(publicKey) {
		key, reconcileErr := c.reconcileSSHKey(name, publicKey)
		if reconcileErr != nil {
			var conflict *sshKeyConflictError
			if errors.As(reconcileErr, &conflict) {
				return sshKey{}, false, conflict
			}
			return sshKey{}, false, &ambiguousSSHKeyCreateError{err: errors.Join(
				fmt.Errorf("digitalocean ssh key creation returned incomplete identity: id=%d name=%q", res.SSHKey.ID, res.SSHKey.Name),
				reconcileErr,
			)}
		}
		return key, true, nil
	}
	return res.SSHKey, true, nil
}

func newSSHKeyConflictError(name string) error {
	return &sshKeyConflictError{err: core.Exit(3, "digitalocean ssh key %q exists with different public key", name)}
}

func (c *digitalOceanClient) reconcileSSHKey(name, publicKey string) (sshKey, error) {
	timeout := c.reconcileTimeout
	if timeout <= 0 {
		timeout = createReconcileTimeout
	}
	interval := c.reconcileInterval
	if interval <= 0 {
		interval = createReconcileInterval
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastErr error
	for {
		keys, err := c.ListSSHKeys(ctx)
		if err == nil {
			if key, found, selectErr := selectSSHKey(keys, name, publicKey); selectErr != nil {
				return sshKey{}, selectErr
			} else if found {
				return key, nil
			}
			lastErr = core.Exit(4, "digitalocean ssh key reconciliation did not find %q", name)
		} else {
			lastErr = err
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return sshKey{}, errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func selectSSHKey(keys []sshKey, name, publicKey string) (sshKey, bool, error) {
	publicKey = strings.TrimSpace(publicKey)
	var match sshKey
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
		if strings.TrimSpace(key.PublicKey) != publicKey {
			continue
		}
		match = key
		publicKeyMatches++
	}
	switch {
	case publicKey == "" && nameMatches > 0:
		return sshKey{}, true, nil
	case publicKeyMatches == 1:
		return match, true, nil
	case publicKeyMatches > 1:
		return sshKey{}, false, core.Exit(4, "digitalocean SSH key %q has multiple entries matching the retained public key", name)
	case nameMatches > 0:
		return sshKey{}, false, newSSHKeyConflictError(name)
	default:
		return sshKey{}, false, nil
	}
}

func (c *digitalOceanClient) FindSSHKey(ctx context.Context, name, publicKey string) (sshKey, bool, error) {
	keys, err := c.ListSSHKeys(ctx)
	if err != nil {
		return sshKey{}, false, err
	}
	return selectSSHKey(keys, name, publicKey)
}

func (c *digitalOceanClient) DeleteSSHKey(ctx context.Context, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/account/keys/%d", id), nil, nil)
	if isDigitalOceanNotFound(err) {
		return nil
	}
	return err
}

func (c *digitalOceanClient) ListTags(ctx context.Context) ([]digitalOceanTag, error) {
	var out []digitalOceanTag
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", "200")
		var res struct {
			Tags  []digitalOceanTag `json:"tags"`
			Links struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := c.do(ctx, http.MethodGet, "/tags?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Tags...)
		if res.Links.Pages.Next == "" {
			return out, nil
		}
	}
}

func canonicalTagNames(tags []digitalOceanTag) map[string]string {
	names := make(map[string]string, len(tags))
	for _, tag := range tags {
		if name := strings.TrimSpace(tag.Name); name != "" {
			names[strings.ToLower(name)] = name
		}
	}
	return names
}

func (c *digitalOceanClient) resolveCreateTagConflict(ctx context.Context, tags []string, leaseID string) ([]string, string, bool, error) {
	existing, err := c.ListTags(ctx)
	if err != nil {
		return nil, "", false, err
	}
	known := canonicalTagNames(existing)
	canonical := make([]string, 0, len(tags))
	leaseTag := encodeTagKV("lease", leaseID)
	changed := false
	for _, tag := range tags {
		name := tag
		if existingName := known[strings.ToLower(tag)]; existingName != "" {
			name, err = canonicalTagName(tag, existingName)
			if err != nil {
				return nil, "", false, err
			}
			changed = changed || name != tag
		}
		if labelsFromTags([]string{name})["lease"] == leaseID {
			leaseTag = name
		}
		canonical = append(canonical, name)
	}
	return normalizeTags(canonical), leaseTag, changed, nil
}

func (c *digitalOceanClient) resolveCanonicalLeaseTag(ctx context.Context, leaseID string) (string, error) {
	requested := encodeTagKV("lease", leaseID)
	tags, err := c.ListTags(ctx)
	if err != nil {
		return "", err
	}
	if existing := canonicalTagNames(tags)[strings.ToLower(requested)]; existing != "" {
		return canonicalTagName(requested, existing)
	}
	return requested, nil
}

func canonicalTagName(requested, existing string) (string, error) {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		existing = requested
	}
	requestedLabels := labelsFromTags([]string{requested})
	if len(requestedLabels) == 0 || !maps.Equal(requestedLabels, labelsFromTags([]string{existing})) {
		return "", core.Exit(2, "digitalocean tag %q conflicts with existing account tag %q", requested, existing)
	}
	return existing, nil
}

func (c *digitalOceanClient) GetTag(ctx context.Context, name string) (digitalOceanTag, bool, error) {
	var res struct {
		Tag digitalOceanTag `json:"tag"`
	}
	err := c.do(ctx, http.MethodGet, "/tags/"+url.PathEscape(name), nil, &res)
	if isDigitalOceanNotFound(err) {
		return digitalOceanTag{}, false, nil
	}
	if err != nil {
		return digitalOceanTag{}, false, err
	}
	return res.Tag, true, nil
}

func (c *digitalOceanClient) EnsureTag(ctx context.Context, tag string, known map[string]string) (string, error) {
	key := strings.ToLower(tag)
	if canonical := known[key]; canonical != "" {
		return canonicalTagName(tag, canonical)
	}
	if existing, ok, err := c.GetTag(ctx, tag); err != nil {
		return "", err
	} else if ok {
		canonical, canonicalErr := canonicalTagName(tag, existing.Name)
		if canonicalErr != nil {
			return "", canonicalErr
		}
		known[key] = canonical
		return canonical, nil
	}
	var res struct {
		Tag digitalOceanTag `json:"tag"`
	}
	err := c.do(ctx, http.MethodPost, "/tags", map[string]any{"name": tag}, &res)
	if err == nil {
		canonical, canonicalErr := canonicalTagName(tag, res.Tag.Name)
		if canonicalErr != nil {
			return "", canonicalErr
		}
		known[key] = canonical
		return canonical, nil
	}
	apiErr, duplicate := err.(*digitalOceanAPIError)
	if !duplicate || apiErr.Status != http.StatusUnprocessableEntity {
		return "", err
	}
	tags, listErr := c.ListTags(ctx)
	if listErr != nil {
		return "", errors.Join(err, fmt.Errorf("resolve canonical digitalocean tag %q: %w", tag, listErr))
	}
	for folded, canonical := range canonicalTagNames(tags) {
		known[folded] = canonical
	}
	if canonical := known[key]; canonical != "" {
		return canonicalTagName(tag, canonical)
	}
	return "", err
}

func (c *digitalOceanClient) ReplaceDropletTags(ctx context.Context, id int64, currentTags, desiredTags []string) error {
	currentTags = normalizeTags(currentTags)
	desiredTags = normalizeTags(desiredTags)
	current := make(map[string]bool, len(currentTags))
	for _, tag := range currentTags {
		current[strings.ToLower(tag)] = true
	}
	desired := make(map[string]bool, len(desiredTags))
	for _, tag := range desiredTags {
		desired[strings.ToLower(tag)] = true
	}
	known := map[string]string{}
	resources := make([]map[string]any, 0, 1)
	resources = append(resources, map[string]any{"resource_id": strconv.FormatInt(id, 10), "resource_type": "droplet"})
	for _, tag := range desiredTags {
		if current[strings.ToLower(tag)] {
			continue
		}
		canonical, err := c.EnsureTag(ctx, tag, known)
		if err != nil {
			return err
		}
		body := map[string]any{"resources": resources}
		if err := c.do(ctx, http.MethodPost, "/tags/"+url.PathEscape(canonical)+"/resources", body, nil); err != nil {
			return err
		}
	}
	for _, tag := range currentTags {
		lowerTag := strings.ToLower(tag)
		if lowerTag == tagCrabbox || !strings.HasPrefix(lowerTag, tagPrefix) || desired[lowerTag] {
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
