package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ProxmoxClient struct {
	BaseURL     string
	TokenID     string
	TokenSecret string
	Node        string
	Client      *http.Client
}

type ProxmoxReadinessCheck struct {
	Status  string
	Check   string
	Message string
	Details map[string]string
}

var (
	proxmoxRunSSHQuietWithOptions = runSSHQuietWithOptions
	proxmoxRunSSHInputQuiet       = runSSHInputQuiet
	proxmoxAPITokenPattern        = regexp.MustCompile(`PVEAPIToken=[A-Za-z0-9@._!%+=:/~-]+`)
)

type ProxmoxError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *ProxmoxError) Error() string {
	return fmt.Sprintf("proxmox %s %s: http %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

type ProxmoxDeleteTaskError struct {
	Err error
}

func (e *ProxmoxDeleteTaskError) Error() string { return e.Err.Error() }
func (e *ProxmoxDeleteTaskError) Unwrap() error { return e.Err }

type ProxmoxDeleteRequestError struct {
	Err error
}

func (e *ProxmoxDeleteRequestError) Error() string { return e.Err.Error() }
func (e *ProxmoxDeleteRequestError) Unwrap() error { return e.Err }

type proxmoxTaskWaitError struct {
	err error
}

func (e *proxmoxTaskWaitError) Error() string { return e.err.Error() }
func (e *proxmoxTaskWaitError) Unwrap() error { return e.err }

func NewProxmoxClient(cfg Config) (*ProxmoxClient, error) {
	apiURL := strings.TrimSpace(cfg.Proxmox.APIURL)
	if apiURL == "" {
		return nil, exit(3, "proxmox apiUrl is required (set proxmox.apiUrl or CRABBOX_PROXMOX_API_URL)")
	}
	apiURL = strings.TrimRight(apiURL, "/")
	apiURL = strings.TrimSuffix(apiURL, "/api2/json")
	if cfg.Proxmox.TokenID == "" || cfg.Proxmox.TokenSecret == "" {
		return nil, exit(3, "proxmox tokenId/tokenSecret are required (set proxmox.tokenId/tokenSecret or CRABBOX_PROXMOX_TOKEN_ID/CRABBOX_PROXMOX_TOKEN_SECRET)")
	}
	if cfg.Proxmox.Node == "" {
		return nil, exit(3, "proxmox node is required (set proxmox.node or CRABBOX_PROXMOX_NODE)")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	if cfg.Proxmox.InsecureTLS {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // User opt-in for self-signed private Proxmox clusters.
		client.Transport = transport
	}
	return &ProxmoxClient{
		BaseURL:     apiURL,
		TokenID:     cfg.Proxmox.TokenID,
		TokenSecret: cfg.Proxmox.TokenSecret,
		Node:        cfg.Proxmox.Node,
		Client:      client,
	}, nil
}

func (c *ProxmoxClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	return c.doEnvelope(ctx, method, path, form, out, false)
}

func (c *ProxmoxClient) doRequired(ctx context.Context, method, path string, form url.Values, out any) error {
	return c.doEnvelope(ctx, method, path, form, out, true)
}

func (c *ProxmoxClient) doEnvelope(ctx context.Context, method, path string, form url.Values, out any, requireData bool) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api2/json"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.TokenID+"="+c.TokenSecret)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
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
		return &ProxmoxError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: summarizeJSON([]byte(c.redactErrorBody(string(data))))}
	}
	if out == nil {
		return nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
		if !requireData {
			return nil
		}
		return fmt.Errorf("proxmox %s %s: missing required data in response", method, path)
	}
	return json.Unmarshal(envelope.Data, out)
}

func (c *ProxmoxClient) redactErrorBody(body string) string {
	redacted := body
	if strings.TrimSpace(c.TokenID) != "" && strings.TrimSpace(c.TokenSecret) != "" {
		redacted = strings.ReplaceAll(redacted, "PVEAPIToken="+c.TokenID+"="+c.TokenSecret, "PVEAPIToken=<redacted>")
	}
	for _, secret := range []string{c.TokenID, c.TokenSecret} {
		if strings.TrimSpace(secret) != "" {
			redacted = strings.ReplaceAll(redacted, secret, "<redacted>")
		}
	}
	return proxmoxAPITokenPattern.ReplaceAllString(redacted, "PVEAPIToken=<redacted>")
}

type proxmoxStorage struct {
	Storage string     `json:"storage"`
	Active  proxmoxInt `json:"active"`
	Enabled proxmoxInt `json:"enabled"`
	Content string     `json:"content"`
}

type proxmoxNetwork struct {
	Iface  string     `json:"iface"`
	Type   string     `json:"type"`
	Active proxmoxInt `json:"active"`
}

type proxmoxInt int

func (i *proxmoxInt) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*i = proxmoxInt(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*i = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*i = proxmoxInt(n)
	return nil
}

func (c *ProxmoxClient) DoctorReadiness(ctx context.Context, cfg Config) ([]ProxmoxReadinessCheck, error) {
	checks := []ProxmoxReadinessCheck{
		c.proxmoxAuthCheck(ctx),
		c.proxmoxNodeCheck(ctx, cfg),
		c.proxmoxStorageCheck(ctx, cfg),
		c.proxmoxNetworkCheck(ctx, cfg),
		c.proxmoxTemplateCheck(ctx, cfg),
		c.proxmoxNextIDCheck(ctx),
	}
	if cfg.Proxmox.Pool != "" {
		checks = append(checks, c.proxmoxPoolCheck(ctx, cfg))
	}
	checks = append(checks,
		c.proxmoxInventoryCheck(ctx),
		ProxmoxReadinessCheck{
			Status:  "ok",
			Check:   "mutation",
			Message: "mutation=false",
			Details: map[string]string{"mutation": "false"},
		},
	)
	return checks, nil
}

func (c *ProxmoxClient) proxmoxAuthCheck(ctx context.Context) ProxmoxReadinessCheck {
	path := "/version"
	var version map[string]any
	if err := c.doRequired(ctx, http.MethodGet, path, nil, &version); err != nil {
		return c.proxmoxFailedReadiness("auth", path, err, nil)
	}
	return ProxmoxReadinessCheck{Status: "ok", Check: "auth", Message: "auth=ready endpoint=/version", Details: map[string]string{"auth": "ready", "endpoint": path}}
}

