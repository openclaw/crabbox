package cli

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os/exec"
	"strings"
)

// phalaGateway models the `gateway` object `phala cvms get --json` emits for a
// CVM. dstack fronts each CVM's sshd behind a TLS gateway: SSH for app <appId>
// is reached at host `<appId>-22.<gateway-domain>` over TLS on :443, where the
// gateway domain comes from gateway_domain (or the nested base_domain). crabbox
// tunnels SSH stdio through that TLS endpoint with Go's TLS client.
type phalaGateway struct {
	GatewayDomain string `json:"gateway_domain"`
	BaseDomain    string `json:"base_domain"`
	Domain        string `json:"domain"`
}

// domain returns the gateway base domain, preferring gateway_domain then the
// nested base_domain/domain fields.
func (g phalaGateway) domain() string {
	return firstNonBlank(
		strings.TrimSpace(g.GatewayDomain),
		strings.TrimSpace(g.BaseDomain),
		strings.TrimSpace(g.Domain),
	)
}

// phalaCVM models the subset of a `cvms get --json` CVM object crabbox needs to
// build the SSH gateway host: the app id and the gateway object.
type phalaCVM struct {
	AppID      string       `json:"app_id"`
	AppIDAlt   string       `json:"appId"`
	ID         string       `json:"id"`
	InstanceID string       `json:"instance_id"`
	Gateway    phalaGateway `json:"gateway"`

	// GatewayDomain accepts a top-level gateway_domain emitted alongside (rather
	// than inside) the gateway object on some payloads.
	GatewayDomain string `json:"gateway_domain"`
}

// appID returns the app id used as the `<appId>-22` host label, preferring the
// canonical app_id then its camelCase and id aliases.
func (c phalaCVM) appID() string {
	return firstNonBlank(
		strings.TrimSpace(c.AppID),
		strings.TrimSpace(c.AppIDAlt),
		strings.TrimSpace(c.ID),
		strings.TrimSpace(c.InstanceID),
	)
}

// gatewayDomain returns the gateway base domain from the gateway object or a
// top-level gateway_domain fallback.
func (c phalaCVM) gatewayDomain() string {
	return firstNonBlank(c.Gateway.domain(), strings.TrimSpace(c.GatewayDomain))
}

type phalaCVMGetOutput struct {
	Success bool      `json:"success"`
	CVM     *phalaCVM `json:"cvm"`
	phalaCVM
}

func (a App) phalaProxy(ctx context.Context, args []string) error {
	return a.phalaProxyWithTunnel(ctx, args, tunnelPhalaProxy)
}

func (a App) phalaProxyWithTunnel(ctx context.Context, args []string, tunnel func(context.Context, string, io.Reader, io.Writer) error) error {
	fs := flag.NewFlagSet("__phala-proxy", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	phala := fs.String("phala", "phala", "phala CLI path")
	nodeID := fs.String("node-id", "", "Phala node id")
	gatewayHost := fs.String("gateway-host", "", "pre-resolved TLS SSH gateway host (skips the per-connection cvms-get lookup)")
	if err := fs.Parse(args); err != nil {
		return exit(2, "%v", err)
	}
	if fs.NArg() != 1 {
		return exit(2, "phala proxy requires a CVM id")
	}
	cvmID := fs.Arg(0)
	_ = nodeID // reserved for future node-scoped gateway resolution

	// A cached gateway host (resolved once at acquire time and carried on the
	// SSH ProxyCommand) lets every SSH connection tunnel straight to the TLS
	// gateway, skipping the `phala cvms get` API round-trip. This is what keeps
	// the short `status --wait` readiness probe inside its timeout budget.
	host := strings.TrimSpace(*gatewayHost)
	if host == "" {
		var err error
		host, err = resolvePhalaProxyHost(ctx, *phala, cvmID, a.Stderr)
		if err != nil {
			return err
		}
	}
	return tunnel(ctx, host, a.input(), a.Stdout)
}

// resolvePhalaProxyHost queries `phala cvms get --json` and derives the TLS SSH
// gateway host `<appId>-22.<gateway-domain>`. The phala CLI authenticates from
// its own stored credentials; no API key is ever passed on the command line.
func resolvePhalaProxyHost(ctx context.Context, phala, cvmID string, stderr io.Writer) (string, error) {
	cmd := exec.CommandContext(ctx, phala, "cvms", "get", "--cvm-id", cvmID, "--json")
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return "", exit(exitCode(err), "phala cvms get: %v", err)
	}
	payload := phalaJSONObjectPrefix(string(out))
	if payload == "" {
		return "", exit(5, "phala cvms get produced no JSON output")
	}
	var parsed phalaCVMGetOutput
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return "", exit(5, "parse phala cvms get output: %v", err)
	}
	cvm := parsed.phalaCVM
	appID := firstNonBlank(cvm.appID(), cvmGetAppID(parsed.CVM))
	domain := firstNonBlank(cvm.gatewayDomain(), cvmGetGatewayDomain(parsed.CVM))
	if appID == "" {
		return "", exit(5, "phala cvms get output omitted the CVM app id")
	}
	if domain == "" {
		return "", exit(5, "phala cvms get output omitted the gateway domain")
	}
	return appID + "-22." + domain, nil
}

