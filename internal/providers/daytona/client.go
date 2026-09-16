package daytona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	daytona "github.com/daytonaio/daytona/libs/api-client-go"
	sdkdaytona "github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	sdktypes "github.com/daytonaio/daytona/libs/sdk-go/pkg/types"
	toolbox "github.com/daytonaio/daytona/libs/toolbox-api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type daytonaAPI interface {
	GetSnapshot(context.Context, string) (*daytona.SnapshotDto, error)
	CreateSandbox(context.Context, daytona.CreateSandbox) (*daytona.Sandbox, error)
	GetSandbox(context.Context, string) (*daytona.Sandbox, error)
	ListCrabboxSandboxes(context.Context) ([]daytona.Sandbox, error)
	StartSandbox(context.Context, string) (*daytona.Sandbox, error)
	DeleteSandbox(context.Context, string) error
	ReplaceLabels(context.Context, string, map[string]string) error
	UpdateLastActivity(context.Context, string) error
	SetAutoStopInterval(context.Context, string, time.Duration) error
	CreateSSHAccess(context.Context, string, time.Duration) (daytonaSSHAccess, error)
}

type daytonaSSHAccess struct {
	Token   string
	Command string
}

type daytonaSDKClient struct {
	api    *daytona.APIClient
	token  string
	orgID  string
	apiURL string
	apiKey bool
}

const defaultDaytonaAPIURL = "https://app.daytona.io/api"
const daytonaControlTimeout = 60 * time.Second

var newDaytonaClient = func(cfg core.Config, rt core.Runtime) (daytonaAPI, error) {
	auth, err := daytonaAuthConfig(cfg)
	if err != nil {
		return nil, err
	}
	apiURL := daytonaAPIURL(cfg, auth)
	apiCfg := daytona.NewConfiguration()
	apiCfg.Servers = daytona.ServerConfigurations{{URL: apiURL}}
	// Request builders own this header. A default adds a second literal-key
	// copy beside Go's canonical key, which Daytona rejects for OAuth profiles.
	controlClient := rt.HTTP
	if controlClient == nil {
		controlClient = &http.Client{Timeout: daytonaControlTimeout}
	}
	apiCfg.HTTPClient, err = daytonaHTTPClient(controlClient, apiURL)
	if err != nil {
		return nil, err
	}
	return &daytonaSDKClient{api: daytona.NewAPIClient(apiCfg), token: auth.token(), orgID: auth.OrganizationID, apiURL: apiURL, apiKey: auth.APIKey != ""}, nil
}

