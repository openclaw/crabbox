package proxmox

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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ValidateProxmoxTailnetConfig admits only the native, confidential, fixed-lease
// path. Direct authKey/seed bootstrap and implicit underlay fallback are not used.
func ValidateProxmoxTailnetConfig(cfg core.Config) error {
	if !cfg.Tailscale.Enabled {
		return nil
	}
	endpoint, err := url.Parse(cfg.Proxmox.APIURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || cfg.Proxmox.InsecureTLS {
		return core.Exit(2, "Proxmox Tailnet provisioning requires strictly verified HTTPS without URL credentials, query or fragment")
	}
	if cfg.Network != core.NetworkTailscale || cfg.TargetOS != core.TargetLinux {
		return core.Exit(2, "Proxmox Tailnet provisioning requires --network tailscale, target=linux and SSH port 22")
	}
	if strings.TrimSpace(cfg.Tailscale.AuthKey) != "" || cfg.Tailscale.ExitNode != "" || cfg.Tailscale.ExitNodeAllowLANAccess {
		return core.Exit(2, "Proxmox Tailnet provisioning requires the per-lease broker, not a direct auth key or exit node")
	}
	if !filepath.IsAbs(cfg.Proxmox.TailscaleBroker) {
		return core.Exit(3, "proxmox.tailscaleBroker must be a trusted absolute executable path (user config or CRABBOX_PROXMOX_TAILSCALE_BROKER)")
	}
	if err := core.ValidateNetworkConfig(cfg); err != nil {
		return err
	}
	return validateProxmoxTailnetTags(cfg.Tailscale.Tags)
}

// Keep admission aligned with the prepared helper before clone or key issuance.
func validateProxmoxTailnetTags(tags []string) error {
	if len(tags) == 0 || len(tags) > 32 {
		return core.Exit(2, "Proxmox Tailnet enrollment requires 1–32 unique tags")
	}
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		name := strings.TrimPrefix(tag, "tag:")
		if !strings.HasPrefix(tag, "tag:") || !validProxmoxTailnetName(name, 63) || seen[tag] {
			return core.Exit(2, "Proxmox Tailnet enrollment requires unique tags with alphanumeric names and hyphens")
		}
		seen[tag] = true
	}
	return nil
}

func validProxmoxTailnetName(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	for i, ch := range []byte(value) {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || i > 0 && ch == '-' {
			continue
		}
		return false
	}
	return true
}

// ValidateProxmoxTailnetIdentity validates the exact final rendering, not a
// template estimate. Call after slug allocation and before reserving a VMID.
func ValidateProxmoxTailnetIdentity(cfg core.Config, leaseID, slug string) error {
	if !cfg.Tailscale.Enabled {
		return nil
	}
	if err := validateProxmoxTailnetTags(cfg.Tailscale.Tags); err != nil {
		return err
	}
	if !validProxmoxTailnetName(core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, "proxmox"), 63) {
		return core.Exit(2, "Proxmox Tailnet rendered hostname must be 1–63 lowercase alphanumeric or hyphen characters, starting with alphanumeric")
	}
	return nil
}

// Native descriptions are not cloud label sets: keep structured tag syntax
// intact instead of the lossy generic cloud-label sanitizer.
func ProxmoxTailnetIdentityLabels(cfg core.Config, leaseID, slug string) map[string]string {
	if !cfg.Tailscale.Enabled {
		return nil
	}
	return map[string]string{"tailscale": "true", "tailscale_state": "requested", "tailscale_tags": strings.Join(cfg.Tailscale.Tags, ","), "tailscale_hostname": core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, leaseID, slug, "proxmox")}
}

func proxmoxTailnetIPv4(value string) bool {
	ip := net.ParseIP(value).To4()
	return ip != nil && ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127
}

