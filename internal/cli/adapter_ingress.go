package cli

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	adapterIngressMaxConfigBytes = 16 << 10
	adapterIngressMaxHeaderBytes = 16 << 10
	adapterIngressMaxHeaders     = 100
)

type adapterIngressConfig struct {
	Listen              string   `json:"listen"`
	Upstream            string   `json:"upstream"`
	PublicOrigin        string   `json:"publicOrigin"`
	IdentityHeader      string   `json:"identityHeader"`
	Identity            string   `json:"identity"`
	SecretHeader        string   `json:"secretHeader"`
	SecretFile          string   `json:"secretFile"`
	DenyPaths           []string `json:"denyPaths,omitempty"`
	DenyPrefixes        []string `json:"denyPrefixes,omitempty"`
	StripHeaderPrefixes []string `json:"stripHeaderPrefixes,omitempty"`

	upstreamURL     *url.URL
	publicOriginURL *url.URL
}

type adapterIngressProxy struct {
	config  adapterIngressConfig
	secret  string
	health  *http.Client
	reverse *httputil.ReverseProxy
}

func (a App) adapterIngress(ctx context.Context, args []string) error {
	fs := newFlagSet("adapter ingress", a.Stderr)
	configPath := fs.String("config", getenv("CRABBOX_ADAPTER_INGRESS_CONFIG", ""), "private JSON ingress config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*configPath) == "" {
		return exit(2, "usage: crabbox adapter ingress --config <path>")
	}
	config, err := loadAdapterIngressConfig(*configPath)
	if err != nil {
		return err
	}
	proxy, err := newAdapterIngressProxy(config)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return exit(5, "listen on %s: %v", config.Listen, err)
	}
	defer listener.Close()
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	server := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    adapterIngressMaxHeaderBytes,
		BaseContext: func(net.Listener) context.Context {
			return serveCtx
		},
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-serveCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(a.Stderr, "adapter ingress listening=%s upstream=%s\n", listener.Addr(), config.upstreamURL)
	err = server.Serve(listener)
	cancel()
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func loadAdapterIngressConfig(path string) (adapterIngressConfig, error) {
	path = expandUserPath(strings.TrimSpace(path))
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return adapterIngressConfig{}, exit(2, "adapter ingress config path must be absolute and clean")
	}
	data, err := readPrivateAdapterFile(path, adapterIngressMaxConfigBytes, "adapter ingress config")
	if err != nil {
		return adapterIngressConfig{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var config adapterIngressConfig
	if err := decoder.Decode(&config); err != nil {
		return adapterIngressConfig{}, exit(2, "decode adapter ingress config: %v", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return adapterIngressConfig{}, exit(2, "decode adapter ingress config: %v", err)
	}
	return validateAdapterIngressConfig(config)
}

func readPrivateAdapterFile(path string, maximum int64, label string) ([]byte, error) {
	file, err := openControllerTokenFile(path)
	if err != nil {
		return nil, exit(2, "read %s file: %v", label, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, exit(2, "stat %s file: %v", label, err)
	}
	if !info.Mode().IsRegular() {
		return nil, exit(2, "%s file must be regular", label)
	}
	if mode := info.Mode().Perm(); mode != 0o400 && mode != 0o600 {
		return nil, exit(2, "%s file must have mode 0400 or 0600", label)
	}
	if info.Size() > maximum {
		return nil, exit(2, "%s file is too large", label)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, exit(2, "read %s file: %v", label, err)
	}
	if int64(len(data)) > maximum {
		return nil, exit(2, "%s file is too large", label)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, exit(2, "stat %s file after reading: %v", label, err)
	}
	if !adapterIngressPrivateFileStable(info, after, int64(len(data))) {
		return nil, exit(2, "%s file changed while it was being read", label)
	}
	return data, nil
}

func adapterIngressPrivateFileStable(before, after fs.FileInfo, readBytes int64) bool {
	return before != nil && after != nil && readBytes == before.Size() && after.Size() == before.Size() && after.Mode() == before.Mode() && after.ModTime().Equal(before.ModTime())
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func validateAdapterIngressConfig(config adapterIngressConfig) (adapterIngressConfig, error) {
	listen, err := validateAdapterIngressListen(config.Listen)
	if err != nil {
		return adapterIngressConfig{}, err
	}
	upstream, err := validateAdapterIngressUpstream(config.Upstream)
	if err != nil {
		return adapterIngressConfig{}, err
	}
	if adapterIngressEndpointsOverlap(listen, upstream) {
		return adapterIngressConfig{}, exit(2, "adapter ingress listen and upstream must differ")
	}
	publicOrigin, err := validateAdapterIngressPublicOrigin(config.PublicOrigin)
	if err != nil {
		return adapterIngressConfig{}, err
	}
	identityHeader, err := validateAdapterIngressHeader(config.IdentityHeader, "identityHeader")
	if err != nil {
		return adapterIngressConfig{}, err
	}
	secretHeader, err := validateAdapterIngressHeader(config.SecretHeader, "secretHeader")
	if err != nil {
		return adapterIngressConfig{}, err
	}
	if strings.EqualFold(identityHeader, secretHeader) {
		return adapterIngressConfig{}, exit(2, "adapter ingress identityHeader and secretHeader must differ")
	}
	for _, header := range []string{identityHeader, secretHeader} {
		name := strings.ToLower(header)
		if adapterIngressReservedAuthHeader(name) {
			return adapterIngressConfig{}, exit(2, "adapter ingress authentication headers must not use reserved header %s", header)
		}
	}
	identity := strings.TrimSpace(config.Identity)
	if identity == "" || identity != config.Identity || len(identity) > 1024 || !validAdapterIngressHeaderValue(identity) {
		return adapterIngressConfig{}, exit(2, "adapter ingress identity must be one nonempty bounded header value")
	}
	secretFile := expandUserPath(strings.TrimSpace(config.SecretFile))
	if !filepath.IsAbs(secretFile) || filepath.Clean(secretFile) != secretFile {
		return adapterIngressConfig{}, exit(2, "adapter ingress secretFile must be absolute and clean")
	}
	denyPaths, err := validateAdapterIngressRoutes(config.DenyPaths, false)
	if err != nil {
		return adapterIngressConfig{}, err
	}
	denyPrefixes, err := validateAdapterIngressRoutes(config.DenyPrefixes, true)
	if err != nil {
		return adapterIngressConfig{}, err
	}
	stripPrefixes, err := validateAdapterIngressHeaderPrefixes(config.StripHeaderPrefixes)
	if err != nil {
		return adapterIngressConfig{}, err
	}
	config.Listen = listen
	config.Upstream = upstream.String()
	config.PublicOrigin = publicOrigin
	config.publicOriginURL, _ = url.Parse(publicOrigin)
	config.IdentityHeader = identityHeader
	config.Identity = identity
	config.SecretHeader = secretHeader
	config.SecretFile = secretFile
	config.DenyPaths = denyPaths
	config.DenyPrefixes = denyPrefixes
	config.StripHeaderPrefixes = stripPrefixes
	config.upstreamURL = upstream
	return config, nil
}

func adapterIngressEndpointsOverlap(listen string, upstream *url.URL) bool {
	listenHost, listenPort, listenErr := net.SplitHostPort(listen)
	if listenErr != nil || upstream == nil || upstream.Port() != listenPort {
		return false
	}
	listenAddress, listenErr := netip.ParseAddr(strings.Trim(listenHost, "[]"))
	upstreamAddress, upstreamErr := netip.ParseAddr(strings.Trim(upstream.Hostname(), "[]"))
	if listenErr != nil || upstreamErr != nil {
		return false
	}
	listenAddress = listenAddress.WithZone("").Unmap()
	upstreamAddress = upstreamAddress.Unmap()
	return listenAddress.IsUnspecified() || listenAddress == upstreamAddress
}

func validateAdapterIngressListen(value string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil || port == "0" {
		return "", exit(2, "adapter ingress listen must be an IP address with a nonzero port")
	}
	address, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return "", exit(2, "adapter ingress listen host must be a literal IP address")
	}
	parsedPort, err := net.LookupPort("tcp", port)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return "", exit(2, "adapter ingress listen port must be from 1 through 65535")
	}
	return net.JoinHostPort(address.String(), fmt.Sprint(parsedPort)), nil
}

func validateAdapterIngressUpstream(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, exit(2, "adapter ingress upstream must be one exact loopback HTTP origin")
	}
	host := strings.Trim(parsed.Hostname(), "[]")
	address, err := netip.ParseAddr(host)
	if err != nil || !address.Unmap().IsLoopback() || address.Zone() != "" || parsed.Port() == "" {
		return nil, exit(2, "adapter ingress upstream must be one exact loopback HTTP origin")
	}
	port, err := net.LookupPort("tcp", parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, exit(2, "adapter ingress upstream must contain a valid nonzero port")
	}
	parsed.Host = net.JoinHostPort(address.Unmap().String(), fmt.Sprint(port))
	parsed.Path = ""
	return parsed, nil
}

func validateAdapterIngressPublicOrigin(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > 2048 || !adapterIngressVisibleASCII(value) {
		return "", exit(2, "adapter ingress publicOrigin must be one exact non-loopback HTTPS origin")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || parsed.Hostname() == "" {
		return "", exit(2, "adapter ingress publicOrigin must be one exact non-loopback HTTPS origin")
	}
	host := strings.Trim(parsed.Hostname(), "[]")
	if strings.HasSuffix(host, ".") || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return "", exit(2, "adapter ingress publicOrigin must be one exact non-loopback HTTPS origin")
	}
	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		if address.Unmap().IsLoopback() || address.Zone() != "" || address.Is4In6() || host != address.String() {
			return "", exit(2, "adapter ingress publicOrigin must be one exact non-loopback HTTPS origin")
		}
	} else if !validAdapterIngressDNSName(host) || looksLikeLegacyIPv4Host(host) {
		return "", exit(2, "adapter ingress publicOrigin must contain a valid lowercase DNS hostname or IP address")
	}
	port, explicitPort, err := adapterIngressOriginPort(parsed)
	if err != nil || (explicitPort && port == 443) {
		return "", exit(2, "adapter ingress publicOrigin must contain a canonical valid HTTPS port")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	if value != origin {
		return "", exit(2, "adapter ingress publicOrigin must be one exact non-loopback HTTPS origin")
	}
	return origin, nil
}

func looksLikeLegacyIPv4Host(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		candidate := part
		base := 10
		if strings.HasPrefix(candidate, "0x") {
			candidate = strings.TrimPrefix(candidate, "0x")
			base = 16
		}
		if candidate == "" {
			return false
		}
		for index := range len(candidate) {
			character := candidate[index]
			if base == 10 && (character < '0' || character > '9') {
				return false
			}
			if base == 16 && !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
				return false
			}
		}
	}
	return true
}