// Resolve native organization identity using the same authenticated client.
// An API key's optional organization header is not an attestation: Daytona
// derives its organization from the key and ignores that header.
func (c *daytonaSDKClient) fixedOrganization(ctx context.Context, allowResourceIdentity bool) (string, string, error) {
	if !c.apiKey {
		if c.orgID == "" {
			return "", "", core.Exit(4, "Daytona fixed leases require a selected organization")
		}
		organization, _, err := c.api.OrganizationsAPI.GetOrganization(c.ctx(ctx), c.orgID).Execute()
		if err != nil {
			return "", "", c.redactError(err)
		}
		if organization == nil || organization.GetId() != c.orgID {
			return "", "", core.Exit(4, "Daytona authenticated organization does not match the selected organization")
		}
		return c.apiURL, organization.GetId(), nil
	}
	key, _, err := c.api.ApiKeysAPI.GetCurrentApiKey(c.ctx(ctx)).Execute()
	if err != nil {
		return "", "", c.redactError(err)
	}
	if key == nil {
		return "", "", core.Exit(4, "Daytona current API-key response is missing its identity")
	}
	// Deployed Daytona supplies this field; the pinned SDK retains it as an
	// additional property. Never persist or report the other key metadata.
	if value, present := key.AdditionalProperties["organizationId"]; present {
		organization, valid := value.(string)
		if !valid || strings.TrimSpace(organization) == "" || organization != strings.TrimSpace(organization) {
			return "", "", core.Exit(4, "Daytona current API-key response has an invalid organization identity")
		}
		if c.orgID != "" && c.orgID != organization {
			return "", "", core.Exit(4, "Daytona authenticated API-key organization differs from the selected organization")
		}
		return c.apiURL, organization, nil
	}
	if !allowResourceIdentity {
		return "", "", core.Exit(4, "Daytona current API-key response does not expose organizationId; this API deployment cannot attest fixed cleanup with an API key; use an OAuth organization profile")
	}
	// The public v0.190.0 server omits organizationId from current-key metadata.
	// Preserve its resource-backed acquisition contract, never absence proof.
	identity := c.api.SandboxAPI.ListSandboxes(c.ctx(ctx)).Limit(1)
	if c.orgID != "" {
		identity = identity.XDaytonaOrganizationID(c.orgID)
	}
	items, _, err := identity.Execute()
	if err != nil {
		return "", "", c.redactError(err)
	}
	if items == nil || len(items.GetItems()) != 1 {
		return "", "", core.Exit(4, "Daytona API-key fixed leases need an existing sandbox to establish organization identity; use an authenticated Daytona CLI organization profile, or ordinary warmup without --lease-id")
	}
	item := items.GetItems()[0]
	if item.GetId() == "" || item.GetOrganizationId() == "" {
		return "", "", core.Exit(4, "Daytona sandbox inventory did not establish organization identity")
	}
	sandbox, err := c.GetSandbox(ctx, item.GetId())
	if err != nil {
		return "", "", err
	}
	if sandbox == nil || sandbox.GetId() != item.GetId() || sandbox.GetOrganizationId() != item.GetOrganizationId() ||
		(c.orgID != "" && c.orgID != sandbox.GetOrganizationId()) {
		return "", "", core.Exit(4, "Daytona authenticated sandbox organization does not match its selected scope")
	}
	return c.apiURL, sandbox.GetOrganizationId(), nil
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

func daytonaAuthConfig(cfg core.Config) (daytonaAuth, error) {
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
		return daytonaAuth{}, core.Exit(3, "provider=daytona requires DAYTONA_API_KEY, DAYTONA_JWT_TOKEN, or an authenticated Daytona CLI profile")
	}
	if auth.APIKey == "" && auth.JWTToken != "" && auth.OrganizationID == "" {
		return daytonaAuth{}, core.Exit(3, "provider=daytona with DAYTONA_JWT_TOKEN requires DAYTONA_ORGANIZATION_ID")
	}
	if err := validateNativeCredentialDestination(cfg); err != nil {
		return daytonaAuth{}, err
	}
	return auth, nil
}

func daytonaAPIURL(cfg core.Config, auth daytonaAuth) string {
	configured := strings.TrimSpace(cfg.Daytona.APIURL)
	if configured != "" && configured != defaultDaytonaAPIURL {
		return strings.TrimRight(configured, "/")
	}
	if auth.APIURL != "" {
		return strings.TrimRight(auth.APIURL, "/")
	}
	return strings.TrimRight(core.Blank(configured, defaultDaytonaAPIURL), "/")
}

func daytonaHTTPClient(source *http.Client, endpoint string) (*http.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid Daytona HTTP endpoint")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return nil, fmt.Errorf("Daytona HTTP endpoint must use HTTPS except for loopback tests")
	}
	if source == nil {
		source = http.DefaultClient
	}
	return shared.SecureHTTPClient(source, u, func(*url.URL) error {
		return fmt.Errorf("daytona refused cross-origin HTTP redirect")
	}), nil
}