func (c *ProxmoxClient) proxmoxNodeCheck(ctx context.Context, cfg Config) ProxmoxReadinessCheck {
	path := "/nodes/" + url.PathEscape(c.Node) + "/status"
	var status map[string]any
	if err := c.doRequired(ctx, http.MethodGet, path, nil, &status); err != nil {
		return c.proxmoxFailedReadiness("node", path, err, map[string]string{"node": cfg.Proxmox.Node})
	}
	return ProxmoxReadinessCheck{
		Status:  "ok",
		Check:   "node",
		Message: fmt.Sprintf("node=%s endpoint=/nodes/%s/status", cfg.Proxmox.Node, cfg.Proxmox.Node),
		Details: map[string]string{"node": cfg.Proxmox.Node, "endpoint": "/nodes/" + cfg.Proxmox.Node + "/status"},
	}
}

func (c *ProxmoxClient) proxmoxStorageCheck(ctx context.Context, cfg Config) ProxmoxReadinessCheck {
	path := "/nodes/" + url.PathEscape(c.Node) + "/storage"
	var storages []proxmoxStorage
	if err := c.doRequired(ctx, http.MethodGet, path, nil, &storages); err != nil {
		return c.proxmoxFailedReadiness("storage", path, err, map[string]string{"storage": cfg.Proxmox.Storage})
	}
	var destination *ProxmoxReadinessCheck
	if cfg.Proxmox.Storage == "" {
		usable := 0
		for _, storage := range storages {
			if storage.Enabled != 0 && storage.Active != 0 && proxmoxStorageSupportsImages(storage.Content) {
				usable++
			}
		}
		if usable == 0 {
			return ProxmoxReadinessCheck{
				Status:  "failed",
				Check:   "storage",
				Message: fmt.Sprintf("storage=any class=missing_resource hint=grant_or_enable_proxmox_storage count=%d usable=0", len(storages)),
				Details: map[string]string{"storage": "any", "class": "missing_resource", "hint": "grant_or_enable_proxmox_storage", "count": strconv.Itoa(len(storages)), "usable": "0", "endpoint": "/nodes/" + cfg.Proxmox.Node + "/storage"},
			}
		}
	} else {
		check := proxmoxNamedStorageReadiness(cfg.Proxmox.Storage, "images", storages, "/nodes/"+cfg.Proxmox.Node+"/storage")
		if check.Status != "ok" {
			return check
		}
		destination = &check
	}
	required, failure := c.proxmoxTemplateStorageReadiness(ctx, cfg, storages)
	if failure != nil {
		return *failure
	}
	requiredNames := proxmoxTemplateStorageNames(required)
	if destination != nil {
		destination.Details["source"] = "configured"
		destination.Details["templateStorages"] = strings.Join(requiredNames, ",")
		return *destination
	}
	return ProxmoxReadinessCheck{
		Status:  "ok",
		Check:   "storage",
		Message: fmt.Sprintf("storage=%s source=template active=1 enabled=1", strings.Join(requiredNames, ",")),
		Details: map[string]string{"storage": strings.Join(requiredNames, ","), "source": "template", "active": "1", "enabled": "1", "endpoint": "/nodes/" + cfg.Proxmox.Node + "/storage"},
	}
}

func (c *ProxmoxClient) proxmoxTemplateStorageReadiness(ctx context.Context, cfg Config, storages []proxmoxStorage) ([]proxmoxTemplateStorageRequirement, *ProxmoxReadinessCheck) {
	if cfg.Proxmox.TemplateID <= 0 {
		check := ProxmoxReadinessCheck{
			Status:  "failed",
			Check:   "storage",
			Message: "storage=template class=config hint=set_proxmox_template_id",
			Details: map[string]string{"storage": "template", "class": "config", "hint": "set_proxmox_template_id"},
		}
		return nil, &check
	}
	configPath := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(c.Node), cfg.Proxmox.TemplateID)
	var config map[string]any
	if err := c.doRequired(ctx, http.MethodGet, configPath, nil, &config); err != nil {
		check := c.proxmoxFailedReadiness("storage", configPath, err, map[string]string{"storage": "template"})
		return nil, &check
	}
	required := proxmoxTemplateStorages(config)
	if len(required) == 0 {
		check := ProxmoxReadinessCheck{
			Status:  "failed",
			Check:   "storage",
			Message: fmt.Sprintf("storage=template class=missing_resource hint=configure_proxmox_template_storage templateId=%d", cfg.Proxmox.TemplateID),
			Details: map[string]string{"storage": "template", "class": "missing_resource", "hint": "configure_proxmox_template_storage", "templateId": strconv.Itoa(cfg.Proxmox.TemplateID), "endpoint": proxmoxDisplayPath(configPath)},
		}
		return nil, &check
	}
	for _, name := range required {
		check := proxmoxNamedStorageReadiness(name.Name, name.Content, storages, "/nodes/"+cfg.Proxmox.Node+"/storage")
		check.Details["source"] = "template"
		if check.Status != "ok" {
			return nil, &check
		}
	}
	return required, nil
}

func proxmoxNamedStorageReadiness(name, requiredContent string, storages []proxmoxStorage, endpoint string) ProxmoxReadinessCheck {
	for _, storage := range storages {
		if storage.Storage != name {
			continue
		}
		if storage.Enabled == 0 || storage.Active == 0 {
			return ProxmoxReadinessCheck{
				Status:  "failed",
				Check:   "storage",
				Message: fmt.Sprintf("storage=%s class=missing_resource hint=enable_proxmox_storage active=%d enabled=%d", name, storage.Active, storage.Enabled),
				Details: map[string]string{"storage": name, "class": "missing_resource", "hint": "enable_proxmox_storage", "active": strconv.Itoa(int(storage.Active)), "enabled": strconv.Itoa(int(storage.Enabled)), "endpoint": endpoint},
			}
		}
		if !proxmoxStorageSupportsContent(storage.Content, requiredContent) {
			hint := "enable_proxmox_storage_" + requiredContent
			return ProxmoxReadinessCheck{
				Status:  "failed",
				Check:   "storage",
				Message: fmt.Sprintf("storage=%s class=missing_resource hint=%s", name, hint),
				Details: map[string]string{"storage": name, "class": "missing_resource", "hint": hint, "content": storage.Content, "requiredContent": requiredContent, "endpoint": endpoint},
			}
		}
		return ProxmoxReadinessCheck{
			Status:  "ok",
			Check:   "storage",
			Message: fmt.Sprintf("storage=%s active=1 enabled=1", name),
			Details: map[string]string{"storage": name, "active": "1", "enabled": "1", "endpoint": endpoint},
		}
	}
	return ProxmoxReadinessCheck{
		Status:  "failed",
		Check:   "storage",
		Message: fmt.Sprintf("storage=%s class=missing_resource hint=grant_or_configure_proxmox_storage", name),
		Details: map[string]string{"storage": name, "class": "missing_resource", "hint": "grant_or_configure_proxmox_storage", "endpoint": endpoint},
	}
}

