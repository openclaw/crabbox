package sprites

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

func newSpritesClient(cfg Config, rt Runtime) spritesAPI {
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	apiURL := strings.TrimRight(blank(cfg.Sprites.APIURL, "https://api.sprites.dev"), "/")
	return &spritesClient{token: strings.TrimSpace(cfg.Sprites.Token), apiURL: apiURL, httpClient: httpClient}
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
		return &spritesAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: summarizeSpritesBody(data)}
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