// The adapter owns allocation and HTTP policy; SDK services own only toolbox operations.
func newDaytonaToolboxSandbox(cfg core.Config, rt core.Runtime, sandbox *daytona.Sandbox) (*sdkdaytona.Sandbox, error) {
	headers, err := daytonaToolboxHeaders(cfg)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(sandbox.GetToolboxProxyUrl(), "/") + "/" + url.PathEscape(sandbox.GetId())
	httpClient, err := daytonaHTTPClient(rt.HTTP, endpoint)
	if err != nil {
		return nil, err
	}
	toolboxConfig := toolbox.NewConfiguration()
	toolboxConfig.Servers = toolbox.ServerConfigurations{{URL: endpoint}}
	toolboxConfig.HTTPClient = httpClient
	for key, value := range headers {
		toolboxConfig.AddDefaultHeader(key, value)
	}
	return sdkdaytona.NewSandbox(nil, toolbox.NewAPIClient(toolboxConfig), sandbox, sdktypes.CodeLanguagePython), nil
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
		URL   string `json:"url"`
		Key   string `json:"key"`
		Token *struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   string `json:"expiresAt"`
		} `json:"token"`
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
	if dir := os.Getenv("DAYTONA_CONFIG_DIR"); dir != "" {
		// The CLI override selects one profile store, never a fallback account.
		return []string{filepath.Join(dir, "config.json")}
	}
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
	activeProfile := strings.TrimSpace(config.ActiveProfile)
	var selected *daytonaCLIProfile
	for i := range config.Profiles {
		profile := &config.Profiles[i]
		if activeProfile == "" || strings.TrimSpace(profile.ID) == activeProfile || strings.TrimSpace(profile.Name) == activeProfile {
			selected = profile
			break
		}
	}
	if selected == nil {
		if activeProfile != "" {
			return daytonaAuth{}, fmt.Errorf("daytona CLI active profile %q was not found", activeProfile)
		}
		if len(config.Profiles) > 0 {
			selected = &config.Profiles[0]
		}
	}
	if selected == nil {
		return daytonaAuth{}, nil
	}
	auth := daytonaAuth{
		APIKey:         strings.TrimSpace(selected.API.Key),
		OrganizationID: strings.TrimSpace(selected.ActiveOrganizationID),
		APIURL:         strings.TrimSpace(selected.API.URL),
	}
	if auth.APIKey == "" && selected.API.Token != nil {
		token := selected.API.Token
		expiresAt, err := time.Parse(time.RFC3339, token.ExpiresAt)
		if err != nil || !time.Now().Before(expiresAt) {
			return daytonaAuth{}, fmt.Errorf("daytona CLI OAuth token is expired or has no valid expiry; run 'daytona login' to reauthenticate")
		}
		// Refresh and profile writes belong to the Daytona CLI, not this reader.
		auth.JWTToken = strings.TrimSpace(token.AccessToken)
	}
	return auth, nil
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
	return out, c.redactError(err)
}

func (c *daytonaSDKClient) GetSandbox(ctx context.Context, id string) (*daytona.Sandbox, error) {
	req := c.api.SandboxAPI.GetSandbox(c.ctx(ctx), id).Verbose(true)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	out, _, err := req.Execute()
	return out, c.redactError(err)
}

func (c *daytonaSDKClient) ListCrabboxSandboxes(ctx context.Context) ([]daytona.Sandbox, error) {
	filter, _ := json.Marshal(map[string]string{"crabbox": "true"})
	var all []daytona.Sandbox
	var cursor string
	for {
		req := c.api.SandboxAPI.ListSandboxes(c.ctx(ctx)).Limit(100).Labels(string(filter))
		if cursor != "" {
			req = req.Cursor(cursor)
		}
		if c.orgID != "" {
			req = req.XDaytonaOrganizationID(c.orgID)
		}
		out, _, err := req.Execute()
		if err != nil {
			return nil, c.redactError(err)
		}
		if out == nil {
			return all, nil
		}
		for _, item := range out.GetItems() {
			all = append(all, daytonaSandboxFromListItem(item))
		}
		nextCursor := strings.TrimSpace(out.GetNextCursor())
		if nextCursor == "" {
			return all, nil
		}
		cursor = nextCursor
	}
}