func proxmoxStorageSupportsImages(content string) bool {
	return proxmoxStorageSupportsContent(content, "images")
}

func proxmoxStorageSupportsContent(content, required string) bool {
	for _, item := range strings.Split(content, ",") {
		if strings.EqualFold(strings.TrimSpace(item), required) {
			return true
		}
	}
	return false
}

type proxmoxTemplateStorageRequirement struct {
	Name    string
	Content string
}

func proxmoxTemplateStorages(config map[string]any) []proxmoxTemplateStorageRequirement {
	seen := map[proxmoxTemplateStorageRequirement]bool{}
	var storages []proxmoxTemplateStorageRequirement
	for key, value := range config {
		lowerKey := strings.ToLower(key)
		diskKey := strings.HasPrefix(lowerKey, "ide") ||
			strings.HasPrefix(lowerKey, "sata") ||
			strings.HasPrefix(lowerKey, "scsi") ||
			strings.HasPrefix(lowerKey, "virtio") ||
			strings.HasPrefix(lowerKey, "efidisk") ||
			strings.HasPrefix(lowerKey, "tpmstate")
		if !diskKey {
			continue
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		storage, _, found := strings.Cut(text, ":")
		storage = strings.TrimSpace(storage)
		if !found || storage == "" || storage == "none" {
			continue
		}
		requiredContent := "images"
		if !strings.Contains(strings.ToLower(text), "cloudinit") {
			for _, item := range strings.Split(text, ",") {
				name, value, found := strings.Cut(item, "=")
				if found && strings.EqualFold(strings.TrimSpace(name), "media") && strings.EqualFold(strings.TrimSpace(value), "cdrom") {
					requiredContent = "iso"
				}
			}
		}
		requirement := proxmoxTemplateStorageRequirement{Name: storage, Content: requiredContent}
		if seen[requirement] {
			continue
		}
		seen[requirement] = true
		storages = append(storages, requirement)
	}
	sort.Slice(storages, func(i, j int) bool {
		if storages[i].Name == storages[j].Name {
			return storages[i].Content < storages[j].Content
		}
		return storages[i].Name < storages[j].Name
	})
	return storages
}

func proxmoxTemplateStorageNames(requirements []proxmoxTemplateStorageRequirement) []string {
	var names []string
	last := ""
	for _, requirement := range requirements {
		if requirement.Name != last {
			names = append(names, requirement.Name)
			last = requirement.Name
		}
	}
	return names
}

func (c *ProxmoxClient) proxmoxNetworkCheck(ctx context.Context, cfg Config) ProxmoxReadinessCheck {
	path := "/nodes/" + url.PathEscape(c.Node) + "/network"
	inventoryPath := path + "?type=include_sdn"
	var networks []proxmoxNetwork
	if err := c.doRequired(ctx, http.MethodGet, inventoryPath, nil, &networks); err != nil {
		var proxErr *ProxmoxError
		if !errors.As(err, &proxErr) || proxErr.StatusCode != http.StatusBadRequest {
			return c.proxmoxFailedReadiness("bridge", inventoryPath, err, map[string]string{"bridge": cfg.Proxmox.Bridge})
		}
		// include_sdn was added in PVE 9. PVE 8 uses any_bridge for local
		// bridges and SDN vnets.
		inventoryPath = path + "?type=any_bridge"
		networks = nil
		if err := c.doRequired(ctx, http.MethodGet, inventoryPath, nil, &networks); err != nil {
			return c.proxmoxFailedReadiness("bridge", inventoryPath, err, map[string]string{"bridge": cfg.Proxmox.Bridge})
		}
	}
	bridges := []string{strings.TrimSpace(cfg.Proxmox.Bridge)}
	source := "config"
	if bridges[0] == "" {
		source = "template"
		if cfg.Proxmox.TemplateID <= 0 {
			return ProxmoxReadinessCheck{
				Status:  "failed",
				Check:   "bridge",
				Message: "bridge=missing class=config hint=set_proxmox_bridge_or_template_network",
				Details: map[string]string{"bridge": "missing", "class": "config", "hint": "set_proxmox_bridge_or_template_network"},
			}
		}
		configPath := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(c.Node), cfg.Proxmox.TemplateID)
		var config map[string]any
		if err := c.doRequired(ctx, http.MethodGet, configPath, nil, &config); err != nil {
			return c.proxmoxFailedReadiness("bridge", configPath, err, map[string]string{"bridge": "template"})
		}
		if bridge := proxmoxTemplateNet0Bridge(config); bridge != "" {
			bridges = []string{bridge}
		} else {
			bridges = nil
		}
	}
	if len(bridges) == 0 {
		return ProxmoxReadinessCheck{
			Status:  "failed",
			Check:   "bridge",
			Message: fmt.Sprintf("bridge=missing class=missing_resource hint=set_proxmox_bridge_or_template_network templateId=%d", cfg.Proxmox.TemplateID),
			Details: map[string]string{"bridge": "missing", "class": "missing_resource", "hint": "set_proxmox_bridge_or_template_network", "templateId": strconv.Itoa(cfg.Proxmox.TemplateID)},
		}
	}
	var firstFailure ProxmoxReadinessCheck
	for _, bridge := range bridges {
		check := c.proxmoxBridgeCheck(ctx, path, cfg.Proxmox.Node, bridge, networks)
		check.Details["source"] = source
		if check.Status == "ok" {
			return check
		}
		if firstFailure.Check == "" {
			firstFailure = check
		}
	}
	return firstFailure
}

func (c *ProxmoxClient) proxmoxBridgeCheck(ctx context.Context, path, node, bridge string, networks []proxmoxNetwork) ProxmoxReadinessCheck {
	for _, network := range networks {
		if network.Iface == bridge {
			return proxmoxBridgeReadiness(bridge, network, "/nodes/"+node+"/network")
		}
	}
	interfacePath := path + "/" + url.PathEscape(bridge)
	var network proxmoxNetwork
	if err := c.doRequired(ctx, http.MethodGet, interfacePath, nil, &network); err != nil {
		if !IsProxmoxNotFound(err) {
			return c.proxmoxFailedReadiness("bridge", interfacePath, err, map[string]string{"bridge": bridge})
		}
		return ProxmoxReadinessCheck{
			Status:  "failed",
			Check:   "bridge",
			Message: fmt.Sprintf("bridge=%s class=missing_resource hint=configure_proxmox_bridge", bridge),
			Details: map[string]string{"bridge": bridge, "class": "missing_resource", "hint": "configure_proxmox_bridge", "endpoint": "/nodes/" + node + "/network"},
		}
	}
	return proxmoxBridgeReadiness(bridge, network, "/nodes/"+node+"/network/"+bridge)
}

