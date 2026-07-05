package fastapicloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type fastAPICloudAPI interface {
	GetApp(ctx context.Context, appID string) (fastAPICloudApp, error)
	ListApps(ctx context.Context, teamID string) ([]fastAPICloudApp, error)
	LatestDeployment(ctx context.Context, appID string) (fastAPICloudDeployment, bool, error)
}

type fastAPICloudClient struct {
	token      string
	apiURL     string
	httpClient *http.Client
}

const (
	fastAPICloudMaxResponseBytes = 16 << 20
	fastAPICloudListPageSize     = 100
)

type fastAPICloudAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *fastAPICloudAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

type fastAPICloudApp struct {
	ID        string `json:"id"`
	TeamID    string `json:"team_id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Directory string `json:"directory"`
	URL       string `json:"url"`
	Region    string `json:"region"`
	UpdatedAt string `json:"updated_at"`
}

type fastAPICloudDeployment struct {
	ID           string                       `json:"id"`
	AppID        string                       `json:"app_id"`
	Slug         string                       `json:"slug"`
	Status       fastAPICloudDeploymentStatus `json:"status"`
	CreatedAt    string                       `json:"created_at"`
	URL          string                       `json:"url"`
	DashboardURL string                       `json:"dashboard_url"`
}

type fastAPICloudDeploymentStatus string

const (
	fastAPICloudStatusWaitingUpload       fastAPICloudDeploymentStatus = "waiting_upload"
	fastAPICloudStatusUploadCancelled     fastAPICloudDeploymentStatus = "upload_cancelled"
	fastAPICloudStatusReadyForBuild       fastAPICloudDeploymentStatus = "ready_for_build"
	fastAPICloudStatusBuilding            fastAPICloudDeploymentStatus = "building"
	fastAPICloudStatusExtracting          fastAPICloudDeploymentStatus = "extracting"
	fastAPICloudStatusExtractingFailed    fastAPICloudDeploymentStatus = "extracting_failed"
	fastAPICloudStatusBuildingImage       fastAPICloudDeploymentStatus = "building_image"
	fastAPICloudStatusBuildingImageFailed fastAPICloudDeploymentStatus = "building_image_failed"
	fastAPICloudStatusDeploying           fastAPICloudDeploymentStatus = "deploying"
	fastAPICloudStatusDeployingFailed     fastAPICloudDeploymentStatus = "deploying_failed"
	fastAPICloudStatusVerifying           fastAPICloudDeploymentStatus = "verifying"
	fastAPICloudStatusVerifyingFailed     fastAPICloudDeploymentStatus = "verifying_failed"
	fastAPICloudStatusVerifyingSkipped    fastAPICloudDeploymentStatus = "verifying_skipped"
	fastAPICloudStatusSuccess             fastAPICloudDeploymentStatus = "success"
	fastAPICloudStatusExpired             fastAPICloudDeploymentStatus = "expired"
	fastAPICloudStatusFailed              fastAPICloudDeploymentStatus = "failed"
)

func (s *fastAPICloudDeploymentStatus) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = fastAPICloudDeploymentStatus(strings.ToLower(strings.TrimSpace(raw)))
	return nil
}

func (s fastAPICloudDeploymentStatus) Normalized() fastAPICloudDeploymentStatus {
	return fastAPICloudDeploymentStatus(strings.ToLower(strings.TrimSpace(string(s))))
}

func (s fastAPICloudDeploymentStatus) IsReady() bool {
	switch s.Normalized() {
	case fastAPICloudStatusSuccess, fastAPICloudStatusVerifyingSkipped:
		return true
	default:
		return false
	}
}

func (s fastAPICloudDeploymentStatus) State() string {
	switch s.Normalized() {
	case fastAPICloudStatusSuccess, fastAPICloudStatusVerifyingSkipped:
		return "ready"
	case fastAPICloudStatusFailed,
		fastAPICloudStatusVerifyingFailed,
		fastAPICloudStatusDeployingFailed,
		fastAPICloudStatusBuildingImageFailed,
		fastAPICloudStatusExtractingFailed,
		fastAPICloudStatusUploadCancelled:
		return "failed"
	case fastAPICloudStatusExpired:
		return "expired"
	case "":
		return "unknown"
	default:
		return string(s.Normalized())
	}
}

func newFastAPICloudClient(cfg Config, rt Runtime) (fastAPICloudAPI, error) {
	token := strings.TrimSpace(cfg.FastAPICloud.Token)
	if token == "" {
		return nil, exit(2, "provider=%s requires FASTAPI_CLOUD_TOKEN", providerName)
	}
	apiURL, err := validateFastAPICloudAPIURL(blank(cfg.FastAPICloud.APIURL, "https://api.fastapicloud.com/api/v1"))
	if err != nil {
		return nil, err
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &fastAPICloudClient{token: token, apiURL: apiURL, httpClient: secureFastAPICloudHTTPClient(httpClient, apiURL)}, nil
}

func validateFastAPICloudAPIURL(raw string) (string, error) {
	apiURL := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", exit(2, "provider=%s API URL must be an absolute HTTPS URL", providerName)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", exit(2, "provider=%s API URL must not contain userinfo, query parameters, or a fragment", providerName)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return "", exit(2, "provider=%s API URL must use HTTPS except for loopback development endpoints", providerName)
	}
	return apiURL, nil
}

func secureFastAPICloudHTTPClient(source *http.Client, apiURL string) *http.Client {
	client := *source
	trusted, _ := url.Parse(apiURL)
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameFastAPICloudOrigin(trusted, req.URL) {
			return &fastAPICloudRedirectError{origin: fastAPICloudRedirectOrigin(req.URL)}
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func sameFastAPICloudOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveFastAPICloudPort(a) == effectiveFastAPICloudPort(b)
}

func effectiveFastAPICloudPort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

type fastAPICloudRedirectError struct {
	origin string
}

func (e *fastAPICloudRedirectError) Error() string {
	return fmt.Sprintf("%s refused cross-origin redirect to %s", providerName, e.origin)
}

func fastAPICloudRedirectOrigin(value *url.URL) string {
	if value == nil || value.Scheme == "" || value.Host == "" {
		return "<redacted>"
	}
	return value.Scheme + "://" + value.Host
}

func (c *fastAPICloudClient) GetApp(ctx context.Context, appID string) (fastAPICloudApp, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return fastAPICloudApp{}, fmt.Errorf("get app: app id is required")
	}
	var app fastAPICloudApp
	if err := c.get(ctx, "/apps/"+url.PathEscape(appID), nil, &app); err != nil {
		return fastAPICloudApp{}, err
	}
	return app, nil
}

func (c *fastAPICloudClient) ListApps(ctx context.Context, teamID string) ([]fastAPICloudApp, error) {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return nil, fmt.Errorf("list apps: team id is required")
	}
	var apps []fastAPICloudApp
	for skip := 0; ; {
		var page fastAPICloudListResponse[fastAPICloudApp]
		params := url.Values{
			"team_id": []string{teamID},
			"limit":   []string{fmt.Sprint(fastAPICloudListPageSize)},
			"skip":    []string{fmt.Sprint(skip)},
		}
		if err := c.get(ctx, "/apps/", params, &page); err != nil {
			return nil, err
		}
		apps = append(apps, page.Data...)
		// A short/empty page is the canonical end-of-pages signal. Only trust the
		// envelope's count when it is actually present (>0): a full page with an
		// absent/zero "count" must not be mistaken for the final page.
		if len(page.Data) == 0 || len(page.Data) < fastAPICloudListPageSize || (page.Count > 0 && len(apps) >= page.Count) {
			return apps, nil
		}
		skip += len(page.Data)
	}
}

func (c *fastAPICloudClient) LatestDeployment(ctx context.Context, appID string) (fastAPICloudDeployment, bool, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return fastAPICloudDeployment{}, false, fmt.Errorf("latest deployment: app id is required")
	}
	// The deployments endpoint's default sort order is not a documented part of the
	// contract, so don't assume Data[0] is newest: fetch a page and select the latest
	// by created_at client-side. (Latest-deployment readiness is the common case and
	// sits well within a single page.)
	var page fastAPICloudListResponse[fastAPICloudDeployment]
	params := url.Values{
		"limit": []string{fmt.Sprint(fastAPICloudListPageSize)},
		"skip":  []string{"0"},
	}
	if err := c.get(ctx, "/apps/"+url.PathEscape(appID)+"/deployments/", params, &page); err != nil {
		return fastAPICloudDeployment{}, false, err
	}
	if len(page.Data) == 0 {
		return fastAPICloudDeployment{}, false, nil
	}
	latest := page.Data[0]
	for _, d := range page.Data[1:] {
		if fastAPICloudDeploymentNewer(d, latest) {
			latest = d
		}
	}
	return latest, true, nil
}

// fastAPICloudDeploymentNewer reports whether a is more recent than b by created_at.
// Timestamps are RFC3339; fall back to lexical comparison (which also sorts RFC3339
// chronologically) when either value cannot be parsed.
func fastAPICloudDeploymentNewer(a, b fastAPICloudDeployment) bool {
	at, aerr := time.Parse(time.RFC3339, a.CreatedAt)
	bt, berr := time.Parse(time.RFC3339, b.CreatedAt)
	if aerr == nil && berr == nil {
		return at.After(bt)
	}
	return a.CreatedAt > b.CreatedAt
}

type fastAPICloudListResponse[T any] struct {
	Data  []T `json:"data"`
	Count int `json:"count"`
}

func (c *fastAPICloudClient) get(ctx context.Context, apiPath string, params url.Values, out any) error {
	endpoint, err := c.endpoint(apiPath, params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// net/http wraps CheckRedirect failures with the untrusted Location URL.
		var redirectErr *fastAPICloudRedirectError
		if errors.As(err, &redirectErr) {
			return redirectErr
		}
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, fastAPICloudMaxResponseBytes+1))
	if readErr != nil {
		return readErr
	}
	if len(data) > fastAPICloudMaxResponseBytes {
		return fmt.Errorf("%s response exceeds %d bytes", providerName, fastAPICloudMaxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &fastAPICloudAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(data))}
	}
	if out != nil {
		// A 2xx with a non-JSON body (an HTML error page from a proxy, a
		// misconfigured gateway) would otherwise surface only as an opaque
		// json.Unmarshal error; check the media type first for a clearer message.
		if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "" && mediaType != "application/json" {
			return fmt.Errorf("%s expected application/json response, got %q", providerName, resp.Header.Get("Content-Type"))
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s response: %w", providerName, err)
		}
	}
	return nil
}

func (c *fastAPICloudClient) endpoint(apiPath string, params url.Values) (string, error) {
	if !strings.HasPrefix(apiPath, "/") {
		apiPath = "/" + apiPath
	}
	parsed, err := url.Parse(c.apiURL + apiPath)
	if err != nil {
		return "", err
	}
	if len(params) > 0 {
		parsed.RawQuery = params.Encode()
	}
	return parsed.String(), nil
}

func isLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
