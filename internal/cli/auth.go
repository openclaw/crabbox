package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (a App) login(ctx context.Context, args []string) error {
	fs := newFlagSet("login", a.Stderr)
	brokerURL := fs.String("url", "", "broker URL")
	provider := fs.String("provider", "", "provider for managed or registered leases")
	tokenStdin := fs.Bool("token-stdin", false, "read broker token from stdin")
	noBrowser := fs.Bool("no-browser", false, "print GitHub login URL instead of opening a browser")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	urlWasExplicit := strings.TrimSpace(*brokerURL) != ""
	cfg, err := loadConfigWithOverrides(*brokerURL, *provider)
	if err != nil {
		return err
	}
	brokerMode := cfg.BrokerMode
	if *brokerURL == "" {
		*brokerURL = cfg.Coordinator
	}
	if *provider == "" {
		if cfg.brokerProvider != "" {
			candidate, candidateErr := validateBrokerProviderForMode(cfg.brokerProvider, string(brokerMode))
			if candidateErr != nil {
				return candidateErr
			}
			if !urlWasExplicit {
				*provider = candidate
			}
		} else if !urlWasExplicit {
			if candidate, candidateErr := validateBrokerProviderForMode(cfg.Provider, string(brokerMode)); candidateErr == nil {
				*provider = candidate
			}
		}
	}
	if *brokerURL == "" {
		return exit(2, "crabbox login requires --url <broker-url> or a configured broker URL")
	}
	if *tokenStdin {
		return a.loginWithToken(ctx, *brokerURL, *provider, brokerMode, *jsonOut)
	}
	return a.loginWithGitHub(ctx, *brokerURL, *provider, brokerMode, *noBrowser, *jsonOut)
}