func daytonaSandboxFromListItem(item daytona.SandboxListItem) daytona.Sandbox {
	sandbox := daytona.Sandbox{}
	sandbox.SetId(item.GetId())
	sandbox.SetOrganizationId(item.GetOrganizationId())
	sandbox.SetName(item.GetName())
	sandbox.SetTarget(item.GetTarget())
	sandbox.SetUser(item.GetUser())
	sandbox.SetPublic(item.GetPublic())
	sandbox.SetCpu(item.GetCpu())
	sandbox.SetGpu(item.GetGpu())
	sandbox.SetMemory(item.GetMemory())
	sandbox.SetDisk(item.GetDisk())
	sandbox.SetLabels(item.GetLabels())
	sandbox.SetToolboxProxyUrl(item.GetToolboxProxyUrl())
	if state, ok := item.GetStateOk(); ok && state != nil {
		sandbox.SetState(*state)
	}
	if desiredState, ok := item.GetDesiredStateOk(); ok && desiredState != nil {
		sandbox.SetDesiredState(*desiredState)
	}
	if snapshot, ok := item.GetSnapshotOk(); ok && snapshot != nil {
		sandbox.SetSnapshot(*snapshot)
	}
	if errorReason, ok := item.GetErrorReasonOk(); ok && errorReason != nil {
		sandbox.SetErrorReason(*errorReason)
	}
	if recoverable, ok := item.GetRecoverableOk(); ok && recoverable != nil {
		sandbox.SetRecoverable(*recoverable)
	}
	if backupState, ok := item.GetBackupStateOk(); ok && backupState != nil {
		sandbox.SetBackupState(*backupState)
	}
	if autoStopInterval, ok := item.GetAutoStopIntervalOk(); ok && autoStopInterval != nil {
		sandbox.SetAutoStopInterval(*autoStopInterval)
	}
	if autoArchiveInterval, ok := item.GetAutoArchiveIntervalOk(); ok && autoArchiveInterval != nil {
		sandbox.SetAutoArchiveInterval(*autoArchiveInterval)
	}
	if autoDeleteInterval, ok := item.GetAutoDeleteIntervalOk(); ok && autoDeleteInterval != nil {
		sandbox.SetAutoDeleteInterval(*autoDeleteInterval)
	}
	if createdAt, ok := item.GetCreatedAtOk(); ok && createdAt != nil {
		sandbox.SetCreatedAt(*createdAt)
	}
	if updatedAt, ok := item.GetUpdatedAtOk(); ok && updatedAt != nil {
		sandbox.SetUpdatedAt(*updatedAt)
	}
	if lastActivityAt, ok := item.GetLastActivityAtOk(); ok && lastActivityAt != nil {
		sandbox.SetLastActivityAt(*lastActivityAt)
	}
	if daemonVersion, ok := item.GetDaemonVersionOk(); ok && daemonVersion != nil {
		sandbox.SetDaemonVersion(*daemonVersion)
	}
	if runnerID, ok := item.GetRunnerIdOk(); ok && runnerID != nil {
		sandbox.SetRunnerId(*runnerID)
	}
	return sandbox
}

func (c *daytonaSDKClient) StartSandbox(ctx context.Context, id string) (*daytona.Sandbox, error) {
	req := c.api.SandboxAPI.StartSandbox(c.ctx(ctx), id)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	out, _, err := req.Execute()
	return out, c.redactError(err)
}

func (c *daytonaSDKClient) DeleteSandbox(ctx context.Context, id string) error {
	_, err := c.requestSandboxDeletion(ctx, id)
	return err
}

func (c *daytonaSDKClient) requestSandboxDeletion(ctx context.Context, id string) (*daytona.Sandbox, error) {
	req := c.api.SandboxAPI.DeleteSandbox(c.ctx(ctx), id)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	out, _, err := req.Execute()
	return out, c.redactError(err)
}

