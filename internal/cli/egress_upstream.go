package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type egressUpstreamProxy struct {
	address       string
	serverName    string
	tls           bool
	authorization string
}

func egressUpstreamProxyFromEnv(name string) (*egressUpstreamProxy, error) {
	if !ValidShellEnvName(name) {
		return nil, Exit(2, "--upstream-proxy-env requires an environment variable name")
	}
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, Exit(2, "upstream proxy environment variable %s is empty", name)
	}
	u, err := url.Parse(raw)
	// URL parser errors may contain credentials. Keep configuration diagnostics
	// local and name-only; never return the raw URL or its parser error.
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, Exit(2, "upstream proxy environment variable %s must contain an HTTP(S) proxy URL without a path, query, or fragment", name)
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, Exit(2, "upstream proxy environment variable %s has an invalid port", name)
	}
	proxy := &egressUpstreamProxy{
		address:    net.JoinHostPort(u.Hostname(), strconv.Itoa(portNumber)),
		serverName: u.Hostname(),
		tls:        u.Scheme == "https",
	}
	if u.User != nil {
		password, _ := u.User.Password()
		proxy.authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password))
	}
	return proxy, nil
}

func (p *egressUpstreamProxy) dialPublicHost(ctx context.Context, host, port string) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, egressDialTimeout)
	defer cancel()
	_, port, err := publicEgressDestination(dialCtx, host, port, net.DefaultResolver.LookupNetIP)
	if err != nil {
		return nil, err
	}
	// CONNECT must retain the hostname for upstream routing and credential
	// policy. The explicitly trusted proxy owns final DNS resolution and pinning.
	return p.dialTunnel(dialCtx, net.JoinHostPort(strings.TrimSpace(strings.Trim(host, "[]")), port))
}

func (p *egressUpstreamProxy) dialTunnel(ctx context.Context, destination string) (_ net.Conn, err error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.address)
	if err != nil {
		return nil, errors.New("connect upstream proxy failed")
	}
	rawConn := conn
	stopCancellation := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stopCancellation()
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()
	if p.tls {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: p.serverName, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, errors.New("upstream proxy TLS handshake failed")
		}
		conn = tlsConn
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: destination},
		Host:   destination,
		Header: make(http.Header),
	}
	if p.authorization != "" {
		req.Header.Set("Proxy-Authorization", p.authorization)
	}
	if err := req.Write(conn); err != nil {
		return nil, errors.New("write upstream proxy CONNECT failed")
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, errors.New("read upstream proxy CONNECT response failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Neither the reason phrase, headers, nor body are safe to echo: proxies
		// can reflect authentication material into their error responses.
		return nil, fmt.Errorf("upstream proxy CONNECT returned HTTP %d", response.StatusCode)
	}
	if !stopCancellation() {
		return nil, context.Cause(ctx)
	}
	return &egressUpstreamConn{Conn: conn, reader: reader}, nil
}

type egressUpstreamConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *egressUpstreamConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}
