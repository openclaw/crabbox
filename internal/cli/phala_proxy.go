package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os/exec"
	"strings"
)

// phalaGateway models the `gateway` object `phala cvms get --json` emits for a
// CVM. dstack fronts each CVM's sshd behind a TLS gateway: SSH for app <appId>
// is reached at host `<appId>-22.<gateway-domain>` over TLS on :443, where the
// gateway domain comes from gateway_domain (or the nested base_domain). crabbox
// tunnels SSH stdio through that TLS endpoint with `openssl s_client`.
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
	fs := flag.NewFlagSet("__phala-proxy", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	phala := fs.String("phala", "phala", "phala CLI path")
	nodeID := fs.String("node-id", "", "Phala node id")
	if err := fs.Parse(args); err != nil {
		return exit(2, "%v", err)
	}
	if fs.NArg() != 1 {
		return exit(2, "phala proxy requires a CVM id")
	}
	cvmID := fs.Arg(0)
	_ = nodeID // reserved for future node-scoped gateway resolution

	host, err := resolvePhalaProxyHost(ctx, *phala, cvmID, a.Stderr)
	if err != nil {
		return err
	}
	return tunnelPhalaProxy(ctx, host, a.input(), a.Stdout, a.Stderr)
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

// tunnelPhalaProxy pipes SSH stdio through the Phala TLS gateway using
// `openssl s_client`. dstack terminates the gateway with TLS on :443 and routes
// by SNI, so the servername must match the derived host. The connection carries
// raw SSH bytes once the TLS session is up; openssl is therefore a host
// dependency for this provider.
func tunnelPhalaProxy(ctx context.Context, host string, input io.Reader, output, stderr io.Writer) error {
	if _, err := exec.LookPath("openssl"); err != nil {
		return exit(2, "phala SSH gateway requires the openssl client: %v", err)
	}
	cmd := exec.CommandContext(ctx, "openssl", "s_client",
		"-connect", host+":443",
		"-quiet",
		"-verify_quiet",
		"-servername", host,
	)
	cmd.Stdin = input
	cmd.Stdout = output
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return exit(exitCode(err), "tunnel Phala SSH gateway %s via openssl s_client: %v", host, err)
	}
	return nil
}

// phalaJSONObjectPrefix mirrors the provider-side prefix scanner: it returns the
// leading JSON object from a CLI stream, discarding the libuv assertion line the
// phala CLI appends on some platforms.
func phalaJSONObjectPrefix(stdout string) string {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return ""
	}
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
