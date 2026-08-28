package boxd

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
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const defaultConsoleURL = "https://app.boxd.sh"
const maxAPIResponse = 4 << 20

// consoleClient uses the console's HTTPS API. The vendor CLI and SDK default
// to a separate, plaintext gRPC endpoint and must not handle these credentials.
type consoleClient struct {
	base  *url.URL
	http  *http.Client
	org   string
	token string
}

type consoleMachine struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	PublicIP string `json:"public_ip"`
	Isolated bool   `json:"isolated"`
	OwnerID  string `json:"owner_id"`
}

// Create returns vm_id, while inventory returns id and the isolation proof.
type consoleCreateResult struct {
	VMID     string `json:"vm_id"`
	Name     string `json:"name"`
	PublicIP string `json:"public_ip"`
	Status   string `json:"status"`
	URL      string `json:"url"`
}

type consoleForward struct {
	Endpoint   string `json:"endpoint"`
	PublicPort int    `json:"public_port"`
	VMPort     int    `json:"vm_port"`
	Protocol   string `json:"protocol"`
}

type consoleHTTPError struct{ Code int }

func (e *consoleHTTPError) Error() string {
	if e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden {
		return fmt.Sprintf("boxd HTTPS API returned HTTP %d; use an interactive BOXD_TOKEN session from node scripts/boxd-login.mjs --output <private-file>; API-key and in-VM sessions cannot perform console mutations", e.Code)
	}
	return fmt.Sprintf("boxd HTTPS API returned HTTP %d", e.Code)
}

func consoleURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		raw = defaultConsoleURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return nil, core.Exit(2, "boxd.apiUrl must be an HTTPS console origin without credentials, path, query, or fragment")
	}
	u.Path = ""
	u.Host = strings.ToLower(u.Host)
	if u.Port() == "443" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	if u.RawPath != "" || strings.HasSuffix(raw, "#") {
		return nil, core.Exit(2, "invalid boxd HTTPS origin")
	}
	return u, nil
}

func newConsoleClient(cfg core.Config, rt core.Runtime, token string) (*consoleClient, error) {
	base, err := consoleURL(cfg.Boxd.APIURL)
	if err != nil {
		return nil, err
	}
	if err := validateSessionToken(token); err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 30 * time.Second}
	if rt.HTTP != nil {
		client = *rt.HTTP
	}
	// Even same-origin redirects can turn a create into a GET. There is no
	// redirect in the console contract, so do not replay credentials or writes.
	client.Jar = nil
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("boxd API redirects are not allowed") }
	return &consoleClient{base: base, http: &client, token: token, org: cfg.Boxd.Org}, nil
}

func validateSessionToken(token string) error {
	if strings.HasPrefix(strings.TrimSpace(token), "bxd_") {
		return core.Exit(2, "BOXD_TOKEN requires an interactive session, not a bxd_ API key: console mutations reject API-key sessions; run node scripts/boxd-login.mjs --output <private-file>")
	}
	if token == "" {
		return core.Exit(2, "set BOXD_TOKEN or CRABBOX_BOXD_TOKEN to an interactive session from node scripts/boxd-login.mjs --output <private-file>; API keys are not supported")
	}
	if len(token) > 16<<10 {
		return core.Exit(2, "invalid Boxd interactive session token")
	}
	for _, c := range token {
		if c < 33 || c > 126 {
			return core.Exit(2, "invalid Boxd interactive session token")
		}
	}
	return nil
}

func (c *consoleClient) call(ctx context.Context, method, route string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return core.Exit(2, "encode boxd API request")
		}
	}
	u := *c.base
	parts, err := url.Parse(route)
	if err != nil || parts.IsAbs() || parts.Host != "" || !strings.HasPrefix(parts.Path, "/api/v1/") {
		return core.Exit(2, "invalid boxd API route")
	}
	u.Path, u.RawPath, u.RawQuery = parts.Path, parts.RawPath, parts.RawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(data))
	if err != nil {
		return core.Exit(2, "construct boxd API request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Transport errors can include request headers or URLs. Keep secrets
		// out of diagnostics even with a custom injected HTTP transport.
		return core.Exit(5, "boxd HTTPS request failed; response details withheld")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &consoleHTTPError{Code: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse+1))
	if err != nil {
		return core.Exit(5, "read boxd HTTPS response")
	}
	if len(payload) > maxAPIResponse {
		return core.Exit(5, "boxd HTTPS response exceeds size limit")
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return core.Exit(5, "invalid boxd HTTPS response")
	}
	return nil
}

func (c *consoleClient) whoami(ctx context.Context) (string, error) {
	var result struct {
		UserID string `json:"user_id"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/whoami", nil, &result); err != nil {
		return "", err
	}
	if result.UserID == "" {
		return "", core.Exit(5, "boxd HTTPS identity response has no user ID")
	}
	return result.UserID, nil
}

func (c *consoleClient) machines(ctx context.Context) ([]consoleMachine, error) {
	route := "/api/v1/vms"
	if c.org != "" {
		route += "?org=" + url.QueryEscape(c.org)
	}
	var result []consoleMachine
	err := c.call(ctx, http.MethodGet, route, nil, &result)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, core.Exit(5, "boxd inventory must be a JSON array")
	}
	seen := make(map[string]bool)
	for _, vm := range result {
		if _, err := machineRoute(vm.ID, ""); err != nil || seen[vm.ID] || vm.OwnerID == "" {
			return nil, core.Exit(5, "boxd inventory contains invalid machine identities")
		}
		seen[vm.ID] = true
	}
	return result, nil
}

func (c *consoleClient) create(ctx context.Context, name string) (consoleMachine, error) {
	var result consoleCreateResult
	err := c.call(ctx, http.MethodPost, "/api/v1/vms", map[string]any{"name": name, "org": c.org, "isolated": true}, &result)
	return consoleMachine{ID: result.VMID, Name: result.Name, PublicIP: result.PublicIP, Status: result.Status}, err
}

func machineRoute(id, action string) (string, error) {
	if len(id) == 0 || len(id) > 128 {
		return "", core.Exit(2, "invalid boxd machine ID")
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "", core.Exit(2, "invalid boxd machine ID")
		}
	}
	return "/api/v1/vms/" + url.PathEscape(id) + "/" + action, nil
}

func (c *consoleClient) action(ctx context.Context, id, action string) error {
	switch action {
	case "start", "stop", "destroy":
	default:
		return core.Exit(2, "invalid boxd machine action")
	}
	route, err := machineRoute(id, action)
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodPost, route, nil, nil)
}

func (c *consoleClient) exposeSSH(ctx context.Context, id string) (consoleForward, error) {
	route, err := machineRoute(id, "expose")
	if err != nil {
		return consoleForward{}, err
	}
	var result consoleForward
	err = c.call(ctx, http.MethodPost, route, map[string]any{"vm_port": 2222, "protocol": "tcp"}, &result)
	if err == nil && (result.PublicPort < 1 || result.PublicPort > 65535 || result.VMPort != 2222 || result.Protocol != "tcp") {
		err = core.Exit(5, "boxd returned an invalid SSH port forward")
	}
	return result, err
}