// ValidateProxmoxTailnetAccess also applies when the current invocation omits
// --tailscale: persisted lease policy cannot be downgraded by run/inspect/replay.
func ValidateProxmoxTailnetAccess(cfg core.Config, server core.Server) error {
	required := server.Labels["tailscale"] == "true"
	if cfg.Network == core.NetworkTailscale && !required {
		return core.Exit(5, "network=tailscale requested but Proxmox lease has no managed Tailnet identity")
	}
	if required && (cfg.Network == core.NetworkPublic || !proxmoxTailnetIPv4(server.PublicNet.IPv4.IP) || server.Labels["tailscale_ipv4"] != server.PublicNet.IPv4.IP) {
		return core.Exit(5, "Proxmox Tailnet lease requires a current tailscale0 CGNAT IPv4; no underlay fallback")
	}
	return nil
}

type proxmoxTailnetBrokerRequest struct {
	Version    int      `json:"version"`
	Operation  string   `json:"operation"`
	Endpoint   string   `json:"endpoint"`
	Node       string   `json:"node"`
	LeaseID    string   `json:"leaseId"`
	VMID       string   `json:"vmid"`
	Generation string   `json:"generation"`
	Hostname   string   `json:"hostname"`
	Tags       []string `json:"tags"`
}

type proxmoxTailnetCredential struct {
	AuthKey   string    `json:"authKey"`
	ExpiresAt time.Time `json:"expiresAt"`
	Ephemeral bool      `json:"ephemeral"`
	Reusable  *bool     `json:"reusable"`
}

// PVE normalizes QGA booleans to 0/1; accept the underlying QGA JSON form too.
type proxmoxAgentBool bool

func (value *proxmoxAgentBool) UnmarshalJSON(data []byte) error {
	switch string(bytes.TrimSpace(data)) {
	case "true", "1":
		*value = true
	case "false", "0":
		*value = false
	default:
		return errors.New("invalid Proxmox guest-agent boolean")
	}
	return nil
}

// broker credentials are inherited through the service's protected environment,
// never argv or the lease record. Its stdout is private protocol, not diagnostics.
var proxmoxRunTailnetBroker = runProxmoxTailnetBroker

