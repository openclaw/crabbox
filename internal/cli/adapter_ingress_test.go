//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

const (
	adapterIngressTestIdentity = "operator@example.test"
	adapterIngressTestSecret   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	adapterIngressTestOrigin   = "https://fleet.example.test"
)

func TestAdapterIngressForwardsAuthenticatedHTTPAndReplacesPrivateHeaders(t *testing.T) {
	type observedRequest struct {
		method string
		path   string
		body   string
		header http.Header
		host   string
	}
	observed := make(chan observedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		observed <- observedRequest{request.Method, request.URL.RequestURI(), string(body), request.Header.Clone(), request.Host}
		response.Header().Set("Connection", "X-Upstream-Private")
		response.Header().Set("X-Upstream-Private", "drop")
		response.Header().Set("X-Public", "keep")
		response.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(response, "created")
	}))
	defer upstream.Close()
	proxy := newAdapterIngressTestProxy(t, upstream.URL, nil)

	request := httptest.NewRequest(http.MethodPost, "/api/workspaces?token=a;b&wait=1", strings.NewReader("payload"))
	request.RemoteAddr = "203.0.113.10:4242"
	setAdapterIngressTestAuth(request.Header)
	request.Header.Set("Origin", adapterIngressTestOrigin)
	request.Header.Set("Authorization", "Bearer browser-secret")
	request.Header.Set("Cookie", "session=browser-secret")
	request.Header.Set("X-Authenticated-User", "spoofed@example.test")
	request.Header.Set("X-Forwarded-For", "198.51.100.10")
	request.Header.Set("X-Crabbox-Claim", "spoofed")
	request.Header.Set("X-Crabfleet-Claim", "spoofed")
	request.Header.Set("X-Deployment-Claim", "spoofed")
	request.Header.Set("Connection", "X-Remove-Me")
	request.Header.Set("X-Remove-Me", "spoofed")
	request.Header.Set("X-Public-Request", "keep")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || response.Body.String() != "created" {
		t.Fatalf("response code=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Public") != "keep" || response.Header().Get("X-Upstream-Private") != "" {
		t.Fatalf("response headers=%v", response.Header())
	}
	got := <-observed
	if got.method != http.MethodPost || got.path != "/api/workspaces?token=a;b&wait=1" || got.body != "payload" {
		t.Fatalf("upstream request=%#v", got)
	}
	if got.host != strings.TrimPrefix(upstream.URL, "http://") {
		t.Fatalf("upstream host=%q", got.host)
	}
	if got.header.Get("X-Test-User") != adapterIngressTestIdentity || got.header.Get("X-Test-Secret") != adapterIngressTestSecret {
		t.Fatalf("trusted headers user=%q secret=%q", got.header.Get("X-Test-User"), got.header.Get("X-Test-Secret"))
	}
	if got.header.Get("X-Forwarded-Host") != "fleet.example.test" || got.header.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("sanitized forwarding headers=%v", got.header)
	}
	for _, name := range []string{"Authorization", "Cookie", "X-Authenticated-User", "X-Forwarded-For", "X-Crabbox-Claim", "X-Crabfleet-Claim", "X-Deployment-Claim", "X-Remove-Me"} {
		if value := got.header.Get(name); value != "" {
			t.Errorf("private header %s reached upstream as %q", name, value)
		}
	}
	if got.header.Get("X-Public-Request") != "keep" {
		t.Fatalf("ordinary request header missing: %v", got.header)
	}
}

