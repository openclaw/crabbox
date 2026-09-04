package boxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const defaultConsoleURL = "https://app.boxd.sh"
const exchangeRoute = "/api/v1/auth/token"
const maxExchangeResponse = 1 << 20
const jwtRefreshMargin = 5 * time.Minute

// consoleURL validates the HTTPS console origin. The console is used only for
// the API-key exchange; every other call rides the TLS gRPC endpoint.
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

// apiKeyFromEnv reads the boxd API key. Keys are never accepted from
// configuration files or flags, and the retired interactive-session
// environment variables get migration guidance instead of silent reuse.
func apiKeyFromEnv() (string, error) {
	key := os.Getenv("CRABBOX_BOXD_API_KEY")
	if key == "" {
		key = os.Getenv("BOXD_API_KEY")
	}
	if key == "" {
		if os.Getenv("CRABBOX_BOXD_TOKEN") != "" || os.Getenv("BOXD_TOKEN") != "" {
			return "", core.Exit(2, "boxd now authenticates with an API key over TLS gRPC; BOXD_TOKEN interactive sessions are no longer used. Set CRABBOX_BOXD_API_KEY or BOXD_API_KEY to a bxd_ key from the boxd console or `boxd auth keys create`")
		}
		return "", core.Exit(2, "set CRABBOX_BOXD_API_KEY or BOXD_API_KEY to a boxd API key (bxd_...) from the boxd console or `boxd auth keys create`")
	}
	if err := validateAPIKey(key); err != nil {
		return "", err
	}
	return key, nil
}

func validateAPIKey(key string) error {
	rest, ok := strings.CutPrefix(key, "bxd_")
	if !ok || len(rest) != 43 {
		return core.Exit(2, "invalid boxd API key: expected bxd_ followed by 43 base62 characters")
	}
	for _, r := range rest {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return core.Exit(2, "invalid boxd API key: expected bxd_ followed by 43 base62 characters")
		}
	}
	return nil
}

// authSession exchanges the long-lived API key for short-lived JWTs over the
// HTTPS console and re-exchanges before expiry. The raw key is sent only to
// the validated console origin; gRPC metadata carries only the derived JWT.
type authSession struct {
	http     *http.Client
	exchange url.URL
	key      string

	mu     sync.Mutex
	jwt    string
	expiry time.Time
}

func newAuthSession(cfg core.Config, rt core.Runtime, key string) (*authSession, error) {
	base, err := consoleURL(cfg.Boxd.APIURL)
	if err != nil {
		return nil, err
	}
	if err := validateAPIKey(key); err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 30 * time.Second}
	if rt.HTTP != nil {
		client = *rt.HTTP
	}
	// A redirect must never replay the API key to another origin.
	client.Jar = nil
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("boxd API redirects are not allowed") }
	exchange := *base
	exchange.Path = exchangeRoute
	return &authSession{http: &client, exchange: exchange, key: key}, nil
}

func (a *authSession) bearer(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && time.Now().Before(a.expiry.Add(-jwtRefreshMargin)) {
		return a.jwt, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]string{"api_key": a.key})
	if err != nil {
		return "", core.Exit(2, "encode boxd API-key exchange")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.exchange.String(), bytes.NewReader(body))
	if err != nil {
		return "", core.Exit(2, "construct boxd API-key exchange")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Transport errors can include request bodies or URLs. Keep the key
		// out of diagnostics even with a custom injected HTTP transport.
		return "", core.Exit(5, "boxd API-key exchange failed; response details withheld")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnprocessableEntity {
			return "", core.Exit(3, "boxd rejected the API key (HTTP %d); check CRABBOX_BOXD_API_KEY / BOXD_API_KEY and its organization", resp.StatusCode)
		}
		return "", core.Exit(5, "boxd API-key exchange returned HTTP %d", resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxExchangeResponse+1))
	if err != nil || len(payload) > maxExchangeResponse {
		return "", core.Exit(5, "read boxd API-key exchange response")
	}
	var result struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return "", core.Exit(5, "invalid boxd API-key exchange response")
	}
	if err := validateSessionJWT(result.Token); err != nil {
		return "", err
	}
	expiry := time.Now().Add(time.Hour)
	if result.ExpiresAt > 0 {
		expiry = time.Unix(result.ExpiresAt, 0)
	}
	if !expiry.After(time.Now().Add(jwtRefreshMargin)) {
		return "", core.Exit(5, "boxd API-key exchange returned an already-expired session")
	}
	a.jwt, a.expiry = result.Token, expiry
	return a.jwt, nil
}

func validateSessionJWT(token string) error {
	if token == "" || len(token) > 16<<10 || strings.HasPrefix(token, "bxd_") {
		return core.Exit(5, "invalid boxd exchange session token")
	}
	for _, c := range token {
		if c < 33 || c > 126 {
			return core.Exit(5, "invalid boxd exchange session token")
		}
	}
	return nil
}