func runProxmoxTailnetBroker(ctx context.Context, program string, request proxmoxTailnetBrokerRequest) (proxmoxTailnetCredential, error) {
	var result proxmoxTailnetCredential
	if !filepath.IsAbs(program) {
		return result, errors.New("Proxmox Tailnet broker is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	input, err := json.Marshal(request)
	if err != nil {
		return result, errors.New("encode Proxmox Tailnet broker request")
	}
	cmd := exec.CommandContext(ctx, program)
	cmd.Stdin = bytes.NewReader(input)
	out := newBrokerOutput(16 << 10)
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, errors.New("Proxmox Tailnet broker failed (output suppressed)")
	}
	data, truncated := out.boundedString()
	if truncated {
		return result, errors.New("Proxmox Tailnet broker response exceeds limit")
	}
	if request.Operation == "release" {
		// An explicit acknowledgment is required; an empty/crashed wrapper is not cleanup.
		var ack struct {
			Released bool `json:"released"`
		}
		if json.Unmarshal([]byte(data), &ack) != nil || !ack.Released {
			return result, errors.New("Proxmox Tailnet broker did not confirm release")
		}
		return result, nil
	}
	if json.Unmarshal([]byte(data), &result) != nil {
		return proxmoxTailnetCredential{}, errors.New("invalid Proxmox Tailnet broker response")
	}
	now := time.Now()
	if !strings.HasPrefix(result.AuthKey, "tskey-auth-") || len(result.AuthKey) > 4096 || strings.ContainsAny(result.AuthKey, "\r\n\x00") || !result.Ephemeral || (result.Reusable == nil || *result.Reusable) || !result.ExpiresAt.After(now) || result.ExpiresAt.After(now.Add(10*time.Minute)) {
		return proxmoxTailnetCredential{}, errors.New("Proxmox Tailnet broker must issue a short-lived nonreusable ephemeral key")
	}
	return result, nil
}

func proxmoxTailnetRequest(cfg core.Config, server core.Server, operation string) (proxmoxTailnetBrokerRequest, error) {
	if server.Provider != "proxmox" || !core.IsCanonicalLeaseID(server.Labels["lease"]) || server.CloudID == "" || server.HostID == "" || core.ProxmoxGenerationID(server.ImmutableID) == "" || server.Labels["tailscale"] != "true" {
		return proxmoxTailnetBrokerRequest{}, errors.New("Proxmox Tailnet operation requires a bound lease, VMID, node and generation")
	}
	return proxmoxTailnetBrokerRequest{Version: 1, Operation: operation, Endpoint: strings.TrimSuffix(strings.TrimRight(cfg.Proxmox.APIURL, "/"), "/api2/json"), Node: server.HostID, LeaseID: server.Labels["lease"], VMID: server.CloudID, Generation: server.ImmutableID, Hostname: server.Labels["tailscale_hostname"], Tags: core.NormalizeList(strings.Split(server.Labels["tailscale_tags"], ","))}, nil
}

// ReleaseProxmoxTailnet is idempotent at the broker and runs before a fixed
// claim becomes terminal, including retries where the VM is already absent.
func ReleaseProxmoxTailnet(ctx context.Context, cfg core.Config, server core.Server) error {
	request, err := proxmoxTailnetRequest(cfg, server, "release")
	if err != nil {
		return err
	}
	_, err = proxmoxRunTailnetBroker(ctx, cfg.Proxmox.TailscaleBroker, request)
	return err
}

// Both identity and issuance policy must still match the durable clone inputs.
// Agreement between successive mutable description reads is not ownership.
func validateProxmoxTailnetEnrollmentBinding(current, bound core.Server) error {
	if current.Provider != bound.Provider || current.CloudID != bound.CloudID || current.HostID != bound.HostID || current.ImmutableID != bound.ImmutableID {
		return errors.New("Proxmox Tailnet guest identity changed before enrollment")
	}
	for _, key := range []string{"lease", "fixed_intent_sha256", "tailscale", "tailscale_hostname", "tailscale_tags"} {
		if current.Labels[key] != bound.Labels[key] {
			return errors.New("Proxmox Tailnet guest identity or bound policy changed before enrollment")
		}
	}
	return nil
}

func provisionTailnet(ctx context.Context, c *core.ProxmoxClient, cfg core.Config, server core.Server) error {
	ctx, cancel := context.WithTimeout(ctx, core.BootstrapWaitTimeout(cfg))
	defer cancel()
	current, err := c.GetServerOnNode(ctx, server.HostID, server.CloudID)
	if err != nil {
		return err
	}
	if err := validateProxmoxTailnetEnrollmentBinding(current, server); err != nil {
		return err
	}
	request, err := proxmoxTailnetRequest(cfg, server, "acquire")
	if err != nil {
		return err
	}
	if !validProxmoxTailnetName(request.Hostname, 63) {
		return core.Exit(2, "invalid Proxmox Tailnet rendered hostname")
	}
	if err := validateProxmoxTailnetTags(request.Tags); err != nil {
		return err
	}
	// Check the exact isolated capability before issuing a key. This check never
	// grants permissions or falls back to the controller/CI principal.
	var permissions map[string]map[string]proxmoxPrivilegeValue
	permissionPath := "/vms/" + server.CloudID
	if err := c.RequestRequired(ctx, http.MethodGet, "/access/permissions?path="+url.QueryEscape(permissionPath), nil, &permissions); err != nil {
		return err
	}
	if _, ok := permissions[permissionPath]["VM.GuestAgent.Unrestricted"]; !ok {
		return errors.New("Proxmox Tailnet enrollment requires VM.GuestAgent.Unrestricted on " + permissionPath + "; operator must approve this isolated runtime capability")
	}
	vmid, err := strconv.Atoi(server.CloudID)
	if err != nil || vmid <= 0 {
		return errors.New("invalid Proxmox Tailnet VMID")
	}
	// Wait for QGA, without waiting for (or using) an underlay SSH address.
	for {
		_, err = c.GuestIPv4(ctx, vmid, false)
		if err == nil {
			break
		}
		if err = proxmoxTailnetWait(ctx); err != nil {
			return err
		}
	}
	// Re-read generation immediately before issuing any credential.
	current, err = c.GetServerOnNode(ctx, server.HostID, server.CloudID)
	if err != nil {
		return err
	}
	if err := validateProxmoxTailnetEnrollmentBinding(current, server); err != nil {
		return err
	}
	credential, err := proxmoxRunTailnetBroker(ctx, cfg.Proxmox.TailscaleBroker, request)
	if err != nil {
		return err
	}
	input, err := json.Marshal(struct {
		AuthKey  string   `json:"authKey"`
		Hostname string   `json:"hostname"`
		Tags     []string `json:"tags"`
	}{credential.AuthKey, request.Hostname, request.Tags})
	if err != nil {
		return errors.New("encode Proxmox Tailnet enrollment input")
	}
	// PVE's REST API accepts raw input-data and base64-encodes it for QGA.
	path := fmt.Sprintf("/nodes/%s/qemu/%s/agent/exec", url.PathEscape(server.HostID), url.PathEscape(server.CloudID))
	var process struct {
		PID int `json:"pid"`
	}
	if err := tailnetAgentRequest(ctx, c, http.MethodPost, path, url.Values{"command": {"/usr/local/libexec/crabbox-enroll"}, "input-data": {string(input)}}, &process); err != nil {
		return err
	}
	if process.PID <= 0 {
		return errors.New("Proxmox Tailnet helper did not return a PID")
	}
	for {
		var status struct {
			Exited   proxmoxAgentBool `json:"exited"`
			ExitCode *int             `json:"exitcode"`
			Signal   *int             `json:"signal"`
		}
		if err := tailnetAgentRequest(ctx, c, http.MethodGet, path+"-status?pid="+strconv.Itoa(process.PID), nil, &status); err != nil {
			return err
		}
		if status.Exited {
			if status.ExitCode == nil || *status.ExitCode != 0 || status.Signal != nil {
				return errors.New("Proxmox Tailnet helper failed (output suppressed)")
			}
			return nil
		}
		if err := proxmoxTailnetWait(ctx); err != nil {
			return err
		}
	}
}

func proxmoxTailnetWait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return nil
	}
}