func TestAdapterIngressRejectsAuthenticationOriginRoutesAndHeaderFloods(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	proxy := newAdapterIngressTestProxy(t, upstream.URL, func(config *adapterIngressConfig) {
		config.DenyPaths = []string{"/login/provider", "/api/private", "/escaped/%70ath"}
		config.DenyPrefixes = []string{"/api/private/"}
	})

	tests := []struct {
		name   string
		path   string
		mutate func(*http.Request)
		want   int
	}{
		{name: "missing auth", path: "/app", want: http.StatusUnauthorized},
		{name: "wrong secret", path: "/app", mutate: func(request *http.Request) {
			setAdapterIngressTestAuth(request.Header)
			request.Header.Set("X-Test-Secret", "wrong")
		}, want: http.StatusUnauthorized},
		{name: "duplicate identity", path: "/app", mutate: func(request *http.Request) {
			setAdapterIngressTestAuth(request.Header)
			request.Header["X-Test-User"] = []string{adapterIngressTestIdentity, adapterIngressTestIdentity}
		}, want: http.StatusUnauthorized},
		{name: "wrong origin", path: "/app", mutate: func(request *http.Request) {
			setAdapterIngressTestAuth(request.Header)
			request.Header.Set("Origin", "https://other.example.test")
		}, want: http.StatusForbidden},
		{name: "exact denied route", path: "/login/provider", want: http.StatusNotFound},
		{name: "prefixed denied route", path: "/api/private/child", want: http.StatusNotFound},
		{name: "encoded denied route", path: "/api/private%2fchild", want: http.StatusNotFound},
		{name: "encoded configured route", path: "/escaped/path", want: http.StatusNotFound},
		{name: "dot segment denied route", path: "/public/../api/private", want: http.StatusNotFound},
		{name: "header flood", path: "/app", mutate: func(request *http.Request) {
			setAdapterIngressTestAuth(request.Header)
			for index := 0; index < adapterIngressMaxHeaders; index++ {
				request.Header.Add("X-Many", "value")
			}
		}, want: http.StatusRequestHeaderFieldsTooLarge},
		{name: "underscore header alias", path: "/app", mutate: func(request *http.Request) {
			setAdapterIngressTestAuth(request.Header)
			request.Header.Set("X_Forwarded_Host", "spoofed.example.test")
		}, want: http.StatusBadRequest},
		{name: "malformed upgrade", path: "/app", mutate: func(request *http.Request) {
			setAdapterIngressTestAuth(request.Header)
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "h2c")
		}, want: http.StatusBadRequest},
		{name: "mixed-case trace", path: "/app", mutate: func(request *http.Request) {
			request.Method = "Trace"
			setAdapterIngressTestAuth(request.Header)
		}, want: http.StatusMethodNotAllowed},
		{name: "track", path: "/app", mutate: func(request *http.Request) {
			request.Method = "TRACK"
			setAdapterIngressTestAuth(request.Header)
		}, want: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.RemoteAddr = "203.0.113.10:4242"
			if test.mutate != nil {
				test.mutate(request)
			}
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("code=%d body=%q want=%d", response.Code, response.Body.String(), test.want)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("rejected requests reached upstream %d times", calls.Load())
	}
}

func TestAdapterIngressHealthIsLoopbackOnlyAndStripsConnectionHeaders(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/healthz" || request.Header.Get("X-Test-Secret") != "" {
			t.Errorf("health request path=%q headers=%v", request.URL.Path, request.Header)
		}
		response.Header().Set("Connection", "X-Internal")
		response.Header().Set("X-Internal", "drop")
		response.Header().Set("X-Health", "ok")
		_, _ = io.WriteString(response, "healthy")
	}))
	defer upstream.Close()
	proxy := newAdapterIngressTestProxy(t, upstream.URL, nil)

	remote := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	remote.RemoteAddr = "203.0.113.10:4242"
	remoteResponse := httptest.NewRecorder()
	proxy.ServeHTTP(remoteResponse, remote)
	if remoteResponse.Code != http.StatusNotFound || calls.Load() != 0 {
		t.Fatalf("remote health code=%d calls=%d", remoteResponse.Code, calls.Load())
	}

	local := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	local.RemoteAddr = "127.0.0.1:4242"
	localResponse := httptest.NewRecorder()
	proxy.ServeHTTP(localResponse, local)
	if localResponse.Code != http.StatusOK || localResponse.Body.String() != "healthy" || localResponse.Header().Get("X-Health") != "ok" {
		t.Fatalf("local health code=%d body=%q headers=%v", localResponse.Code, localResponse.Body.String(), localResponse.Header())
	}
	if localResponse.Header().Get("Connection") != "" || localResponse.Header().Get("X-Internal") != "" {
		t.Fatalf("hop headers leaked: %v", localResponse.Header())
	}
}

func TestAdapterIngressHealthDoesNotFollowRedirects(t *testing.T) {
	var redirectedCalls atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedCalls.Add(1)
	}))
	defer redirected.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Redirect(response, &http.Request{}, redirected.URL, http.StatusFound)
	}))
	defer upstream.Close()
	proxy := newAdapterIngressTestProxy(t, upstream.URL, nil)

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.RemoteAddr = "127.0.0.1:4242"
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusFound || redirectedCalls.Load() != 0 {
		t.Fatalf("health redirect code=%d followed=%d", response.Code, redirectedCalls.Load())
	}
}