func proxmoxTemplateNet0Bridge(config map[string]any) string {
	text, ok := config["net0"].(string)
	if !ok {
		return ""
	}
	for _, item := range strings.Split(text, ",") {
		name, bridge, found := strings.Cut(item, "=")
		if found && strings.EqualFold(strings.TrimSpace(name), "bridge") {
			return strings.TrimSpace(bridge)
		}
	}
	return ""
}

func proxmoxBridgeReadiness(bridge string, network proxmoxNetwork, endpoint string) ProxmoxReadinessCheck {
	bridgeType := network.Type
	if bridgeType == "" {
		bridgeType = "bridge"
	}
	switch strings.ToLower(bridgeType) {
	case "bridge", "ovsbridge", "vnet":
	default:
		return ProxmoxReadinessCheck{
			Status:  "failed",
			Check:   "bridge",
			Message: fmt.Sprintf("bridge=%s class=missing_resource hint=configure_proxmox_bridge type=%s", bridge, network.Type),
			Details: map[string]string{"bridge": bridge, "class": "missing_resource", "hint": "configure_proxmox_bridge", "type": network.Type, "endpoint": endpoint},
		}
	}
	if network.Active == 0 {
		return ProxmoxReadinessCheck{
			Status:  "failed",
			Check:   "bridge",
			Message: fmt.Sprintf("bridge=%s class=missing_resource hint=activate_proxmox_bridge active=0", bridge),
			Details: map[string]string{"bridge": bridge, "class": "missing_resource", "hint": "activate_proxmox_bridge", "active": "0", "endpoint": endpoint},
		}
	}
	return ProxmoxReadinessCheck{
		Status:  "ok",
		Check:   "bridge",
		Message: fmt.Sprintf("bridge=%s type=%s active=1", bridge, bridgeType),
		Details: map[string]string{"bridge": bridge, "type": bridgeType, "active": "1", "endpoint": endpoint},
	}
}

func (c *ProxmoxClient) proxmoxTemplateCheck(ctx context.Context, cfg Config) ProxmoxReadinessCheck {
	if cfg.Proxmox.TemplateID <= 0 {
		return ProxmoxReadinessCheck{
			Status:  "failed",
			Check:   "template",
			Message: "templateId=missing class=config hint=set_proxmox_template_id",
			Details: map[string]string{"templateId": "missing", "class": "config", "hint": "set_proxmox_template_id"},
		}
	}
	vms, err := c.listQEMU(ctx)
	if err != nil {
		return c.proxmoxFailedReadiness("template", "/nodes/"+url.PathEscape(c.Node)+"/qemu", err, map[string]string{"templateId": strconv.Itoa(cfg.Proxmox.TemplateID)})
	}
	for _, vm := range vms {
		if vm.VMID != cfg.Proxmox.TemplateID {
			continue
		}
		if vm.Template == 0 {
			return ProxmoxReadinessCheck{
				Status:  "failed",
				Check:   "template",
				Message: fmt.Sprintf("templateId=%d class=missing_resource hint=convert_vm_to_template", cfg.Proxmox.TemplateID),
				Details: map[string]string{"templateId": strconv.Itoa(cfg.Proxmox.TemplateID), "class": "missing_resource", "hint": "convert_vm_to_template", "endpoint": "/nodes/" + cfg.Proxmox.Node + "/qemu"},
			}
		}
		configPath := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(c.Node), cfg.Proxmox.TemplateID)
		var config map[string]any
		if err := c.doRequired(ctx, http.MethodGet, configPath, nil, &config); err != nil {
			return c.proxmoxFailedReadiness("template", configPath, err, map[string]string{"templateId": strconv.Itoa(cfg.Proxmox.TemplateID)})
		}
		if !proxmoxTemplateHasCloudInit(config) {
			return ProxmoxReadinessCheck{
				Status:  "failed",
				Check:   "template",
				Message: fmt.Sprintf("templateId=%d class=missing_resource hint=attach_proxmox_cloudinit_drive", cfg.Proxmox.TemplateID),
				Details: map[string]string{"templateId": strconv.Itoa(cfg.Proxmox.TemplateID), "class": "missing_resource", "hint": "attach_proxmox_cloudinit_drive", "endpoint": fmt.Sprintf("/nodes/%s/qemu/%d/config", cfg.Proxmox.Node, cfg.Proxmox.TemplateID)},
			}
		}
		return ProxmoxReadinessCheck{
			Status:  "ok",
			Check:   "template",
			Message: fmt.Sprintf("templateId=%d template=ready", cfg.Proxmox.TemplateID),
			Details: map[string]string{"templateId": strconv.Itoa(cfg.Proxmox.TemplateID), "template": "ready", "endpoint": fmt.Sprintf("/nodes/%s/qemu/%d/config", cfg.Proxmox.Node, cfg.Proxmox.TemplateID)},
		}
	}
	return ProxmoxReadinessCheck{
		Status:  "failed",
		Check:   "template",
		Message: fmt.Sprintf("templateId=%d class=missing_resource hint=configure_existing_proxmox_template", cfg.Proxmox.TemplateID),
		Details: map[string]string{"templateId": strconv.Itoa(cfg.Proxmox.TemplateID), "class": "missing_resource", "hint": "configure_existing_proxmox_template", "endpoint": "/nodes/" + cfg.Proxmox.Node + "/qemu"},
	}
}

func proxmoxTemplateHasCloudInit(config map[string]any) bool {
	for key, value := range config {
		lowerKey := strings.ToLower(key)
		diskKey := strings.HasPrefix(lowerKey, "ide") ||
			strings.HasPrefix(lowerKey, "sata") ||
			strings.HasPrefix(lowerKey, "scsi") ||
			strings.HasPrefix(lowerKey, "virtio")
		if !diskKey {
			continue
		}
		text, ok := value.(string)
		if ok && strings.Contains(strings.ToLower(text), "cloudinit") {
			return true
		}
	}
	return false
}