// Never promote secret-bearing request/response bodies or helper output into
// errors, even if a proxy echoes input-data. Do not follow credential redirects.
func tailnetAgentRequest(ctx context.Context, c *core.ProxmoxClient, method, path string, form url.Values, out any) error {
	scoped := *c
	client := *c.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	scoped.Client = &client
	if err := scoped.RequestRequired(ctx, method, path, form, out); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Proxmox Tailnet guest-agent request failed (body suppressed); requires VM.GuestAgent.Unrestricted on the worker VM")
	}
	return nil
}

// Install adapter-owned policy on a native client; the client owns transport and
// ordering only, never broker credentials or enrollment/cleanup decisions.
func configureTailnetClient(client *core.ProxmoxClient) {
	client.GuestAdmission = func(cfg core.Config, leaseID, slug string, labels map[string]string) (map[string]string, error) {
		if !cfg.Tailscale.Enabled {
			return nil, nil
		}
		if err := ValidateProxmoxTailnetConfig(cfg); err != nil {
			return nil, err
		}
		if labels["fixed_intent_sha256"] == "" {
			return nil, core.Exit(2, "Proxmox Tailnet provisioning requires a fixed lease")
		}
		if err := ValidateProxmoxTailnetIdentity(cfg, leaseID, slug); err != nil {
			return nil, err
		}
		return ProxmoxTailnetIdentityLabels(cfg, leaseID, slug), nil
	}
	client.GuestEnrollment = func(ctx context.Context, cfg core.Config, server core.Server) error {
		return provisionTailnet(ctx, client, cfg, server)
	}
}

// exec copies stdout concurrently. Retain only the private protocol limit and
// report overflow even when a noisy child exits successfully.
type brokerOutput struct {
	mu        sync.Mutex
	data      bytes.Buffer
	limit     int
	truncated bool
}

func newBrokerOutput(limit int) brokerOutput { return brokerOutput{limit: limit} }
func (b *brokerOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(data)
	left := b.limit - b.data.Len()
	if len(data) > left {
		data = data[:left]
		b.truncated = true
	}
	_, _ = b.data.Write(data)
	return n, nil
}
func (b *brokerOutput) boundedString() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String(), b.truncated
}

type proxmoxPrivilegeValue int

func (i *proxmoxPrivilegeValue) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*i = proxmoxPrivilegeValue(n)
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
	*i = proxmoxPrivilegeValue(n)
	return nil
}
