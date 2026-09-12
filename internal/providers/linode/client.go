package linode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const apiBaseURL = "https://api.linode.com/v4"

type linodeClient struct {
	token   string
	client  *http.Client
	baseURL string
}

type linodeAPIError struct {
	Operation string
	Status    int
	Body      string
}

func (e *linodeAPIError) Error() string {
	return fmt.Sprintf("linode %s: http %d: %s", e.Operation, e.Status, e.Body)
}

func newLinodeClient(rt core.Runtime) (*linodeClient, error) {
	token, err := requireToken()
	if err != nil {
		return nil, err
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &linodeClient{token: token, client: httpClient, baseURL: apiBaseURL}, nil
}

func (c *linodeClient) do(ctx context.Context, method, path string, body any, out any) error {
	req, err := shared.NewJSONRequest(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(data) > 400 {
			data = data[:400]
		}
		body := c.redactErrorBody(strings.TrimSpace(string(data)))
		if readErr != nil {
			if body != "" {
				body += "; "
			}
			body += "response body read failed: " + readErr.Error()
		}
		return &linodeAPIError{Operation: method + " " + path, Status: resp.StatusCode, Body: body}
	}
	if readErr != nil {
		return fmt.Errorf("linode %s %s response body: %w", method, path, readErr)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("linode %s %s decode: %w", method, path, err)
	}
	return nil
}

func (c *linodeClient) redactErrorBody(body string) string {
	if c.token != "" {
		body = strings.ReplaceAll(body, c.token, "<redacted>")
	}
	for _, field := range []string{"root_pass", "user_data"} {
		body = redactJSONishField(body, field)
		body = redactInlineField(body, field)
	}
	return body
}

func redactJSONishField(body, field string) string {
	pattern := regexp.MustCompile(`("` + regexp.QuoteMeta(field) + `"\s*:\s*")[^"]*(")`)
	return pattern.ReplaceAllString(body, `${1}<redacted>${2}`)
}

func redactInlineField(body, field string) string {
	pattern := regexp.MustCompile(`(?i)(` + regexp.QuoteMeta(field) + `\s*[=: ]\s*)[^",\s]+`)
	return pattern.ReplaceAllString(body, `${1}<redacted>`)
}

func (c *linodeClient) AccountID(ctx context.Context) (string, error) {
	var res account
	if err := c.do(ctx, http.MethodGet, "/account", nil, &res); err != nil {
		return "", err
	}
	if strings.TrimSpace(res.EUUID) == "" {
		return "", core.Exit(3, "linode account response did not include euuid identity")
	}
	return "euuid:" + strings.TrimSpace(res.EUUID), nil
}

func (c *linodeClient) AccountSettings(ctx context.Context) (accountSettings, error) {
	var out accountSettings
	if err := c.do(ctx, http.MethodGet, "/account/settings", nil, &out); err != nil {
		return accountSettings{}, err
	}
	return out, nil
}

func (c *linodeClient) ListLinodes(ctx context.Context) ([]linodeInstance, error) {
	return listLinodePages[linodeInstance](ctx, c, "/linode/instances")
}

func (c *linodeClient) GetLinode(ctx context.Context, id int64) (linodeInstance, error) {
	var out linodeInstance
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/linode/instances/%d", id), nil, &out); err != nil {
		return linodeInstance{}, err
	}
	return out, nil
}

func (c *linodeClient) CreateLinode(ctx context.Context, req createLinodeRequest) (linodeInstance, error) {
	var out linodeInstance
	if err := c.do(ctx, http.MethodPost, "/linode/instances", req, &out); err != nil {
		return linodeInstance{}, err
	}
	return out, nil
}

func (c *linodeClient) DeleteLinode(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/linode/instances/%d", id), nil, nil)
}

func (c *linodeClient) UpdateLinodeTags(ctx context.Context, id int64, tags []string) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/linode/instances/%d", id), map[string]any{"tags": tags}, nil)
}

func (c *linodeClient) ListTypes(ctx context.Context) ([]linodeType, error) {
	return listLinodePages[linodeType](ctx, c, "/linode/types")
}

func (c *linodeClient) ListImages(ctx context.Context) ([]linodeImage, error) {
	return listLinodePages[linodeImage](ctx, c, "/images")
}

func (c *linodeClient) ListRegions(ctx context.Context) ([]linodeRegion, error) {
	return listLinodePages[linodeRegion](ctx, c, "/regions")
}

func (c *linodeClient) ListFirewalls(ctx context.Context) ([]linodeFirewall, error) {
	return listLinodePages[linodeFirewall](ctx, c, "/networking/firewalls")
}

func (c *linodeClient) AddFirewallDevice(ctx context.Context, firewallID int64, req firewallDeviceRequest) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/networking/firewalls/%d/devices", firewallID), req, nil)
}

func listLinodePages[T any](ctx context.Context, c *linodeClient, path string) ([]T, error) {
	var out []T
	page := 1
	for {
		nextPath := withPage(path, page)
		var raw struct {
			Page    int             `json:"page"`
			Pages   int             `json:"pages"`
			Results int             `json:"results"`
			Data    json.RawMessage `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, nextPath, nil, &raw); err != nil {
			return out, err
		}
		var data []T
		if err := json.Unmarshal(raw.Data, &data); err != nil {
			return out, fmt.Errorf("linode %s decode data: %w", nextPath, err)
		}
		out = append(out, data...)
		if raw.Pages <= 0 || page >= raw.Pages {
			return out, nil
		}
		page++
	}
}

func withPage(path string, page int) string {
	u, err := url.Parse(path)
	if err != nil {
		return path
	}
	q := u.Query()
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", "500")
	u.RawQuery = q.Encode()
	return u.String()
}