func (c *ProxmoxClient) proxmoxNextIDCheck(ctx context.Context) ProxmoxReadinessCheck {
	id, err := c.nextID(ctx)
	if err != nil {
		return c.proxmoxFailedReadiness("nextid", "/cluster/nextid", err, nil)
	}
	return ProxmoxReadinessCheck{
		Status:  "ok",
		Check:   "nextid",
		Message: fmt.Sprintf("nextid=%d endpoint=/cluster/nextid", id),
		Details: map[string]string{"nextid": strconv.Itoa(id), "endpoint": "/cluster/nextid"},
	}
}

func (c *ProxmoxClient) proxmoxPoolCheck(ctx context.Context, cfg Config) ProxmoxReadinessCheck {
	path := "/pools/" + url.PathEscape(cfg.Proxmox.Pool)
	var pool map[string]any
	if err := c.doRequired(ctx, http.MethodGet, path, nil, &pool); err != nil {
		return c.proxmoxFailedReadiness("pool", path, err, map[string]string{"pool": cfg.Proxmox.Pool})
	}
	return ProxmoxReadinessCheck{
		Status:  "ok",
		Check:   "pool",
		Message: fmt.Sprintf("pool=%s endpoint=%s", cfg.Proxmox.Pool, path),
		Details: map[string]string{"pool": cfg.Proxmox.Pool, "endpoint": path},
	}
}

func (c *ProxmoxClient) proxmoxInventoryCheck(ctx context.Context) ProxmoxReadinessCheck {
	permissionPath := "/vms"
	if err := c.requirePropagatedVMAudit(ctx, permissionPath); err != nil {
		return c.proxmoxFailedReadiness("inventory", "/access/permissions?path="+permissionPath, err, map[string]string{"scope": "cluster", "permissionPath": permissionPath})
	}
	vms, err := c.listClusterVMs(ctx)
	if err != nil {
		return c.proxmoxFailedReadiness("inventory", "/cluster/resources?type=vm", err, map[string]string{"scope": "cluster"})
	}
	leases := 0
	qemuVMs := 0
	for _, vm := range vms {
		if vm.Type != "qemu" {
			continue
		}
		qemuVMs++
		if vm.Template == 0 && strings.HasPrefix(vm.Name, "crabbox-") {
			leases++
		}
	}
	return ProxmoxReadinessCheck{
		Status:  "ok",
		Check:   "inventory",
		Message: fmt.Sprintf("api=list mutation=false leases=%d vms=%d", leases, qemuVMs),
		Details: map[string]string{"api": "list", "mutation": "false", "leases": strconv.Itoa(leases), "vms": strconv.Itoa(qemuVMs), "scope": "cluster", "permissionPath": permissionPath, "endpoint": "/cluster/resources?type=vm"},
	}
}

func (c *ProxmoxClient) proxmoxFailedReadiness(check, path string, err error, extra map[string]string) ProxmoxReadinessCheck {
	class := proxmoxReadinessErrorClass(err)
	details := map[string]string{
		"class":    class,
		"endpoint": proxmoxDisplayPath(path),
		"hint":     proxmoxReadinessHint(check, class),
		"error":    proxmoxSafeError(err),
	}
	for key, value := range extra {
		if value != "" {
			details[key] = value
		}
	}
	return ProxmoxReadinessCheck{
		Status:  "failed",
		Check:   check,
		Message: fmt.Sprintf("class=%s endpoint=%s hint=%s error=%s", class, proxmoxDisplayPath(path), details["hint"], proxmoxSafeError(err)),
		Details: details,
	}
}

func proxmoxReadinessErrorClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var proxErr *ProxmoxError
	if errors.As(err, &proxErr) {
		switch proxErr.StatusCode {
		case http.StatusUnauthorized:
			return "auth"
		case http.StatusForbidden:
			return "permission"
		case http.StatusNotFound:
			return "missing_resource"
		}
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "timeout") || strings.Contains(message, "timed out") || strings.Contains(message, "deadline"):
		return "timeout"
	case strings.Contains(message, "permission") || strings.Contains(message, "denied") || strings.Contains(message, "sys.audit") || strings.Contains(message, "403"):
		return "permission"
	case strings.Contains(message, "unauthorized") || strings.Contains(message, "401") || strings.Contains(message, "token"):
		return "auth"
	case strings.Contains(message, "no such host") || strings.Contains(message, "connection refused") || strings.Contains(message, "tls") || strings.Contains(message, "network") || strings.Contains(message, "dial"):
		return "network"
	case strings.Contains(message, "missing") || strings.Contains(message, "required"):
		return "config"
	default:
		return "provider"
	}
}

func proxmoxReadinessHint(check, class string) string {
	if class == "permission" {
		switch check {
		case "node":
			return "grant_proxmox_node_audit"
		case "storage":
			return "grant_proxmox_storage_audit"
		case "bridge":
			return "grant_proxmox_network_audit"
		case "template":
			return "grant_proxmox_vm_audit"
		case "nextid":
			return "grant_proxmox_sys_audit"
		case "pool":
			return "grant_proxmox_pool_audit"
		case "inventory":
			return "grant_proxmox_cluster_vm_audit"
		default:
			return "grant_proxmox_read_permissions"
		}
	}
	if class == "auth" {
		return "check_proxmox_token_id_secret"
	}
	if class == "network" {
		return "check_proxmox_api_url_tls_network"
	}
	if class == "missing_resource" {
		return "check_proxmox_node_storage_bridge_template"
	}
	if class == "config" {
		return "check_proxmox_provider_config"
	}
	return "check_proxmox_api_response"
}

func proxmoxSafeError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	text = proxmoxAPITokenPattern.ReplaceAllString(text, "PVEAPIToken=<redacted>")
	return strings.Join(strings.Fields(text), "_")
}

func proxmoxDisplayPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	u, err := url.Parse(path)
	if err == nil && u.Path != "" {
		path = u.Path
	}
	if strings.Contains(path, "?") {
		path, _, _ = strings.Cut(path, "?")
	}
	return path
}

func (c *ProxmoxClient) nextID(ctx context.Context) (int, error) {
	var raw any
	if err := c.doRequired(ctx, http.MethodGet, "/cluster/nextid", nil, &raw); err != nil {
		return 0, err
	}
	switch v := raw.(type) {
	case float64:
		return int(v), nil
	case string:
		id, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("parse proxmox nextid %q: %w", v, err)
		}
		return id, nil
	default:
		return 0, fmt.Errorf("unexpected proxmox nextid response: %#v", raw)
	}
}

