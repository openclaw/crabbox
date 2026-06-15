package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// phalaProxyEndpoint models the SSH gateway host/port that fronts a Phala
// confidential CVM. dstack publishes a per-CVM gateway domain and port that
// forwards to the CVM's sshd; `phala ssh <id> -g <gateway> -p <port>` documents
// this offline transport. The exact JSON field names are resolved defensively
// across the candidate keys `phala cvms get --json` may emit.
//
// TODO(phala): confirm the gateway/port field names against a live CVM. The
// candidate set below is derived from the documented offline `-g`/`-p` form and
// the dstack gateway model, not yet from an observed populated `cvms get`
// payload.
type phalaProxyEndpoint struct {
	Gateway     string `json:"gateway"`
	GatewayHost string `json:"gateway_host"`
	GatewayURL  string `json:"gateway_url"`
	Host        string `json:"host"`
	Domain      string `json:"domain"`
	Port        int    `json:"port"`
	GatewayPort int    `json:"gateway_port"`
	SSHPort     int    `json:"ssh_port"`
}

type phalaCVMGetOutput struct {
	Success bool                `json:"success"`
	CVM     *phalaProxyEndpoint `json:"cvm"`
	phalaProxyEndpoint
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

	endpoint, err := resolvePhalaProxyEndpoint(ctx, *phala, cvmID, a.Stderr)
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return exit(2, "connect Phala CVM gateway %s: %v", endpoint, err)
	}
	if copyErr := copyPhalaProxyStreams(ctx, conn, a.input(), a.Stdout); copyErr != nil &&
		!errors.Is(copyErr, net.ErrClosed) && ctx.Err() == nil {
		return exit(2, "Phala proxy stream: %v", copyErr)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func resolvePhalaProxyEndpoint(ctx context.Context, phala, cvmID string, stderr io.Writer) (string, error) {
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
	endpoint := parsed.phalaProxyEndpoint
	if parsed.CVM != nil {
		endpoint = mergePhalaProxyEndpoint(*parsed.CVM, endpoint)
	}
	host := firstNonBlank(
		phalaGatewayHost(endpoint.Gateway),
		phalaGatewayHost(endpoint.GatewayURL),
		strings.TrimSpace(endpoint.GatewayHost),
		strings.TrimSpace(endpoint.Host),
		strings.TrimSpace(endpoint.Domain),
	)
	if host == "" {
		return "", exit(5, "phala cvms get output omitted an SSH gateway host")
	}
	port := firstNonZero(endpoint.GatewayPort, endpoint.SSHPort, endpoint.Port, phalaGatewayPort(endpoint.Gateway), phalaGatewayPort(endpoint.GatewayURL))
	if port == 0 {
		port = 443
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	if _, _, err := net.SplitHostPort(address); err != nil {
		return "", exit(5, "invalid phala gateway endpoint %q: %v", address, err)
	}
	return address, nil
}

func mergePhalaProxyEndpoint(primary, fallback phalaProxyEndpoint) phalaProxyEndpoint {
	if strings.TrimSpace(primary.Gateway) == "" {
		primary.Gateway = fallback.Gateway
	}
	if strings.TrimSpace(primary.GatewayHost) == "" {
		primary.GatewayHost = fallback.GatewayHost
	}
	if strings.TrimSpace(primary.GatewayURL) == "" {
		primary.GatewayURL = fallback.GatewayURL
	}
	if strings.TrimSpace(primary.Host) == "" {
		primary.Host = fallback.Host
	}
	if strings.TrimSpace(primary.Domain) == "" {
		primary.Domain = fallback.Domain
	}
	if primary.Port == 0 {
		primary.Port = fallback.Port
	}
	if primary.GatewayPort == 0 {
		primary.GatewayPort = fallback.GatewayPort
	}
	if primary.SSHPort == 0 {
		primary.SSHPort = fallback.SSHPort
	}
	return primary
}

func phalaGatewayHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "ssh://")
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return host
	}
	if idx := strings.IndexAny(raw, "/?#"); idx >= 0 {
		raw = raw[:idx]
	}
	return raw
}

func phalaGatewayPort(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "ssh://")
	if _, port, err := net.SplitHostPort(raw); err == nil {
		if value, convErr := strconv.Atoi(port); convErr == nil {
			return value
		}
	}
	return 0
}

func copyPhalaProxyStreams(ctx context.Context, conn net.Conn, input io.Reader, output io.Writer) error {
	copyDone := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(conn, input)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyDone <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(output, conn)
		copyDone <- copyErr
	}()
	var streamErr error
	for i := 0; i < 2; i++ {
		select {
		case copyErr := <-copyDone:
			if copyErr != nil && !errors.Is(copyErr, net.ErrClosed) && streamErr == nil {
				streamErr = copyErr
			}
			if i == 0 {
				_ = conn.Close()
			}
		case <-ctx.Done():
			_ = conn.Close()
			return ctx.Err()
		}
	}
	return streamErr
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
