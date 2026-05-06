package daytona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	daytona "github.com/daytonaio/daytona/libs/api-client-go"
	sdkdaytona "github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	sdktypes "github.com/daytonaio/daytona/libs/sdk-go/pkg/types"
)

type daytonaAPI interface {
	CreateSandbox(context.Context, daytona.CreateSandbox) (*daytona.Sandbox, error)
	GetSandbox(context.Context, string) (*daytona.Sandbox, error)
	ListCrabboxSandboxes(context.Context) ([]daytona.Sandbox, error)
	StartSandbox(context.Context, string) (*daytona.Sandbox, error)
	DeleteSandbox(context.Context, string) error
	ReplaceLabels(context.Context, string, map[string]string) error
	UpdateLastActivity(context.Context, string) error
	CreateSSHAccess(context.Context, string, time.Duration) (daytonaSSHAccess, error)
}

type daytonaSSHAccess struct {
	Token   string
	Command string
}

type daytonaSDKClient struct {
	api   *daytona.APIClient
	token string
	orgID string
}

const defaultDaytonaAPIURL = "https://app.daytona.io/api"

func newDaytonaClient(cfg Config, rt Runtime) (daytonaAPI, error) {
	auth, err := daytonaAuthConfig(cfg)
	if err != nil {
		return nil, err
	}
	apiURL := daytonaAPIURL(cfg, auth)
	apiCfg := daytona.NewConfiguration()
	apiCfg.Servers = daytona.ServerConfigurations{{URL: apiURL}}
	if rt.HTTP != nil {
		apiCfg.HTTPClient = rt.HTTP
	}
	return &daytonaSDKClient{api: daytona.NewAPIClient(apiCfg), token: auth.token(), orgID: auth.OrganizationID}, nil
}

type daytonaAuth struct {
	APIKey         string
	JWTToken       string
	OrganizationID string
	APIURL         string
}

func (a daytonaAuth) token() string {
	if a.APIKey != "" {
		return a.APIKey
	}
	return a.JWTToken
}

func daytonaAuthConfig(cfg Config) (daytonaAuth, error) {
	auth := daytonaAuth{
		APIKey:         strings.TrimSpace(cfg.Daytona.APIKey),
		JWTToken:       strings.TrimSpace(cfg.Daytona.JWTToken),
		OrganizationID: strings.TrimSpace(cfg.Daytona.OrganizationID),
		APIURL:         strings.TrimSpace(cfg.Daytona.APIURL),
	}
	if auth.APIKey == "" && auth.JWTToken == "" {
		if cliAuth, err := daytonaCLIAuthConfig(); err == nil {
			auth = mergeDaytonaCLIAuth(auth, cliAuth)
		} else if !errors.Is(err, os.ErrNotExist) {
			return daytonaAuth{}, err
		}
	}
	if auth.APIKey == "" && auth.JWTToken == "" {
		return daytonaAuth{}, exit(3, "provider=daytona requires DAYTONA_API_KEY, DAYTONA_JWT_TOKEN, or an authenticated Daytona CLI profile")
	}
	if auth.APIKey == "" && auth.JWTToken != "" && auth.OrganizationID == "" {
		return daytonaAuth{}, exit(3, "provider=daytona with DAYTONA_JWT_TOKEN requires DAYTONA_ORGANIZATION_ID")
	}
	return auth, nil
}

func daytonaAPIURL(cfg Config, auth daytonaAuth) string {
	configured := strings.TrimSpace(cfg.Daytona.APIURL)
	if configured != "" && configured != defaultDaytonaAPIURL {
		return strings.TrimRight(configured, "/")
	}
	if auth.APIURL != "" {
		return strings.TrimRight(auth.APIURL, "/")
	}
	return strings.TrimRight(blank(configured, defaultDaytonaAPIURL), "/")
}

func newDaytonaToolboxClient(cfg Config) (*sdkdaytona.Client, error) {
	auth, err := daytonaAuthConfig(cfg)
	if err != nil {
		return nil, err
	}
	return sdkdaytona.NewClientWithConfig(&sdktypes.DaytonaConfig{
		APIKey:         auth.APIKey,
		JWTToken:       auth.JWTToken,
		OrganizationID: auth.OrganizationID,
		APIUrl:         daytonaAPIURL(cfg, auth),
		Target:         strings.TrimSpace(cfg.Daytona.Target),
	})
}

type daytonaCLIConfig struct {
	ActiveProfile string              `json:"activeProfile"`
	Profiles      []daytonaCLIProfile `json:"profiles"`
}

type daytonaCLIProfile struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	ActiveOrganizationID string `json:"activeOrganizationId"`
	API                  struct {
		URL string `json:"url"`
		Key string `json:"key"`
	} `json:"api"`
}

func daytonaCLIAuthConfig() (daytonaAuth, error) {
	paths := daytonaCLIConfigPaths()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return daytonaAuth{}, fmt.Errorf("read Daytona CLI config %s: %w", path, err)
		}
		auth, err := parseDaytonaCLIAuthConfig(data)
		if err != nil {
			return daytonaAuth{}, fmt.Errorf("read Daytona CLI config %s: %w", path, err)
		}
		if auth.APIKey != "" || auth.JWTToken != "" {
			return auth, nil
		}
	}
	return daytonaAuth{}, os.ErrNotExist
}

