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

type ProxmoxError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *ProxmoxError) Error() string {
	return fmt.Sprintf("proxmox %s %s: http %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

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
	if cfg.Proxmox.TemplateID <= 0 {
		return nil, exit(3, "proxmox templateId is required (set proxmox.templateId or CRABBOX_PROXMOX_TEMPLATE_ID)")
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
		return &ProxmoxError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: summarizeJSON(data)}
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
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

func (c *ProxmoxClient) nextID(ctx context.Context) (int, error) {
	var raw any
	if err := c.do(ctx, http.MethodGet, "/cluster/nextid", nil, &raw); err != nil {
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

func (c *ProxmoxClient) listQEMU(ctx context.Context) ([]proxmoxVM, error) {
	var vms []proxmoxVM
	if err := c.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(c.Node)+"/qemu", nil, &vms); err != nil {
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

func (c *ProxmoxClient) CreateServer(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool) (Server, error) {
	if cfg.TargetOS != targetLinux {
		return Server{}, exit(2, "proxmox provider currently supports target=linux only")
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
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/clone", url.PathEscape(c.Node), cfg.Proxmox.TemplateID), clone, &upid); err != nil {
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
		"sshkeys":     {publicKey},
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
	if err := c.bootstrapGuest(ctx, vmid, cfg); err != nil {
		cleanupClone()
		return Server{}, err
	}
	server, err := c.GetServer(ctx, clonedVMID)
	if err != nil {
		cleanupClone()
		return Server{}, err
	}
	return server, nil
}

func (c *ProxmoxClient) startVM(ctx context.Context, vmid int) error {
	var upid string
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", url.PathEscape(c.Node), vmid), url.Values{}, &upid); err != nil {
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
	if err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/qemu/%d?%s", url.PathEscape(c.Node), vmid, q.Encode()), nil, &upid); err != nil {
		if IsProxmoxNotFound(err) {
			return nil
		}
		return err
	}
	return c.waitTask(ctx, upid)
}

func (c *ProxmoxClient) GetServer(ctx context.Context, id string) (Server, error) {
	vmid, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil {
		return c.getServerByName(ctx, id)
	}
	var status proxmoxVM
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", url.PathEscape(c.Node), vmid), nil, &status); err != nil {
		return Server{}, err
	}
	labels := map[string]string{}
	var config map[string]any
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(c.Node), vmid), nil, &config); err == nil {
		if desc, ok := config["description"].(string); ok {
			labels = proxmoxDescriptionLabels(desc)
		}
		if status.Name == "" {
			if name, ok := config["name"].(string); ok {
				status.Name = name
			}
		}
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

func (c *ProxmoxClient) configureVM(ctx context.Context, vmid int, form url.Values) error {
	var upid string
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(c.Node), vmid), form, &upid); err != nil {
		return err
	}
	return c.waitTask(ctx, upid)
}

type proxmoxTaskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

func (c *ProxmoxClient) waitTask(ctx context.Context, upid string) error {
	if upid == "" {
		return nil
	}
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(c.Node), url.PathEscape(upid))
	deadline := time.Now().Add(15 * time.Minute)
	for {
		var status proxmoxTaskStatus
		if err := c.do(ctx, http.MethodGet, path, nil, &status); err != nil {
			return err
		}
		if status.Status == "stopped" {
			if status.ExitStatus == "" || status.ExitStatus == "OK" {
				return nil
			}
			return fmt.Errorf("proxmox task %s failed: %s", upid, status.ExitStatus)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for proxmox task %s", upid)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

type proxmoxAgentExecStart struct {
	PID int `json:"pid"`
}

type proxmoxAgentExecStatus struct {
	Exited   bool   `json:"exited"`
	ExitCode int    `json:"exitcode"`
	OutData  string `json:"out-data"`
	ErrData  string `json:"err-data"`
}

func (c *ProxmoxClient) bootstrapGuest(ctx context.Context, vmid int, cfg Config) error {
	if err := c.waitGuestAgent(ctx, vmid); err != nil {
		return err
	}
	script := proxmoxBootstrapScript(cfg)
	var start proxmoxAgentExecStart
	form := url.Values{
		"command":    {"/bin/bash", "-s"},
		"input-data": {script},
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec", url.PathEscape(c.Node), vmid), form, &start); err != nil {
		return fmt.Errorf("proxmox guest bootstrap: %w", err)
	}
	return c.waitGuestExec(ctx, vmid, start.PID)
}

func (c *ProxmoxClient) waitGuestAgent(ctx context.Context, vmid int) error {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if _, err := c.guestIPv4(ctx, vmid); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for proxmox qemu guest agent on vmid=%d", vmid)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *ProxmoxClient) waitGuestExec(ctx context.Context, vmid, pid int) error {
	if pid == 0 {
		return fmt.Errorf("proxmox guest exec returned empty pid")
	}
	path := fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec-status?pid=%d", url.PathEscape(c.Node), vmid, pid)
	deadline := time.Now().Add(15 * time.Minute)
	for {
		var status proxmoxAgentExecStatus
		if err := c.do(ctx, http.MethodGet, path, nil, &status); err != nil {
			return err
		}
		if status.Exited {
			if status.ExitCode == 0 {
				return nil
			}
			return fmt.Errorf("proxmox guest bootstrap exit=%d stderr=%s stdout=%s", status.ExitCode, strings.TrimSpace(status.ErrData), strings.TrimSpace(status.OutData))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for proxmox guest bootstrap pid=%d", pid)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

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
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", url.PathEscape(c.Node), vmid), nil, &res); err != nil {
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