func (a App) loginWithToken(ctx context.Context, brokerURL, provider string, brokerMode BrokerMode, jsonOut bool) error {
	data, err := io.ReadAll(a.input())
	if err != nil {
		return exit(2, "read broker token: %v", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return exit(2, "broker token from stdin is empty")
	}
	path, cfg, err := writeBrokerLogin(brokerURL, token, provider, brokerMode)
	if err != nil {
		return err
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !ok {
		return exit(2, "login wrote config but broker is not configured")
	}
	return a.finishLogin(ctx, coord, path, cfg, jsonOut)
}

func (a App) loginWithGitHub(ctx context.Context, brokerURL, provider string, brokerMode BrokerMode, noBrowser, jsonOut bool) error {
	loopback, err := startGitHubLoginLoopback()
	if err != nil {
		return exit(3, "start local GitHub login callback: %v", err)
	}
	defer loopback.Close()

	pollSecret, err := randomHex(32)
	if err != nil {
		return err
	}
	pollSecretHash := sha256Hex(pollSecret)
	client, cfg, err := coordinatorClientConfigForLogin(brokerURL, provider)
	if err != nil {
		return err
	}
	start, err := client.StartGitHubLogin(ctx, pollSecretHash, provider, loopback.URL())
	if err != nil {
		return err
	}
	if err := validateGitHubLoginURL(start.URL); err != nil {
		return exit(3, "GitHub login returned an invalid authorization URL: %v", err)
	}
	if canonicalBrokerURL, ok := canonicalBrokerURLFromLoginURL(start.URL); ok && !sameBrokerURL(brokerURL, canonicalBrokerURL) {
		if !brokerLoginRedirectOriginAllowed(cfg, canonicalBrokerURL) {
			return exit(
				3,
				"GitHub login redirect_uri broker origin %s does not match selected broker %s; add it to broker.loginRedirectOrigins only if this is an intended same-deployment alias",
				canonicalBrokerURL,
				normalizedBrokerURL(brokerURL),
			)
		}
		brokerURL = canonicalBrokerURL
		client, cfg, err = coordinatorClientConfigForLogin(brokerURL, provider)
		if err != nil {
			return err
		}
		start, err = client.StartGitHubLogin(ctx, pollSecretHash, provider, loopback.URL())
		if err != nil {
			return err
		}
		if err := validateGitHubLoginURL(start.URL); err != nil {
			return exit(3, "GitHub login returned an invalid authorization URL: %v", err)
		}
		if nextBrokerURL, ok := canonicalBrokerURLFromLoginURL(start.URL); ok && !sameBrokerURL(brokerURL, nextBrokerURL) {
			return exit(
				3,
				"GitHub login redirect_uri broker origin %s does not match approved broker %s; check CRABBOX_PUBLIC_URL",
				nextBrokerURL,
				normalizedBrokerURL(brokerURL),
			)
		}
	}
	if noBrowser {
		fmt.Fprintf(a.Stderr, "open this GitHub login URL in a browser on this device:\n%s\n", start.URL)
	} else if err := openLoginBrowser(start.URL, cfg); err != nil {
		fmt.Fprintf(a.Stderr, "could not open browser: %v\nopen this GitHub login URL in a browser on this device:\n%s\n", err, start.URL)
	} else {
		fmt.Fprintln(a.Stderr, "opened GitHub login in your browser")
	}
	deadline := time.Now().Add(10 * time.Minute)
	if start.ExpiresAt != "" {
		if parsed, err := time.Parse(time.RFC3339, start.ExpiresAt); err == nil {
			deadline = parsed
		}
	}
	browserConfirmation := ""
	for {
		if time.Now().After(deadline) {
			return exit(3, "GitHub login expired")
		}
		select {
		case browserConfirmation = <-loopback.confirmations:
		default:
		}
		poll, err := client.PollGitHubLogin(ctx, start.LoginID, pollSecret, browserConfirmation)
		if err != nil {
			return err
		}
		switch poll.Status {
		case "pending", "confirmation_required":
		case "complete":
			if browserConfirmation == "" {
				return exit(3, "GitHub login completed before this device received the browser confirmation")
			}
			if poll.Token == "" {
				return exit(3, "GitHub login completed without a broker token")
			}
			if provider == "" {
				provider = poll.Provider
			}
			path, cfg, err := writeBrokerLogin(brokerURL, poll.Token, provider, brokerMode)
			if err != nil {
				return err
			}
			coord, ok, err := newCoordinatorClient(cfg)
			if err != nil {
				return err
			}
			if !ok {
				return exit(2, "login wrote config but broker is not configured")
			}
			return a.finishLogin(ctx, coord, path, cfg, jsonOut)
		case "expired":
			return exit(3, "GitHub login expired")
		case "failed":
			return exit(3, "GitHub login failed: %s", blank(poll.Error, "unknown error"))
		default:
			return exit(3, "GitHub login returned unexpected status %q", poll.Status)
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type githubLoginLoopback struct {
	listener      net.Listener
	server        *http.Server
	path          string
	confirmations chan string
}

func startGitHubLoginLoopback() (*githubLoginLoopback, error) {
	pathToken, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	loopback := &githubLoginLoopback{
		listener:      listener,
		path:          "/crabbox/oauth/" + pathToken,
		confirmations: make(chan string, 1),
	}
	loopback.server = &http.Server{
		Handler:           http.HandlerFunc(loopback.handle),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
	go func() {
		_ = loopback.server.Serve(listener)
	}()
	return loopback, nil
}

func (l *githubLoginLoopback) URL() string {
	return "http://" + l.listener.Addr().String() + l.path
}

func (l *githubLoginLoopback) Close() {
	_ = l.server.Close()
}

func (l *githubLoginLoopback) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != l.path {
		http.NotFound(w, r)
		return
	}
	confirmation := r.URL.Query().Get("confirmation")
	if !validBrowserConfirmation(confirmation) {
		http.Error(w, "invalid login confirmation", http.StatusBadRequest)
		return
	}
	select {
	case l.confirmations <- confirmation:
	default:
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>Crabbox login complete</title></head><body><h1>Crabbox login complete</h1><p>You can close this tab and return to the terminal.</p></body></html>")
}

func validBrowserConfirmation(value string) bool {
	if !strings.HasPrefix(value, "confirm_") || len(value) != len("confirm_")+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "confirm_"))
	return err == nil
}

func (a App) finishLogin(ctx context.Context, coord *CoordinatorClient, path string, cfg Config, jsonOut bool) error {
	who, err := coord.Whoami(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		view := map[string]any{
			"config":   path,
			"broker":   redactedConfigURL(cfg.Coordinator),
			"provider": cfg.Provider,
			"identity": who,
		}
		if who.TokenExpiresAt != "" {
			view["tokenExpiresAt"] = who.TokenExpiresAt
		}
		return json.NewEncoder(a.Stdout).Encode(view)
	}
	expires := ""
	if who.TokenExpiresAt != "" {
		expires = " token_expires=" + who.TokenExpiresAt
	}
	fmt.Fprintf(a.Stdout, "logged in broker=%s provider=%s user=%s org=%s%s config=%s\n", redactedConfigURL(cfg.Coordinator), cfg.Provider, who.Owner, who.Org, expires, path)
	return nil
}

func coordinatorClientForLogin(brokerURL, provider string) (*CoordinatorClient, error) {
	coord, _, err := coordinatorClientConfigForLogin(brokerURL, provider)
	return coord, err
}

func coordinatorClientConfigForLogin(brokerURL, provider string) (*CoordinatorClient, Config, error) {
	cfg, err := loadConfigWithOverrides(brokerURL, provider)
	if err != nil {
		return nil, Config{}, err
	}
	cfg.CoordToken = ""
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, Config{}, err
	}
	if !ok {
		return nil, Config{}, exit(2, "login requires a broker URL")
	}
	return coord, cfg, nil
}

func validateGitHubLoginURL(loginURL string) error {
	u, err := url.Parse(loginURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme must be https")
	}
	if u.User != nil {
		return fmt.Errorf("user information is not allowed")
	}
	if !strings.EqualFold(u.Hostname(), "github.com") || u.Port() != "" {
		return fmt.Errorf("host must be github.com")
	}
	if u.EscapedPath() != "/login/oauth/authorize" {
		return fmt.Errorf("path must be /login/oauth/authorize")
	}
	if u.Fragment != "" {
		return fmt.Errorf("fragment is not allowed")
	}
	return nil
}

func canonicalBrokerURLFromLoginURL(loginURL string) (string, bool) {
	u, err := url.Parse(loginURL)
	if err != nil {
		return "", false
	}
	redirect := u.Query().Get("redirect_uri")
	if redirect == "" {
		return "", false
	}
	redirectURL, err := url.Parse(redirect)
	if err != nil || redirectURL.Scheme == "" || redirectURL.Host == "" {
		return "", false
	}
	const callbackPath = "/v1/auth/github/callback"
	cleanPath := strings.TrimRight(redirectURL.Path, "/")
	if !strings.HasSuffix(cleanPath, callbackPath) {
		return "", false
	}
	redirectURL.Path = strings.TrimRight(strings.TrimSuffix(cleanPath, callbackPath), "/")
	redirectURL.RawPath = ""
	redirectURL.RawQuery = ""
	redirectURL.Fragment = ""
	return strings.TrimRight(redirectURL.String(), "/"), true
}

func sameBrokerURL(left, right string) bool {
	return normalizedBrokerURL(left) == normalizedBrokerURL(right)
}

func brokerLoginRedirectOriginAllowed(cfg Config, brokerURL string) bool {
	for _, origin := range cfg.BrokerLoginRedirectOrigins {
		if sameBrokerURL(origin, brokerURL) {
			return true
		}
	}
	return false
}

func normalizedBrokerURL(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return strings.TrimRight(value, "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if hostname != "" {
		switch {
		case port != "":
			u.Host = net.JoinHostPort(hostname, port)
		case strings.Contains(hostname, ":"):
			u.Host = "[" + hostname + "]"
		default:
			u.Host = hostname
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func openLoginBrowser(target string, cfg Config) error {
	if err := ValidateProviderCredentialDestination(cfg); err != nil {
		return err
	}
	return openLocalURLWithEnvironment(target, externalDesktopChildEnvDenylist(cfg, cfg.TargetOS)...)
}

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (a App) logout(_ context.Context, args []string) error {
	fs := newFlagSet("logout", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	path := writableConfigPath()
	if path == "" {
		return exit(2, "user config directory is unavailable")
	}
	file, err := readFileConfig(path)
	if err != nil {
		return err
	}
	if file.Broker != nil {
		file.Broker.Token = ""
	}
	file.CoordinatorToken = ""
	written, err := writeUserFileConfig(file)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(map[string]any{"config": written, "brokerAuth": "missing"})
	}
	fmt.Fprintf(a.Stdout, "logged out config=%s broker_auth=missing\n", written)
	return nil
}

func (a App) whoami(ctx context.Context, args []string) error {
	fs := newFlagSet("whoami", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !ok {
		return exit(2, "whoami requires a configured coordinator")
	}
	who, err := coord.Whoami(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(who)
	}
	fmt.Fprintf(a.Stdout, "user=%s org=%s auth=%s broker=%s\n", who.Owner, who.Org, who.Auth, redactedConfigURL(cfg.Coordinator))
	return nil
}

func writeBrokerLogin(brokerURL, token, provider string, brokerMode BrokerMode) (string, Config, error) {
	path := writableConfigPath()
	if path == "" {
		return "", Config{}, exit(2, "user config directory is unavailable")
	}
	file, err := readFileConfig(path)
	if err != nil {
		return "", Config{}, err
	}
	if file.Broker == nil {
		file.Broker = &fileBrokerConfig{}
	}
	file.Broker.URL = brokerURL
	file.Broker.Token = token
	file.Broker.Mode = string(brokerMode)
	brokerProvider, err := validateBrokerProviderForMode(provider, string(brokerMode))
	if err != nil {
		return "", Config{}, err
	}
	if brokerProvider != "" {
		file.Broker.Provider = brokerProvider
		file.Provider = brokerProvider
	}
	written, err := writeUserFileConfig(file)
	if err != nil {
		return "", Config{}, err
	}
	cfg, err := loadConfig()
	if err != nil {
		return written, Config{}, err
	}
	// Verify the login against the broker and credential just written. Ambient
	// coordinator overrides still apply to later commands, but must not redirect
	// this one-shot verification away from an explicit login destination.
	cfg.Coordinator = brokerURL
	cfg.CoordToken = token
	cfg.CoordTokenCommand = nil
	cfg.BrokerMode = brokerMode
	cfg.Provider = brokerProvider
	cfg.brokerProvider = brokerProvider
	return written, cfg, nil
}
