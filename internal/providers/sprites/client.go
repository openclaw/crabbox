package sprites

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

type spritesAPI interface {
	CreateSprite(context.Context, string, []string) (spritesInfo, error)
	GetSprite(context.Context, string) (spritesInfo, error)
	ListSprites(context.Context, string) ([]spritesInfo, error)
	DeleteSprite(context.Context, string) error
}

type spritesClient struct {
	token      string
	apiURL     string
	httpClient *http.Client
}

type spritesInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Organization string   `json:"organization"`
	Status       string   `json:"status"`
	URL          string   `json:"url"`
	Labels       []string `json:"labels"`
}

type spritesListResponse struct {
	Sprites               []spritesInfo `json:"sprites"`
	HasMore               bool          `json:"has_more"`
	NextContinuationToken string        `json:"next_continuation_token"`
}

type spritesAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *spritesAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

func newSpritesClient(cfg Config, rt Runtime) (spritesAPI, error) {
	apiURL, origin, err := validateSpritesAPIURL(blank(cfg.Sprites.APIURL, "https://api.sprites.dev"))
	if err != nil {
		return nil, err
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &spritesClient{
		token:      strings.TrimSpace(cfg.Sprites.Token),
		apiURL:     apiURL,
		httpClient: secureSpritesHTTPClient(httpClient, origin),
	}, nil
}

func validateSpritesAPIURL(raw string) (string, *url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", nil, exit(2, "provider=sprites API URL must be an absolute HTTP(S) URL")
	}
	if !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return "", nil, exit(2, "provider=sprites API URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil {
		return "", nil, exit(2, "provider=sprites API URL must not contain userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", nil, exit(2, "provider=sprites API URL must not contain a query")
	}
	if parsed.Fragment != "" {
		return "", nil, exit(2, "provider=sprites API URL must not contain a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	hostname := canonicalSpritesHostname(parsed.Hostname())
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !isSpritesLoopbackHost(hostname)) {
		return "", nil, exit(2, "provider=sprites API URL must use HTTPS except for loopback HTTP")
	}
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	parsed.Host = hostname
	if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(hostname, port)
	}
	return strings.TrimRight(parsed.String(), "/"), parsed, nil
}

func canonicalSpritesHostname(hostname string) string {
	hostname = strings.ToLower(hostname)
	if ip := net.ParseIP(strings.TrimSuffix(hostname, ".")); ip != nil {
		return ip.String()
	}
	return hostname
}

func isSpritesLoopbackHost(hostname string) bool {
	if hostname == "localhost" {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func secureSpritesHTTPClient(source *http.Client, origin *url.URL) *http.Client {
	client := *source
	checkRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameSpritesOrigin(origin, req.URL) {
			return fmt.Errorf("sprites redirect changed API origin")
		}
		if checkRedirect != nil {
			return checkRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func sameSpritesOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		canonicalSpritesHostname(a.Hostname()) == canonicalSpritesHostname(b.Hostname()) &&
		effectiveSpritesPort(a) == effectiveSpritesPort(b)
}

func effectiveSpritesPort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	if strings.EqualFold(value.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(value.Scheme, "http") {
		return "80"
	}
	return ""
}

func (c *spritesClient) CreateSprite(ctx context.Context, name string, labels []string) (spritesInfo, error) {
	body := map[string]any{"name": name, "labels": labels}
	var sprite spritesInfo
	if err := c.doJSON(ctx, http.MethodPost, "/v1/sprites", nil, body, &sprite); err != nil {
		return spritesInfo{}, err
	}
	if sprite.Name == "" {
		sprite.Name = name
	}
	if len(sprite.Labels) == 0 {
		sprite.Labels = labels
	}
	return sprite, nil
}

func (c *spritesClient) GetSprite(ctx context.Context, name string) (spritesInfo, error) {
	var sprite spritesInfo
	if err := c.doJSON(ctx, http.MethodGet, "/v1/sprites/"+url.PathEscape(name), nil, nil, &sprite); err != nil {
		return spritesInfo{}, err
	}
	return sprite, nil
}

func (c *spritesClient) ListSprites(ctx context.Context, prefix string) ([]spritesInfo, error) {
	var all []spritesInfo
	continuation := ""
	seenContinuations := map[string]bool{}
	for {
		query := url.Values{}
		query.Set("max_results", "50")
		if prefix != "" {
			query.Set("prefix", prefix)
		}
		if continuation != "" {
			query.Set("continuation_token", continuation)
		}
		var page spritesListResponse
		if err := c.doJSON(ctx, http.MethodGet, "/v1/sprites", query, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Sprites...)
		if !page.HasMore {
			return all, nil
		}
		next := strings.TrimSpace(page.NextContinuationToken)
		if next == "" {
			return nil, fmt.Errorf("sprites list response has_more without next_continuation_token")
		}
		if seenContinuations[next] {
			return nil, fmt.Errorf("sprites list response repeated continuation token %q", next)
		}
		seenContinuations[next] = true
		continuation = next
	}
}

func (c *spritesClient) DeleteSprite(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/sprites/"+url.PathEscape(name), nil, nil, nil)
}

func (c *spritesClient) doJSON(ctx context.Context, method, requestPath string, query url.Values, body any, out any) error {
	var r io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		r = &buf
	}
	endpoint := c.apiURL + requestPath
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &spritesAPIError{
			StatusCode: resp.StatusCode,
			Status:     shared.RedactErrorSecrets(resp.Status, c.token),
			Body:       shared.RedactErrorSecrets(summarizeSpritesBody(data), c.token),
		}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse sprites response: %w", err)
	}
	return nil
}

func spritesAPILabels(leaseID, slug string) []string {
	return []string{
		"crabbox",
		"provider-sprites",
		"lease-" + strings.ReplaceAll(leaseID, "_", "-"),
		"slug-" + normalizeLeaseSlug(slug),
	}
}

func isCrabboxSprite(sprite spritesInfo) bool {
	for _, label := range sprite.Labels {
		if label == "crabbox" || strings.HasPrefix(label, "lease-cbx-") {
			return true
		}
	}
	return false
}

func isLegacyCrabboxSpriteName(sprite spritesInfo) bool {
	return strings.HasPrefix(sprite.Name, "crabbox-")
}

func spritesLeaseID(sprite spritesInfo) string {
	for _, label := range sprite.Labels {
		if strings.HasPrefix(label, "lease-cbx-") {
			return "cbx_" + strings.TrimPrefix(label, "lease-cbx-")
		}
	}
	return ""
}

func spritesSlug(leaseID string, sprite spritesInfo) string {
	for _, label := range sprite.Labels {
		if strings.HasPrefix(label, "slug-") {
			return normalizeLeaseSlug(strings.TrimPrefix(label, "slug-"))
		}
	}
	if leaseID != "" {
		return newLeaseSlug(leaseID)
	}
	return normalizeLeaseSlug(sprite.Name)
}

func isSpritesNotFound(err error) bool {
	apiErr, ok := err.(*spritesAPIError)
	return ok && apiErr.StatusCode == http.StatusNotFound
}

func spritesError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("sprites %s: %w", action, err)
}

func summarizeSpritesBody(data []byte) string {
	text := strings.TrimSpace(string(data))
	if len(text) > 4096 {
		return text[:4096] + "..."
	}
	return text
}