func TestAdapterIngressTunnelsAuthenticatedWebSockets(t *testing.T) {
	upstreamHeaders := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "")
		upstreamHeaders <- request.Header.Clone()
		messageType, message, err := connection.Read(request.Context())
		if err == nil {
			_ = connection.Write(request.Context(), messageType, message)
		}
	}))
	defer upstream.Close()
	proxy := newAdapterIngressTestProxy(t, upstream.URL, nil)
	ingress := httptest.NewServer(proxy)
	defer ingress.Close()

	header := make(http.Header)
	setAdapterIngressTestAuth(header)
	header.Set("Origin", adapterIngressTestOrigin)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ingress.URL, "http")+"/terminal", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	if err := connection.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	messageType, message, err := connection.Read(ctx)
	if err != nil || messageType != websocket.MessageText || string(message) != "hello" {
		t.Fatalf("echo type=%v message=%q err=%v", messageType, message, err)
	}
	got := <-upstreamHeaders
	if got.Get("X-Test-User") != adapterIngressTestIdentity || got.Get("X-Test-Secret") != adapterIngressTestSecret {
		t.Fatalf("trusted websocket headers=%v", got)
	}
}

func TestAdapterIngressConfigurationRequiresPrivateStrictFiles(t *testing.T) {
	directory := t.TempDir()
	secretPath := filepath.Join(directory, "secret")
	if err := os.WriteFile(secretPath, []byte(adapterIngressTestSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := adapterIngressConfig{
		Listen:         "0.0.0.0:8443",
		Upstream:       "http://127.0.0.1:8787",
		PublicOrigin:   adapterIngressTestOrigin,
		IdentityHeader: "X-Test-User",
		Identity:       adapterIngressTestIdentity,
		SecretHeader:   "X-Test-Secret",
		SecretFile:     secretPath,
	}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "ingress.json")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAdapterIngressConfig(configPath)
	if err != nil || loaded.upstreamURL == nil {
		t.Fatalf("load valid config=%#v err=%v", loaded, err)
	}
	if _, err := newAdapterIngressProxy(loaded); err != nil {
		t.Fatalf("load valid secret: %v", err)
	}

	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAdapterIngressConfig(configPath); err == nil {
		t.Fatal("broad config permissions accepted")
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		t.Fatal(err)
	}
	unknown := append(bytes.TrimSuffix(data, []byte("}")), []byte(`,"unknown":true}`)...)
	if err := os.WriteFile(configPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAdapterIngressConfig(configPath); err == nil {
		t.Fatal("unknown config key accepted")
	}
	if err := os.Chmod(secretPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newAdapterIngressProxy(valid); err == nil {
		t.Fatal("broad secret permissions accepted")
	}
}

func TestValidateAdapterIngressConfigRejectsUnsafeEndpointsAndHeaders(t *testing.T) {
	valid := adapterIngressConfig{
		Listen:         "0.0.0.0:8443",
		Upstream:       "http://127.0.0.1:8787",
		PublicOrigin:   adapterIngressTestOrigin,
		IdentityHeader: "X-Test-User",
		Identity:       adapterIngressTestIdentity,
		SecretHeader:   "X-Test-Secret",
		SecretFile:     "/tmp/adapter-ingress-secret",
	}
	tests := map[string]func(*adapterIngressConfig){
		"DNS listen":         func(config *adapterIngressConfig) { config.Listen = "localhost:8443" },
		"recursive wildcard": func(config *adapterIngressConfig) { config.Listen = "0.0.0.0:8787" },
		"recursive exact":    func(config *adapterIngressConfig) { config.Listen = "127.0.0.1:8787" },
		"recursive IPv6 wildcard": func(config *adapterIngressConfig) {
			config.Listen = "[::]:8787"
			config.Upstream = "http://[::1]:8787"
		},
		"recursive IPv6 zone": func(config *adapterIngressConfig) {
			config.Listen = "[::1%0]:8787"
			config.Upstream = "http://[::1]:8787"
		},
		"non-loopback upstream":   func(config *adapterIngressConfig) { config.Upstream = "http://192.0.2.10:8787" },
		"loopback public origin":  func(config *adapterIngressConfig) { config.PublicOrigin = "https://127.0.0.1" },
		"legacy IPv4 origin":      func(config *adapterIngressConfig) { config.PublicOrigin = "https://127.1" },
		"expanded IPv6 origin":    func(config *adapterIngressConfig) { config.PublicOrigin = "https://[2001:0db8:0:0:0:0:0:1]" },
		"uppercase IPv6 origin":   func(config *adapterIngressConfig) { config.PublicOrigin = "https://[2001:DB8::1]" },
		"uppercase public origin": func(config *adapterIngressConfig) { config.PublicOrigin = "https://FLEET.example.test" },
		"empty origin port":       func(config *adapterIngressConfig) { config.PublicOrigin = "https://fleet.example.test:" },
		"default origin port":     func(config *adapterIngressConfig) { config.PublicOrigin = "https://fleet.example.test:443" },
		"large origin port":       func(config *adapterIngressConfig) { config.PublicOrigin = "https://fleet.example.test:65536" },
		"padded origin port":      func(config *adapterIngressConfig) { config.PublicOrigin = "https://fleet.example.test:08443" },
		"reserved auth header":    func(config *adapterIngressConfig) { config.IdentityHeader = "Authorization" },
		"underscored auth header": func(config *adapterIngressConfig) { config.IdentityHeader = "X_Test_User" },
		"same auth headers":       func(config *adapterIngressConfig) { config.SecretHeader = "x-test-user" },
		"newline identity":        func(config *adapterIngressConfig) { config.Identity = "operator\nspoofed" },
		"relative secret":         func(config *adapterIngressConfig) { config.SecretFile = "secret" },
		"deny all prefix":         func(config *adapterIngressConfig) { config.DenyPrefixes = []string{"/"} },
		"uppercase strip prefix":  func(config *adapterIngressConfig) { config.StripHeaderPrefixes = []string{"X-Private-"} },
		"underscored strip prefix": func(config *adapterIngressConfig) {
			config.StripHeaderPrefixes = []string{"x_private-"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := validateAdapterIngressConfig(config); err == nil {
				t.Fatalf("unsafe config accepted: %#v", config)
			}
		})
	}
	valid.PublicOrigin = "https://fleet.example.test:8443"
	if _, err := validateAdapterIngressConfig(valid); err != nil {
		t.Fatalf("canonical non-default origin port rejected: %v", err)
	}
	valid.PublicOrigin = "https://[2001:db8::1]"
	if _, err := validateAdapterIngressConfig(valid); err != nil {
		t.Fatalf("canonical IPv6 origin rejected: %v", err)
	}
}

func TestAdapterIngressPrivateFileStabilityDetectsSameSizeRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !adapterIngressPrivateFileStable(before, before, before.Size()) {
		t.Fatal("unchanged file reported unstable")
	}
	if err := os.WriteFile(path, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	changedTime := before.ModTime().Add(time.Second)
	if err := os.Chtimes(path, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if adapterIngressPrivateFileStable(before, after, before.Size()) {
		t.Fatal("same-size rewrite reported stable")
	}
}

func TestAdapterIngressCommandIsDiscoverable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"adapter", "--help"})
	if err != nil || !strings.Contains(stdout.String()+stderr.String(), "ingress") {
		t.Fatalf("help err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func newAdapterIngressTestProxy(t *testing.T, upstream string, mutate func(*adapterIngressConfig)) *adapterIngressProxy {
	t.Helper()
	secretPath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretPath, []byte(adapterIngressTestSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := adapterIngressConfig{
		Listen:              "0.0.0.0:8443",
		Upstream:            upstream,
		PublicOrigin:        adapterIngressTestOrigin,
		IdentityHeader:      "X-Test-User",
		Identity:            adapterIngressTestIdentity,
		SecretHeader:        "X-Test-Secret",
		SecretFile:          secretPath,
		StripHeaderPrefixes: []string{"x-crabbox-", "x-crabfleet-", "x-deployment-"},
	}
	if mutate != nil {
		mutate(&config)
	}
	proxy, err := newAdapterIngressProxy(config)
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}

func setAdapterIngressTestAuth(header http.Header) {
	header.Set("X-Test-User", adapterIngressTestIdentity)
	header.Set("X-Test-Secret", adapterIngressTestSecret)
}