func daytonaCLIConfigPaths() []string {
	var candidates []string
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		candidates = append(candidates,
			filepath.Join(dir, "daytona", "config.json"),
			filepath.Join(dir, "Daytona", "config.json"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".config", "daytona", "config.json"),
			filepath.Join(home, ".daytona", "config.json"),
		)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out
}

func parseDaytonaCLIAuthConfig(data []byte) (daytonaAuth, error) {
	var config daytonaCLIConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return daytonaAuth{}, err
	}
	var selected *daytonaCLIProfile
	for i := range config.Profiles {
		profile := &config.Profiles[i]
		if config.ActiveProfile == "" || profile.ID == config.ActiveProfile || profile.Name == config.ActiveProfile {
			selected = profile
			break
		}
	}
	if selected == nil && len(config.Profiles) > 0 {
		selected = &config.Profiles[0]
	}
	if selected == nil {
		return daytonaAuth{}, nil
	}
	return daytonaAuth{
		APIKey:         strings.TrimSpace(selected.API.Key),
		OrganizationID: strings.TrimSpace(selected.ActiveOrganizationID),
		APIURL:         strings.TrimSpace(selected.API.URL),
	}, nil
}

func mergeDaytonaCLIAuth(auth, cliAuth daytonaAuth) daytonaAuth {
	if auth.APIKey == "" && auth.JWTToken == "" {
		auth.APIKey = cliAuth.APIKey
		auth.JWTToken = cliAuth.JWTToken
	}
	if auth.OrganizationID == "" {
		auth.OrganizationID = cliAuth.OrganizationID
	}
	if auth.APIURL == "" || auth.APIURL == defaultDaytonaAPIURL {
		auth.APIURL = cliAuth.APIURL
	}
	return auth
}

func (c *daytonaSDKClient) ctx(ctx context.Context) context.Context {
	return context.WithValue(ctx, daytona.ContextAccessToken, c.token)
}

func (c *daytonaSDKClient) CreateSandbox(ctx context.Context, body daytona.CreateSandbox) (*daytona.Sandbox, error) {
	req := c.api.SandboxAPI.CreateSandbox(c.ctx(ctx)).CreateSandbox(body)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	out, _, err := req.Execute()
	return out, err
}

func (c *daytonaSDKClient) GetSandbox(ctx context.Context, id string) (*daytona.Sandbox, error) {
	req := c.api.SandboxAPI.GetSandbox(c.ctx(ctx), id).Verbose(true)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	out, _, err := req.Execute()
	return out, err
}

func (c *daytonaSDKClient) ListCrabboxSandboxes(ctx context.Context) ([]daytona.Sandbox, error) {
	filter, _ := json.Marshal(map[string]string{"crabbox": "true"})
	var all []daytona.Sandbox
	for page := float32(1); ; page++ {
		req := c.api.SandboxAPI.ListSandboxesPaginated(c.ctx(ctx)).Page(page).Limit(100).Labels(string(filter))
		if c.orgID != "" {
			req = req.XDaytonaOrganizationID(c.orgID)
		}
		out, _, err := req.Execute()
		if err != nil {
			return nil, err
		}
		if out == nil {
			return all, nil
		}
		all = append(all, out.GetItems()...)
		if out.GetTotalPages() <= page || len(out.GetItems()) == 0 {
			return all, nil
		}
	}
}

func (c *daytonaSDKClient) StartSandbox(ctx context.Context, id string) (*daytona.Sandbox, error) {
	req := c.api.SandboxAPI.StartSandbox(c.ctx(ctx), id)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	out, _, err := req.Execute()
	return out, err
}

func (c *daytonaSDKClient) DeleteSandbox(ctx context.Context, id string) error {
	req := c.api.SandboxAPI.DeleteSandbox(c.ctx(ctx), id)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	_, _, err := req.Execute()
	return err
}

func (c *daytonaSDKClient) ReplaceLabels(ctx context.Context, id string, labels map[string]string) error {
	req := c.api.SandboxAPI.ReplaceLabels(c.ctx(ctx), id).SandboxLabels(*daytona.NewSandboxLabels(labels))
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	_, _, err := req.Execute()
	return err
}

func (c *daytonaSDKClient) UpdateLastActivity(ctx context.Context, id string) error {
	req := c.api.SandboxAPI.UpdateLastActivity(c.ctx(ctx), id)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	_, err := req.Execute()
	return err
}

func (c *daytonaSDKClient) CreateSSHAccess(ctx context.Context, id string, ttl time.Duration) (daytonaSSHAccess, error) {
	req := c.api.SandboxAPI.CreateSshAccess(c.ctx(ctx), id).ExpiresInMinutes(float32(durationMinutesCeil(ttl)))
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	out, _, err := req.Execute()
	if err != nil {
		return daytonaSSHAccess{}, err
	}
	if out == nil || out.GetToken() == "" {
		return daytonaSSHAccess{}, fmt.Errorf("daytona ssh access response missing token")
	}
	return daytonaSSHAccess{Token: out.GetToken(), Command: out.GetSshCommand()}, nil
}

func daytonaError(action string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *daytona.GenericOpenAPIError
	if errors.As(err, &apiErr) {
		body := strings.TrimSpace(summarizeJSON(apiErr.Body()))
		if body != "" {
			return fmt.Errorf("daytona %s: %s: %s", action, apiErr.Error(), body)
		}
	}
	return fmt.Errorf("daytona %s: %w", action, err)
}