type proxmoxVM struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Template int    `json:"template"`
}

type proxmoxClusterVM struct {
	VMID     proxmoxInt `json:"vmid"`
	Name     string     `json:"name"`
	Node     string     `json:"node"`
	Type     string     `json:"type"`
	Template proxmoxInt `json:"template"`
}

func (c *ProxmoxClient) requireVMAudit(ctx context.Context, path string) error {
	var permissions map[string]map[string]proxmoxInt
	if err := c.doRequired(ctx, http.MethodGet, "/access/permissions?path="+url.QueryEscape(path), nil, &permissions); err != nil {
		return err
	}
	if _, ok := permissions[path]["VM.Audit"]; !ok {
		return fmt.Errorf("permission denied: authoritative Proxmox inventory requires VM.Audit on %s", path)
	}
	return nil
}

func (c *ProxmoxClient) requirePropagatedVMAudit(ctx context.Context, path string) error {
	var permissions map[string]map[string]proxmoxInt
	if err := c.doRequired(ctx, http.MethodGet, "/access/permissions?path="+url.QueryEscape(path), nil, &permissions); err != nil {
		return err
	}
	if propagated, ok := permissions[path]["VM.Audit"]; !ok || propagated == 0 {
		return fmt.Errorf("permission denied: authoritative Proxmox inventory requires propagated VM.Audit on %s", path)
	}
	return nil
}