func (c *daytonaSDKClient) fixedSelection() (string, string) { return c.apiURL, c.orgID }

// This released endpoint reads the sandbox table directly. Do not substitute
// the search-index list or filter mutable labels: neither can attest absence.
func (c *daytonaSDKClient) findPendingDeletion(ctx context.Context, id string) (*daytona.Sandbox, error) {
	const limit int64 = 100
	previousTotal := int64(-1)
	seen := map[string]bool{}
	for page := int64(1); ; page++ {
		if float64(float32(page)) != float64(page) {
			return nil, core.Exit(4, "Daytona deletion inventory page is not representable by the SDK")
		}
		req := c.api.SandboxAPI.ListSandboxesPaginatedDeprecated(c.ctx(ctx)).Id(id).IncludeErroredDeleted(true).Page(float32(page)).Limit(float32(limit))
		if c.orgID != "" {
			req = req.XDaytonaOrganizationID(c.orgID)
		}
		out, response, err := req.Execute()
		if err != nil {
			return nil, c.redactError(err)
		}
		if out == nil || out.Items == nil || response == nil || response.Body == nil {
			return nil, core.Exit(4, "Daytona deletion database inventory response is incomplete")
		}
		// The SDK coerces null numeric fields to zero. Validate its retained
		// response body so null/rounded metadata cannot establish empty custody.
		var metadata struct {
			Total      *int64 `json:"total"`
			Page       *int64 `json:"page"`
			TotalPages *int64 `json:"totalPages"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&metadata)
		_ = response.Body.Close()
		if decodeErr != nil || metadata.Total == nil || metadata.Page == nil || metadata.TotalPages == nil {
			return nil, core.Exit(4, "Daytona deletion database inventory metadata is incomplete or invalid")
		}
		total, returnedPage, totalPages := *metadata.Total, *metadata.Page, *metadata.TotalPages
		expectedPages := total / limit
		if total%limit != 0 {
			expectedPages++
		}
		if total < 0 || returnedPage != page || totalPages != expectedPages ||
			(page > totalPages && !(page == 1 && total == 0)) || (previousTotal >= 0 && total != previousTotal) {
			return nil, core.Exit(4, "Daytona deletion database inventory pagination is inconsistent")
		}
		expectedItems := min(limit, total-(page-1)*limit)
		if int64(len(out.Items)) != expectedItems {
			return nil, core.Exit(4, "Daytona deletion database inventory page is incomplete")
		}
		previousTotal = total
		for _, item := range out.Items {
			if item.GetId() == "" || seen[item.GetId()] {
				return nil, core.Exit(4, "Daytona deletion database inventory contains an invalid or repeated resource")
			}
			seen[item.GetId()] = true
			if item.GetId() == id {
				return &item, nil
			}
		}
		if page >= totalPages {
			return nil, nil
		}
	}
}

func (c *daytonaSDKClient) attestDeletionOrganization(ctx context.Context, expected string) error {
	if expected == "" || c.orgID != "" && c.orgID != expected {
		return core.Exit(4, "Daytona deletion organization differs from the selected organization")
	}
	_, organization, err := c.fixedOrganization(ctx, false)
	if err != nil {
		return err
	}
	if organization != expected {
		return core.Exit(4, "Daytona deletion organization could not be attested")
	}
	return nil
}

func (c *daytonaSDKClient) ReplaceLabels(ctx context.Context, id string, labels map[string]string) error {
	req := c.api.SandboxAPI.ReplaceLabels(c.ctx(ctx), id).SandboxLabels(*daytona.NewSandboxLabels(labels))
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	_, _, err := req.Execute()
	return c.redactError(err)
}

func (c *daytonaSDKClient) UpdateLastActivity(ctx context.Context, id string) error {
	req := c.api.SandboxAPI.UpdateLastActivity(c.ctx(ctx), id)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	_, err := req.Execute()
	return c.redactError(err)
}

func (c *daytonaSDKClient) SetAutoStopInterval(ctx context.Context, id string, interval time.Duration) error {
	req := c.api.SandboxAPI.SetAutostopInterval(c.ctx(ctx), id, float32(core.DurationMinutesCeil(interval)))
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	_, _, err := req.Execute()
	return c.redactError(err)
}

func (c *daytonaSDKClient) CreateSSHAccess(ctx context.Context, id string, ttl time.Duration) (daytonaSSHAccess, error) {
	req := c.api.SandboxAPI.CreateSshAccess(c.ctx(ctx), id).ExpiresInMinutes(float32(core.DurationMinutesCeil(ttl)))
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	out, _, err := req.Execute()
	if err != nil {
		return daytonaSSHAccess{}, c.redactError(err)
	}
	if out == nil || out.GetToken() == "" {
		return daytonaSSHAccess{}, fmt.Errorf("daytona ssh access response missing token")
	}
	return daytonaSSHAccess{Token: out.GetToken(), Command: out.GetSshCommand()}, nil
}

func (c *daytonaSDKClient) redactError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *daytona.GenericOpenAPIError
	if errors.As(err, &apiErr) {
		return &redactedDaytonaAPIError{
			err:     err,
			message: redactDaytonaSecrets(apiErr.Error(), c.token),
			body:    []byte(redactDaytonaSecrets(string(apiErr.Body()), c.token)),
		}
	}
	return err
}

type redactedDaytonaAPIError struct {
	err     error
	message string
	body    []byte
}

func (e *redactedDaytonaAPIError) Error() string {
	return e.message
}

func (e *redactedDaytonaAPIError) Body() []byte {
	return e.body
}

func (e *redactedDaytonaAPIError) Unwrap() error {
	return e.err
}

func daytonaError(action string, err error) error {
	if err == nil {
		return nil
	}
	var redactedAPIError *redactedDaytonaAPIError
	if errors.As(err, &redactedAPIError) {
		body := strings.TrimSpace(redactDaytonaSecrets(core.SummarizeJSON(redactedAPIError.Body())))
		if body != "" {
			return fmt.Errorf("daytona %s: %s: %s", action, redactDaytonaSecrets(redactedAPIError.Error()), body)
		}
		return fmt.Errorf("daytona %s: %s", action, redactDaytonaSecrets(redactedAPIError.Error()))
	}
	var apiErr *daytona.GenericOpenAPIError
	if errors.As(err, &apiErr) {
		body := strings.TrimSpace(redactDaytonaSecrets(core.SummarizeJSON(apiErr.Body())))
		if body != "" {
			return fmt.Errorf("daytona %s: %s: %s", action, redactDaytonaSecrets(apiErr.Error()), body)
		}
	}
	return fmt.Errorf("daytona %s: %w", action, err)
}

var daytonaBearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[^\s"',;\\]+`)

func redactDaytonaSecrets(value string, secrets ...string) string {
	redacted := value
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, daytonaTokenRedacted)
		}
	}
	redacted = daytonaBearerPattern.ReplaceAllString(redacted, "Bearer "+daytonaTokenRedacted)
	for _, key := range []string{"authorization", "apiKey", "api_key", "accessToken", "access_token", "jwtToken", "jwt_token", "token"} {
		redacted = redactDaytonaJSONSecretField(redacted, key)
	}
	return redacted
}

func redactDaytonaJSONSecretField(value, key string) string {
	pattern := regexp.MustCompile(`(?i)"` + regexp.QuoteMeta(key) + `"\s*:\s*"[^"]*"`)
	return pattern.ReplaceAllStringFunc(value, func(match string) string {
		colon := strings.Index(match, ":")
		if colon < 0 {
			return match
		}
		return match[:colon+1] + `"` + daytonaTokenRedacted + `"`
	})
}

func daytonaIsNotFoundError(err error) bool {
	var apiErr *daytona.GenericOpenAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(apiErr.Error()), "404")
}