func adapterIngressOriginPort(origin *url.URL) (int, bool, error) {
	hostname := origin.Hostname()
	canonicalHost := hostname
	if strings.Contains(hostname, ":") {
		canonicalHost = "[" + hostname + "]"
	}
	remainder := strings.TrimPrefix(origin.Host, canonicalHost)
	if remainder == "" {
		return 443, false, nil
	}
	if !strings.HasPrefix(remainder, ":") || len(remainder) == 1 {
		return 0, true, errors.New("invalid origin port")
	}
	value := strings.TrimPrefix(remainder, ":")
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != value {
		return 0, true, errors.New("invalid origin port")
	}
	return port, true, nil
}

func validAdapterIngressDNSName(value string) bool {
	if value == "" || value != strings.ToLower(value) || len(value) > 253 {
		return false
	}
	for label := range strings.SplitSeq(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := range len(label) {
			character := label[index]
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func adapterIngressVisibleASCII(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func validateAdapterIngressHeader(value, label string) (string, error) {
	value = strings.TrimSpace(value)
	if !validHTTPToken(value) || strings.Contains(value, "_") {
		return "", exit(2, "adapter ingress %s must be one valid HTTP header name", label)
	}
	return http.CanonicalHeaderKey(value), nil
}

func adapterIngressReservedAuthHeader(name string) bool {
	if strings.HasPrefix(name, "x-forwarded-") || strings.HasPrefix(name, "sec-websocket-") || isHopByHopHeader(name) {
		return true
	}
	switch name {
	case "authorization", "content-length", "cookie", "expect", "forwarded", "host", "origin":
		return true
	default:
		return false
	}
}

func validAdapterIngressHeaderValue(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x20 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') {
			continue
		}
		switch character {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validateAdapterIngressRoutes(values []string, prefix bool) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value, err := url.PathUnescape(value)
		if err != nil || value == "" || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?#\r\n") || strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
			return nil, exit(2, "adapter ingress denied routes must be absolute URL paths")
		}
		trailingSlash := strings.HasSuffix(value, "/")
		value = pathpkg.Clean(value)
		if prefix && trailingSlash && value != "/" {
			value += "/"
		}
		if prefix && value == "/" {
			return nil, exit(2, "adapter ingress denyPrefixes must not deny every route")
		}
		if _, ok := seen[value]; ok {
			return nil, exit(2, "adapter ingress denied routes must not contain duplicates")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func validateAdapterIngressHeaderPrefixes(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || value != strings.ToLower(value) || strings.Contains(value, "_") || !strings.HasSuffix(value, "-") || !validHTTPToken(value+"x") {
			return nil, exit(2, "adapter ingress stripHeaderPrefixes must be lowercase HTTP header prefixes ending in a dash")
		}
		if _, ok := seen[value]; ok {
			return nil, exit(2, "adapter ingress stripHeaderPrefixes must not contain duplicates")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func newAdapterIngressProxy(config adapterIngressConfig) (*adapterIngressProxy, error) {
	if config.upstreamURL == nil || config.publicOriginURL == nil {
		validated, err := validateAdapterIngressConfig(config)
		if err != nil {
			return nil, err
		}
		config = validated
	}
	secret, err := readAdapterIngressSecret(config.SecretFile)
	if err != nil {
		return nil, err
	}
	if len(secret) > 1024 || !adapterIngressVisibleASCII(secret) {
		return nil, exit(2, "adapter ingress secret file must contain one bounded visible ASCII token")
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      false,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    32,
		IdleConnTimeout:        90 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		MaxResponseHeaderBytes: adapterIngressMaxHeaderBytes,
	}
	proxy := &adapterIngressProxy{config: config, secret: secret}
	proxy.health = &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	proxy.reverse = &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(config.upstreamURL)
			request.Out.URL.RawQuery = request.In.URL.RawQuery
			request.Out.Host = config.upstreamURL.Host
			stripAdapterIngressHeaders(request.Out.Header, config)
			request.Out.Header.Set(config.IdentityHeader, config.Identity)
			request.Out.Header.Set(config.SecretHeader, secret)
			request.Out.Header.Set("X-Forwarded-Host", config.publicOriginURL.Host)
			request.Out.Header.Set("X-Forwarded-Proto", config.publicOriginURL.Scheme)
			if validAdapterIngressWebSocket(request.In) {
				request.Out.Header.Set("Connection", "Upgrade")
				request.Out.Header.Set("Upgrade", "websocket")
			}
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			writeAdapterIngressError(response, http.StatusBadGateway, "bad gateway")
		},
	}
	return proxy, nil
}

func readAdapterIngressSecret(path string) (string, error) {
	data, err := readPrivateAdapterFile(path, 8<<10, "adapter ingress secret")
	if err != nil {
		return "", err
	}
	defer clear(data)
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", exit(2, "adapter ingress secret file is empty")
	}
	if strings.ContainsAny(secret, "\r\n") {
		return "", exit(2, "adapter ingress secret file must contain one token")
	}
	return secret, nil
}

func (proxy *adapterIngressProxy) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if adapterIngressHeaderCount(request.Header) > adapterIngressMaxHeaders {
		writeAdapterIngressError(response, http.StatusRequestHeaderFieldsTooLarge, "request headers too large")
		return
	}
	if request.Method == http.MethodConnect || request.URL == nil || request.URL.IsAbs() || !strings.HasPrefix(request.RequestURI, "/") {
		writeAdapterIngressError(response, http.StatusBadRequest, "bad request")
		return
	}
	if adapterIngressHasUnderscoreHeader(request.Header) {
		writeAdapterIngressError(response, http.StatusBadRequest, "bad request")
		return
	}
	if strings.EqualFold(request.Method, http.MethodTrace) || strings.EqualFold(request.Method, "TRACK") {
		writeAdapterIngressError(response, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if request.URL.Path == "/healthz" {
		proxy.serveHealth(response, request)
		return
	}
	if proxy.denied(request.URL.Path) {
		writeAdapterIngressError(response, http.StatusNotFound, "not found")
		return
	}
	if !proxy.authenticated(request) {
		writeAdapterIngressError(response, http.StatusUnauthorized, "unauthorized")
		return
	}
	websocket := validAdapterIngressWebSocket(request)
	if adapterIngressHasUpgrade(request) && !websocket {
		writeAdapterIngressError(response, http.StatusBadRequest, "bad websocket upgrade")
		return
	}
	if !adapterIngressOriginAllowed(request, proxy.config.PublicOrigin, websocket) {
		writeAdapterIngressError(response, http.StatusForbidden, "forbidden")
		return
	}
	proxy.reverse.ServeHTTP(response, request)
}

func adapterIngressHasUnderscoreHeader(header http.Header) bool {
	for name := range header {
		if strings.Contains(name, "_") {
			return true
		}
	}
	return false
}

func (proxy *adapterIngressProxy) serveHealth(response http.ResponseWriter, request *http.Request) {
	if !adapterIngressLoopbackRemote(request.RemoteAddr) {
		writeAdapterIngressError(response, http.StatusNotFound, "not found")
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writeAdapterIngressError(response, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	healthURL := *proxy.config.upstreamURL
	healthURL.Path = "/healthz"
	upstreamRequest, err := http.NewRequestWithContext(request.Context(), request.Method, healthURL.String(), nil)
	if err != nil {
		writeAdapterIngressError(response, http.StatusBadGateway, "bad gateway")
		return
	}
	upstreamResponse, err := proxy.health.Do(upstreamRequest)
	if err != nil {
		writeAdapterIngressError(response, http.StatusBadGateway, "bad gateway")
		return
	}
	defer upstreamResponse.Body.Close()
	nominated := adapterIngressConnectionHeaders(upstreamResponse.Header)
	for name, values := range upstreamResponse.Header {
		lower := strings.ToLower(name)
		if isHopByHopHeader(lower) || nominated[lower] {
			continue
		}
		for _, value := range values {
			response.Header().Add(name, value)
		}
	}
	response.WriteHeader(upstreamResponse.StatusCode)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(response, upstreamResponse.Body)
	}
}

func (proxy *adapterIngressProxy) denied(requestPath string) bool {
	paths := []string{requestPath}
	if clean := pathpkg.Clean(requestPath); clean != requestPath {
		paths = append(paths, clean)
	}
	for _, candidate := range paths {
		for _, denied := range proxy.config.DenyPaths {
			if candidate == denied {
				return true
			}
		}
		for _, denied := range proxy.config.DenyPrefixes {
			if strings.HasPrefix(candidate, denied) {
				return true
			}
		}
	}
	return false
}

func (proxy *adapterIngressProxy) authenticated(request *http.Request) bool {
	identity, identityOK := exactAdapterIngressHeader(request.Header, proxy.config.IdentityHeader)
	secret, secretOK := exactAdapterIngressHeader(request.Header, proxy.config.SecretHeader)
	if !identityOK || !secretOK || identity != proxy.config.Identity || len(secret) != len(proxy.secret) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(proxy.secret)) == 1
}

func exactAdapterIngressHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, len(values) == 1
}

func adapterIngressHeaderCount(header http.Header) int {
	count := 0
	for _, values := range header {
		count += len(values)
	}
	return count
}

func adapterIngressOriginAllowed(request *http.Request, publicOrigin string, required bool) bool {
	origins := request.Header.Values("Origin")
	if len(origins) == 0 {
		return !required
	}
	return len(origins) == 1 && origins[0] == publicOrigin
}

func adapterIngressHasUpgrade(request *http.Request) bool {
	return len(request.Header.Values("Upgrade")) > 0 || (len(request.Header.Values("Connection")) > 0 && headerHasToken(request.Header, "Connection", "upgrade"))
}

func validAdapterIngressWebSocket(request *http.Request) bool {
	upgrades := request.Header.Values("Upgrade")
	return request.Method == http.MethodGet && len(upgrades) == 1 && strings.EqualFold(upgrades[0], "websocket") && headerHasToken(request.Header, "Connection", "upgrade")
}

func headerHasToken(header http.Header, name, wanted string) bool {
	for _, value := range header.Values(name) {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), wanted) {
				return true
			}
		}
	}
	return false
}

func adapterIngressConnectionHeaders(header http.Header) map[string]bool {
	result := make(map[string]bool)
	for _, value := range header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			name := strings.ToLower(strings.TrimSpace(token))
			if validHTTPToken(name) {
				result[name] = true
			}
		}
	}
	return result
}

func adapterIngressLoopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	address, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && address.Unmap().IsLoopback()
}

func stripAdapterIngressHeaders(header http.Header, config adapterIngressConfig) {
	for name := range header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "cookie" || lower == "x-authenticated-user" || lower == strings.ToLower(config.IdentityHeader) || lower == strings.ToLower(config.SecretHeader) || strings.HasPrefix(lower, "x-forwarded-") || isHopByHopHeader(lower) {
			header.Del(name)
			continue
		}
		for _, prefix := range config.StripHeaderPrefixes {
			if strings.HasPrefix(lower, prefix) {
				header.Del(name)
				break
			}
		}
	}
}

func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "proxy-connection", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func writeAdapterIngressError(response http.ResponseWriter, status int, message string) {
	body := fmt.Sprintf("{\"error\":%q}\n", message)
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Connection", "close")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, body)
}