func (c *ProxmoxClient) listQEMU(ctx context.Context) ([]proxmoxVM, error) {
	var vms []proxmoxVM
	if err := c.doRequired(ctx, http.MethodGet, "/nodes/"+url.PathEscape(c.Node)+"/qemu", nil, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

func (c *ProxmoxClient) ListCrabboxServers(ctx context.Context) ([]Server, error) {
	vms, err := c.listQEMU(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(vms))
	for _, vm := range vms {
		if vm.Template != 0 || !strings.HasPrefix(vm.Name, "crabbox-") {
			continue
		}
		server, err := c.GetServer(ctx, strconv.Itoa(vm.VMID))
		if err != nil {
			if IsProxmoxNotFound(err) {
				continue
			}
			return nil, err
		}
		if isCrabboxProxmoxLease(server) {
			servers = append(servers, server)
		}
	}
	return servers, nil
}

func (c *ProxmoxClient) ListCrabboxServersCluster(ctx context.Context) ([]Server, error) {
	if err := c.requirePropagatedVMAudit(ctx, "/vms"); err != nil {
		return nil, err
	}
	vms, err := c.listClusterVMs(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(vms))
	for _, vm := range vms {
		if vm.Type != "qemu" || vm.Template != 0 || !strings.HasPrefix(vm.Name, "crabbox-") || vm.Node == "" {
			continue
		}
		server, exists, err := c.getClusterServer(ctx, vm)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		if isCrabboxProxmoxLease(server) {
			servers = append(servers, server)
		}
	}
	return servers, nil
}

func (c *ProxmoxClient) listClusterVMs(ctx context.Context) ([]proxmoxClusterVM, error) {
	var vms []proxmoxClusterVM
	if err := c.doRequired(ctx, http.MethodGet, "/cluster/resources?type=vm", nil, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

func (c *ProxmoxClient) getClusterServer(ctx context.Context, vm proxmoxClusterVM) (Server, bool, error) {
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		server, err := c.GetServerOnNode(ctx, vm.Node, strconv.Itoa(int(vm.VMID)))
		if err == nil {
			return server, true, nil
		}
		if !IsProxmoxNotFound(err) {
			return Server{}, false, err
		}
		refreshed, err := c.listClusterVMs(ctx)
		if err != nil {
			return Server{}, false, err
		}
		found := false
		for _, candidate := range refreshed {
			if candidate.Type == "qemu" && candidate.Template == 0 && candidate.VMID == vm.VMID {
				vm = candidate
				found = true
				break
			}
		}
		if !found {
			return Server{}, false, nil
		}
	}
	return Server{}, false, fmt.Errorf("Proxmox VM %d did not stabilize during cluster inventory reconciliation", vm.VMID)
}

func (c *ProxmoxClient) GetServerOnNode(ctx context.Context, node, id string) (Server, error) {
	scoped := *c
	scoped.Node = node
	return scoped.getServer(ctx, id, true)
}

func (c *ProxmoxClient) VMExistsInCluster(ctx context.Context, id string) (bool, error) {
	vmid, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil || vmid <= 0 {
		return false, fmt.Errorf("invalid Proxmox VM identity %q", id)
	}
	if err := c.requireVMAudit(ctx, "/vms/"+strconv.Itoa(vmid)); err != nil {
		return false, err
	}
	vms, err := c.listClusterVMs(ctx)
	if err != nil {
		return false, err
	}
	for _, vm := range vms {
		if int(vm.VMID) == vmid {
			return true, nil
		}
	}
	return false, nil
}

func (c *ProxmoxClient) CreateServer(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool) (Server, error) {
	if cfg.TargetOS != targetLinux {
		return Server{}, exit(2, "proxmox provider currently supports target=linux only")
	}
	if cfg.Proxmox.TemplateID <= 0 {
		return Server{}, exit(3, "proxmox templateId is required (set proxmox.templateId or CRABBOX_PROXMOX_TEMPLATE_ID)")
	}
	vmid, err := c.nextID(ctx)
	if err != nil {
		return Server{}, err
	}
	name := leaseProviderName(leaseID, slug)
	full := "1"
	if !cfg.Proxmox.FullClone {
		full = "0"
	}
	clone := url.Values{
		"newid": {strconv.Itoa(vmid)},
		"name":  {name},
		"full":  {full},
	}
	if cfg.Proxmox.Storage != "" {
		clone.Set("storage", cfg.Proxmox.Storage)
	}
	if cfg.Proxmox.Pool != "" {
		clone.Set("pool", cfg.Proxmox.Pool)
	}
	var upid string
	if err := c.doRequired(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/clone", url.PathEscape(c.Node), cfg.Proxmox.TemplateID), clone, &upid); err != nil {
		return Server{}, err
	}
	clonedVMID := strconv.Itoa(vmid)
	cleanupClone := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = c.DeleteServer(cleanupCtx, clonedVMID)
	}
	if err := c.waitTask(ctx, upid); err != nil {
		cleanupClone()
		return Server{}, err
	}

	now := time.Now().UTC()
	labels := directLeaseLabels(cfg, leaseID, slug, "proxmox", "", keep, now)
	labels["node"] = cfg.Proxmox.Node
	labels["template_id"] = strconv.Itoa(cfg.Proxmox.TemplateID)
	description := proxmoxDescription(labels)
	config := url.Values{
		"ciuser":      {cfg.SSHUser},
		"sshkeys":     {proxmoxSSHKeysValue(publicKey)},
		"ipconfig0":   {"ip=dhcp"},
		"agent":       {"enabled=1"},
		"description": {description},
		"tags":        {"crabbox"},
	}
	if cfg.Proxmox.Bridge != "" {
		config.Set("net0", "virtio,bridge="+cfg.Proxmox.Bridge)
	}
	if err := c.configureVM(ctx, vmid, config); err != nil {
		cleanupClone()
		return Server{}, err
	}
	if err := c.startVM(ctx, vmid); err != nil {
		cleanupClone()
		return Server{}, err
	}
	server, err := c.waitServerIP(ctx, vmid)
	if err != nil {
		cleanupClone()
		return Server{}, err
	}
	if err := c.bootstrapSSH(ctx, server.PublicNet.IPv4.IP, cfg); err != nil {
		cleanupClone()
		return Server{}, err
	}
	server, err = c.GetServer(ctx, clonedVMID)
	if err != nil {
		cleanupClone()
		return Server{}, err
	}
	return server, nil
}

func (c *ProxmoxClient) waitServerIP(ctx context.Context, vmid int) (Server, error) {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		server, err := c.GetServer(ctx, strconv.Itoa(vmid))
		if err == nil && server.PublicNet.IPv4.IP != "" {
			return server, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return Server{}, fmt.Errorf("timeout waiting for proxmox guest ip: %w", err)
			}
			return Server{}, fmt.Errorf("timeout waiting for proxmox guest ip")
		}
		select {
		case <-ctx.Done():
			return Server{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *ProxmoxClient) bootstrapSSH(ctx context.Context, host string, cfg Config) error {
	target := SSHTargetFromConfig(cfg, host)
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if proxmoxRunSSHQuietWithOptions(ctx, target, sshTransportProbeCommand(target), "5", "1") == nil {
			return proxmoxRunSSHInputQuiet(ctx, target, "sudo /bin/bash -s", proxmoxBootstrapScript(cfg))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for proxmox ssh bootstrap transport")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func proxmoxSSHKeysValue(publicKey string) string {
	// Proxmox marks sshkeys as a "urlencoded" field, so it must remain
	// URL-encoded after the regular application/x-www-form-urlencoded decode.
	return strings.ReplaceAll(url.QueryEscape(strings.TrimSpace(publicKey)), "+", "%20")
}

func (c *ProxmoxClient) startVM(ctx context.Context, vmid int) error {
	var upid string
	if err := c.doRequired(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", url.PathEscape(c.Node), vmid), url.Values{}, &upid); err != nil {
		return err
	}
	return c.waitTask(ctx, upid)
}

func (c *ProxmoxClient) stopVM(ctx context.Context, vmid int) error {
	var upid string
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", url.PathEscape(c.Node), vmid), url.Values{}, &upid); err != nil {
		if IsProxmoxNotFound(err) {
			return nil
		}
		return err
	}
	return c.waitTask(ctx, upid)
}

func (c *ProxmoxClient) DeleteServer(ctx context.Context, id string) error {
	vmid, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil {
		server, err := c.GetServer(ctx, id)
		if err != nil {
			if IsProxmoxNotFound(err) {
				return nil
			}
			return err
		}
		vmid = int(server.ID)
	}
	_ = c.stopVM(ctx, vmid)
	q := url.Values{}
	q.Set("purge", "1")
	var upid string
	if err := c.doRequired(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/qemu/%d?%s", url.PathEscape(c.Node), vmid, q.Encode()), nil, &upid); err != nil {
		if IsProxmoxNotFound(err) {
			return nil
		}
		var proxErr *ProxmoxError
		if errors.As(err, &proxErr) {
			return err
		}
		return &ProxmoxDeleteRequestError{Err: err}
	}
	if err := c.waitTask(ctx, upid); err != nil {
		var waitErr *proxmoxTaskWaitError
		if errors.As(err, &waitErr) {
			return &ProxmoxDeleteTaskError{Err: err}
		}
		return err
	}
	return nil
}

func (c *ProxmoxClient) DeleteServerOnNode(ctx context.Context, node, id string) error {
	scoped := *c
	scoped.Node = node
	return scoped.DeleteServer(ctx, id)
}

func (c *ProxmoxClient) GetServer(ctx context.Context, id string) (Server, error) {
	return c.getServer(ctx, id, false)
}

func (c *ProxmoxClient) getServer(ctx context.Context, id string, requireConfig bool) (Server, error) {
	vmid, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil {
		return c.getServerByName(ctx, id)
	}
	var status proxmoxVM
	if err := c.doRequired(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", url.PathEscape(c.Node), vmid), nil, &status); err != nil {
		return Server{}, err
	}
	labels := map[string]string{}
	var config map[string]any
	configPath := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(c.Node), vmid)
	var configErr error
	if requireConfig {
		configErr = c.doRequired(ctx, http.MethodGet, configPath, nil, &config)
	} else {
		configErr = c.do(ctx, http.MethodGet, configPath, nil, &config)
	}
	if configErr == nil {
		if desc, ok := config["description"].(string); ok {
			labels = proxmoxDescriptionLabels(desc)
		}
		if status.Name == "" {
			if name, ok := config["name"].(string); ok {
				status.Name = name
			}
		}
	} else if requireConfig {
		return Server{}, configErr
	}
	ip, _ := c.guestIPv4(ctx, vmid)
	server := proxmoxVMToServer(c.Node, status, labels, ip)
	if server.Name == "" {
		server.Name = "vm-" + strconv.Itoa(vmid)
	}
	return server, nil
}

func (c *ProxmoxClient) getServerByName(ctx context.Context, name string) (Server, error) {
	vms, err := c.listQEMU(ctx)
	if err != nil {
		return Server{}, err
	}
	for _, vm := range vms {
		if vm.Name == name {
			return c.GetServer(ctx, strconv.Itoa(vm.VMID))
		}
	}
	return Server{}, &ProxmoxError{Method: http.MethodGet, Path: "/nodes/" + c.Node + "/qemu/" + name, StatusCode: http.StatusNotFound, Body: "not found"}
}

func (c *ProxmoxClient) SetLabels(ctx context.Context, id string, labels map[string]string) error {
	vmid, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil {
		server, err := c.GetServer(ctx, id)
		if err != nil {
			return err
		}
		vmid = int(server.ID)
	}
	return c.configureVM(ctx, vmid, url.Values{"description": {proxmoxDescription(labels)}})
}

func (c *ProxmoxClient) SetLabelsOnNode(ctx context.Context, node, id string, labels map[string]string) error {
	scoped := *c
	scoped.Node = node
	return scoped.SetLabels(ctx, id, labels)
}

func (c *ProxmoxClient) configureVM(ctx context.Context, vmid int, form url.Values) error {
	var upid string
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(c.Node), vmid), form, &upid); err != nil {
		return err
	}
	if upid == "" {
		return nil
	}
	return c.waitTask(ctx, upid)
}

type proxmoxTaskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

func (c *ProxmoxClient) waitTask(ctx context.Context, upid string) error {
	if upid == "" {
		return errors.New("proxmox task UPID is empty")
	}
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(c.Node), url.PathEscape(upid))
	deadline := time.Now().Add(15 * time.Minute)
	for {
		var status proxmoxTaskStatus
		if err := c.doRequired(ctx, http.MethodGet, path, nil, &status); err != nil {
			return &proxmoxTaskWaitError{err: err}
		}
		if status.Status == "stopped" {
			if status.ExitStatus == "OK" {
				return nil
			}
			if status.ExitStatus == "" {
				return fmt.Errorf("proxmox task %s failed: missing exitstatus", upid)
			}
			return fmt.Errorf("proxmox task %s failed: %s", upid, status.ExitStatus)
		}
		if time.Now().After(deadline) {
			return &proxmoxTaskWaitError{err: fmt.Errorf("timeout waiting for proxmox task %s", upid)}
		}
		select {
		case <-ctx.Done():
			return &proxmoxTaskWaitError{err: ctx.Err()}
		case <-time.After(2 * time.Second):
		}
	}
}

type proxmoxAgentExecStart struct {
	PID int `json:"pid"`
}

type proxmoxAgentExecStatus struct {
	Exited   proxmoxBool `json:"exited"`
	ExitCode int         `json:"exitcode"`
	OutData  string      `json:"out-data"`
	ErrData  string      `json:"err-data"`
}

type proxmoxBool bool

type proxmoxAgentInterface struct {
	Name        string `json:"name"`
	IPAddresses []struct {
		Type    string `json:"ip-address-type"`
		Address string `json:"ip-address"`
	} `json:"ip-addresses"`
}

func (c *ProxmoxClient) guestIPv4(ctx context.Context, vmid int) (string, error) {
	var res struct {
		Result []proxmoxAgentInterface `json:"result"`
	}
	if err := c.doRequired(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", url.PathEscape(c.Node), vmid), nil, &res); err != nil {
		return "", err
	}
	for _, iface := range res.Result {
		name := strings.ToLower(iface.Name)
		if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") {
			continue
		}
		for _, addr := range iface.IPAddresses {
			if addr.Type != "ipv4" || addr.Address == "" || strings.HasPrefix(addr.Address, "127.") {
				continue
			}
			if ip := net.ParseIP(addr.Address); ip != nil && ip.To4() != nil {
				return addr.Address, nil
			}
		}
	}
	return "", errors.New("no guest ipv4 address reported by qemu guest agent")
}

func proxmoxVMToServer(node string, vm proxmoxVM, labels map[string]string, ip string) Server {
	if labels == nil {
		labels = map[string]string{}
	}
	if labels["node"] == "" {
		labels["node"] = node
	}
	server := Server{
		Provider: "proxmox",
		CloudID:  strconv.Itoa(vm.VMID),
		HostID:   node,
		ID:       int64(vm.VMID),
		Name:     vm.Name,
		Status:   vm.Status,
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = ip
	server.PrivateNet.IPv4.IP = ip
	server.ServerType.Name = blank(labels["server_type"], blank(labels["template_id"], "template"))
	return server
}

func isCrabboxProxmoxLease(server Server) bool {
	if server.Labels == nil {
		return false
	}
	if server.Labels["crabbox"] != "true" {
		return false
	}
	if provider := server.Labels["provider"]; provider != "" && provider != "proxmox" {
		return false
	}
	return true
}

func IsCrabboxProxmoxLease(server Server) bool {
	return isCrabboxProxmoxLease(server)
}

func IsProxmoxNotFound(err error) bool {
	var proxErr *ProxmoxError
	return errors.As(err, &proxErr) && proxErr.StatusCode == http.StatusNotFound
}

func IsProxmoxDeleteTaskError(err error) bool {
	var taskErr *ProxmoxDeleteTaskError
	return errors.As(err, &taskErr)
}

func IsProxmoxDeleteRequestError(err error) bool {
	var requestErr *ProxmoxDeleteRequestError
	return errors.As(err, &requestErr)
}

func proxmoxDescription(labels map[string]string) string {
	var b strings.Builder
	b.WriteString("crabbox labels\n")
	for _, key := range sortedLabelKeys(labels) {
		fmt.Fprintf(&b, "%s=%s\n", key, labels[key])
	}
	return b.String()
}

func proxmoxDescriptionLabels(desc string) map[string]string {
	labels := map[string]string{}
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "crabbox labels" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		labels[key] = strings.TrimSpace(value)
	}
	return labels
}

func sortedLabelKeys(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func proxmoxBootstrapScript(cfg Config) string {
	return fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
mkdir -p %[1]s /var/cache/crabbox/pnpm /var/cache/crabbox/npm /var/lib/crabbox
cat >/etc/apt/apt.conf.d/80-crabbox-retries <<'APT'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APT
retry() {
  n=1
  until "$@"; do
    if [ "$n" -ge 8 ]; then
      return 1
    fi
    sleep $((n * 5))
    n=$((n + 1))
  done
}
retry apt-get update
retry apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq
chown -R %[2]s:%[2]s %[1]s /var/cache/crabbox || true
cat >/usr/local/bin/crabbox-ready <<'READY'
#!/usr/bin/env bash
set -euo pipefail
git --version >/dev/null
rsync --version >/dev/null
curl --version >/dev/null
jq --version >/dev/null
test -w %[1]s
READY
chmod 0755 /usr/local/bin/crabbox-ready
systemctl enable ssh || true
systemctl restart ssh || systemctl restart ssh.socket || true
touch /var/lib/crabbox/bootstrapped
/usr/local/bin/crabbox-ready
`, shellQuote(cfg.WorkRoot), shellQuote(cfg.SSHUser))
}