func cvmGetAppID(cvm *phalaCVM) string {
	if cvm == nil {
		return ""
	}
	return cvm.appID()
}

func cvmGetGatewayDomain(cvm *phalaCVM) string {
	if cvm == nil {
		return ""
	}
	return cvm.gatewayDomain()
}

type phalaTLSDialFunc func(context.Context, string, string) (net.Conn, error)

// tunnelPhalaProxy pipes SSH stdio through the Phala TLS gateway. dstack
// terminates the gateway with TLS on :443 and routes by SNI, so the server name
// must match the derived host. Go's TLS client verifies both the public CA chain
// and the hostname before any SSH bytes are exchanged.
func tunnelPhalaProxy(ctx context.Context, host string, input io.Reader, output io.Writer) error {
	config := phalaTLSConfig(host)
	dialer := &tls.Dialer{Config: config}
	return tunnelPhalaProxyWithDialer(ctx, host, input, output, dialer.DialContext)
}

func phalaTLSConfig(host string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: host,
	}
}

func tunnelPhalaProxyWithDialer(ctx context.Context, host string, input io.Reader, output io.Writer, dial phalaTLSDialFunc) error {
	conn, err := dial(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return exit(1, "connect Phala SSH TLS gateway %s: %v", host, err)
	}
	defer conn.Close()

	type copyResult struct {
		direction string
		err       error
	}
	results := make(chan copyResult, 2)
	go func() {
		_, copyErr := io.Copy(conn, input)
		if closeWriter, ok := conn.(interface{ CloseWrite() error }); ok {
			copyErr = errors.Join(copyErr, closeWriter.CloseWrite())
		}
		results <- copyResult{direction: "upload", err: copyErr}
	}()
	go func() {
		_, copyErr := io.Copy(output, conn)
		results <- copyResult{direction: "download", err: copyErr}
	}()

	uploadDone := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-results:
			if result.direction == "upload" && result.err == nil {
				uploadDone = true
				continue
			}
			if result.err != nil && !errors.Is(result.err, net.ErrClosed) {
				return exit(1, "tunnel Phala SSH gateway %s %s: %v", host, result.direction, result.err)
			}
			if result.direction == "download" || uploadDone {
				return nil
			}
		}
	}
}

// phalaJSONObjectPrefix mirrors the provider-side prefix scanner: it returns the
// first top-level JSON object embedded in a CLI stream, discarding BOTH a
// leading human progress line (e.g. "Provisioning CVM ...") and the libuv
// assertion line the phala CLI appends on some platforms.
func phalaJSONObjectPrefix(stdout string) string {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return ""
	}
	start := strings.IndexAny(trimmed, "{[")
	if start < 0 {
		return ""
	}
	trimmed = trimmed[start:]
	open, closeCh := byte('{'), byte('}')
	if trimmed[0] == '[' {
		open, closeCh = '[', ']'
	}
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return trimmed[:i+1]
			}
		}
	}
	return trimmed
}
