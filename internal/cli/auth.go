package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const defaultCoordinatorURL = "https://crabbox.openclaw.ai"

func (a App) login(ctx context.Context, args []string) error {
	fs := newFlagSet("login", a.Stderr)
	brokerURL := fs.String("url", "", "broker URL")
	provider := fs.String("provider", "", "default brokered provider: hetzner, aws, azure, or gcp")
	tokenStdin := fs.Bool("token-stdin", false, "read broker token from stdin")
	noBrowser := fs.Bool("no-browser", false, "print GitHub login URL instead of opening a browser")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *brokerURL == "" {
		if cfg, err := loadConfig(); err == nil {
			*brokerURL = cfg.Coordinator
			if *provider == "" {
				*provider = cfg.Provider
			}
		}
	}
	if *brokerURL == "" {
		*brokerURL = defaultCoordinatorURL
	}
	if *tokenStdin {
		return a.loginWithToken(ctx, *brokerURL, *provider, *jsonOut)
	}
	return a.loginWithGitHub(ctx, *brokerURL, *provider, *noBrowser, *jsonOut)
}

func (a App) loginWithToken(ctx context.Context, brokerURL, provider string, jsonOut bool) error {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return exit(2, "read broker token: %v", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return exit(2, "broker token from stdin is empty")
	}
	path, cfg, err := writeBrokerLogin(brokerURL, token, provider)
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

func (a App) loginWithGitHub(ctx context.Context, brokerURL, provider string, noBrowser, jsonOut bool) error {
	pollSecret, err := randomHex(32)
	if err != nil {
		return err
	}
	pollSecretHash := sha256Hex(pollSecret)
	client, err := coordinatorClientForLogin(brokerURL)
	if err != nil {
		return err
	}
	start, err := client.StartGitHubLogin(ctx, pollSecretHash, provider)
	if err != nil {
		return err
	}
	if canonicalBrokerURL, ok := canonicalBrokerURLFromLoginURL(start.URL); ok && !sameBrokerURL(brokerURL, canonicalBrokerURL) {
		brokerURL = canonicalBrokerURL
		client, err = coordinatorClientForLogin(brokerURL)
		if err != nil {
			return err
		}
		start, err = client.StartGitHubLogin(ctx, pollSecretHash, provider)
		if err != nil {
			return err
		}
	}
	if noBrowser {
		fmt.Fprintf(a.Stderr, "open this GitHub login URL:\n%s\n", start.URL)
	} else if err := openBrowser(start.URL); err != nil {
		fmt.Fprintf(a.Stderr, "could not open browser: %v\nopen this GitHub login URL:\n%s\n", err, start.URL)
	} else {
		fmt.Fprintln(a.Stderr, "opened GitHub login in your browser")
	}
	deadline := time.Now().Add(10 * time.Minute)
	if start.ExpiresAt != "" {
		if parsed, err := time.Parse(time.RFC3339, start.ExpiresAt); err == nil {
			deadline = parsed
		}
	}
	for {
		if time.Now().After(deadline) {
			return exit(3, "GitHub login expired")
		}
		poll, err := client.PollGitHubLogin(ctx, start.LoginID, pollSecret)
		if err != nil {
			return err
		}
		switch poll.Status {
		case "pending":
		case "complete":
			if poll.Token == "" {
				return exit(3, "GitHub login completed without a broker token")
			}
			if provider == "" {
				provider = poll.Provider
			}
			path, cfg, err := writeBrokerLogin(brokerURL, poll.Token, provider)
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

func (a App) finishLogin(ctx context.Context, coord *CoordinatorClient, path string, cfg Config, jsonOut bool) error {
	who, err := coord.Whoami(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(a.Stdout).Encode(map[string]any{
			"config":   path,
			"broker":   cfg.Coordinator,
			"provider": cfg.Provider,
			"identity": who,
		})
	}
	fmt.Fprintf(a.Stdout, "logged in broker=%s provider=%s user=%s org=%s config=%s\n", cfg.Coordinator, cfg.Provider, who.Owner, who.Org, path)
	return nil
}

func coordinatorClientForLogin(brokerURL string) (*CoordinatorClient, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	cfg.Coordinator = brokerURL
	cfg.CoordToken = ""
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, exit(2, "login requires a broker URL")
	}
	return coord, nil
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

func normalizedBrokerURL(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return strings.TrimRight(value, "/")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
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
	fmt.Fprintf(a.Stdout, "user=%s org=%s auth=%s broker=%s\n", who.Owner, who.Org, who.Auth, cfg.Coordinator)
	return nil
}

func writeBrokerLogin(brokerURL, token, provider string) (string, Config, error) {
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
	if provider != "" {
		file.Broker.Provider = provider
		file.Provider = provider
	}
	written, err := writeUserFileConfig(file)
	if err != nil {
		return "", Config{}, err
	}
	cfg, err := loadConfig()
	return written, cfg, err
}
