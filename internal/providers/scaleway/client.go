package scaleway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	iam "github.com/scaleway/scaleway-sdk-go/api/iam/v1alpha1"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	marketplace "github.com/scaleway/scaleway-sdk-go/api/marketplace/v2"
	scwlogger "github.com/scaleway/scaleway-sdk-go/logger"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
)

const defaultScalewayAPIURL = "https://api.scaleway.com"

const (
	scalewayRedirectMarkerHeader = "X-Crabbox-Scaleway-Redirect"
	scalewaySafeRedirectLocation = "/.crabbox-refused-redirect"
	scalewayRedirectCrossOrigin  = "cross-origin"
	scalewayRedirectInvalid      = "invalid"
	scalewayRedirectLimit        = "limit"
)

var (
	errScalewayCrossOriginRedirect = errors.New("scaleway refused cross-origin redirect")
	errScalewayInvalidRedirect     = errors.New("scaleway refused invalid redirect")
	errScalewayRedirectLimit       = errors.New("scaleway redirect stopped after 10 redirects")
)

type Client interface {
	Instance() InstanceAPI
	IAM() IAMAPI
	Marketplace() MarketplaceAPI
	ProjectID() string
	OrganizationID() string
	Region() string
	Zone() string
}

type InstanceAPI interface {
	ListServers(req *instance.ListServersRequest, opts ...scw.RequestOption) (*instance.ListServersResponse, error)
	GetServer(req *instance.GetServerRequest, opts ...scw.RequestOption) (*instance.GetServerResponse, error)
	CreateServer(req *instance.CreateServerRequest, opts ...scw.RequestOption) (*instance.CreateServerResponse, error)
	UpdateServer(req *instance.UpdateServerRequest, opts ...scw.RequestOption) (*instance.UpdateServerResponse, error)
	DeleteServer(req *instance.DeleteServerRequest, opts ...scw.RequestOption) error
	SetServerUserData(req *instance.SetServerUserDataRequest, opts ...scw.RequestOption) error
	ServerAction(req *instance.ServerActionRequest, opts ...scw.RequestOption) (*instance.ServerActionResponse, error)
}

type IAMAPI interface {
	ListSSHKeys(req *iam.ListSSHKeysRequest, opts ...scw.RequestOption) (*iam.ListSSHKeysResponse, error)
	GetSSHKey(req *iam.GetSSHKeyRequest, opts ...scw.RequestOption) (*iam.SSHKey, error)
	CreateSSHKey(req *iam.CreateSSHKeyRequest, opts ...scw.RequestOption) (*iam.SSHKey, error)
	DeleteSSHKey(req *iam.DeleteSSHKeyRequest, opts ...scw.RequestOption) error
}

type MarketplaceAPI interface {
	GetLocalImageByLabel(req *marketplace.GetLocalImageByLabelRequest, opts ...scw.RequestOption) (*marketplace.LocalImage, error)
}

type sdkClient struct {
	client         *scw.Client
	instanceAPI    InstanceAPI
	iamAPI         IAMAPI
	marketplaceAPI MarketplaceAPI
	projectID      string
	organizationID string
	region         string
	zone           string
}

func (c *sdkClient) Instance() InstanceAPI       { return c.instanceAPI }
func (c *sdkClient) IAM() IAMAPI                 { return c.iamAPI }
func (c *sdkClient) Marketplace() MarketplaceAPI { return c.marketplaceAPI }
func (c *sdkClient) ProjectID() string           { return c.projectID }
func (c *sdkClient) OrganizationID() string      { return c.organizationID }
func (c *sdkClient) Region() string              { return c.region }
func (c *sdkClient) Zone() string                { return c.zone }

func newClient(cfg core.Config, rt core.Runtime) (Client, error) {
	profile, err := scalewayProfileFromSDKConfig()
	if err != nil {
		return nil, err
	}
	envProfile := scw.LoadEnvProfile()
	profile = scw.MergeProfiles(profile, envProfile)
	applyCrabboxScalewayOverrides(profile, cfg)
	applyScalewayLocationDefaults(profile)

	accessKey := stringPtrValue(profile.AccessKey)
	secretKey := stringPtrValue(profile.SecretKey)
	switch {
	case accessKey == "" && secretKey == "":
		return nil, core.Exit(3, "SCW_ACCESS_KEY and SCW_SECRET_KEY or Scaleway SDK config credentials are required")
	case accessKey == "":
		return nil, core.Exit(3, "SCW_ACCESS_KEY or Scaleway SDK access_key is required")
	case secretKey == "":
		return nil, core.Exit(3, "SCW_SECRET_KEY or Scaleway SDK secret_key is required")
	}

	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = defaultScalewayHTTPClient()
	}
	trustedAPI, _ := url.Parse(scalewayAPIURL(profile))
	opts := []scw.ClientOption{
		scw.WithProfile(profile),
		scw.WithHTTPClient(secureScalewayHTTPClient(httpClient, trustedAPI)),
	}
	client, err := scw.NewClient(opts...)
	if err != nil {
		return nil, core.Exit(3, "Scaleway SDK client configuration failed: %s", sanitizeSDKError(err, accessKey, secretKey))
	}
	out := &sdkClient{
		client:         client,
		instanceAPI:    instance.NewAPI(client),
		iamAPI:         iam.NewAPI(client),
		marketplaceAPI: marketplace.NewAPI(client),
		projectID:      stringPtrValue(profile.DefaultProjectID),
		organizationID: stringPtrValue(profile.DefaultOrganizationID),
		region:         stringPtrValue(profile.DefaultRegion),
		zone:           stringPtrValue(profile.DefaultZone),
	}
	if out.projectID == "" {
		return nil, core.Exit(3, "SCW_DEFAULT_PROJECT_ID, CRABBOX_SCALEWAY_PROJECT_ID, or Scaleway SDK default_project_id is required")
	}
	return out, nil
}

func scalewayAPIURL(profile *scw.Profile) string {
	if profile != nil && profile.APIURL != nil {
		return *profile.APIURL
	}
	return defaultScalewayAPIURL
}

func defaultScalewayHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: http.DefaultTransport.(*http.Transport).Clone(),
	}
}

type scalewayRedirectHopKey struct{}

type scalewayRedirectTransport struct {
	base                http.RoundTripper
	trusted             *url.URL
	enforceDefaultLimit bool
}

func (t *scalewayRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || !isScalewayRedirect(resp.StatusCode) {
		return resp, err
	}
	resp.Header = resp.Header.Clone()
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	resp.Header.Del(scalewayRedirectMarkerHeader)
	location := resp.Header.Get("Location")
	if location == "" {
		return resp, nil
	}
	target, parseErr := req.URL.Parse(location)
	marker := ""
	switch {
	case parseErr != nil:
		marker = scalewayRedirectInvalid
	case !sameScalewayOrigin(t.trusted, target):
		marker = scalewayRedirectCrossOrigin
	case t.enforceDefaultLimit && scalewayRedirectHop(req.Context()) >= 9:
		marker = scalewayRedirectLimit
	}
	if marker != "" {
		// Sanitize before the SDK's optional response logger sees the
		// rejected Location, then let CheckRedirect return the sentinel.
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		resp.Body = io.NopCloser(strings.NewReader(""))
		resp.ContentLength = 0
		resp.Header.Del("Content-Length")
		resp.Header.Set(scalewayRedirectMarkerHeader, marker)
		resp.Header.Set("Location", scalewaySafeRedirectLocation)
	}
	return resp, nil
}

func (t *scalewayRedirectTransport) SetInsecureTransport() {
	if transport, ok := t.base.(interface{ SetInsecureTransport() }); ok {
		transport.SetInsecureTransport()
		return
	}
	transport, ok := t.base.(*http.Transport)
	if !ok {
		scwlogger.Warningf("client: cannot use insecure mode with Transport client of type %T", t.base)
		return
	}
	transport = transport.Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	// Explicit SDK profile compatibility for private test/API endpoints.
	transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec
	t.base = transport
}

func secureScalewayHTTPClient(source *http.Client, trusted *url.URL) *http.Client {
	client := *source
	originalCheckRedirect := source.CheckRedirect
	guard := &scalewayRedirectTransport{
		base:                source.Transport,
		trusted:             trusted,
		enforceDefaultLimit: originalCheckRedirect == nil,
	}
	if guard.base == nil {
		guard.base = http.DefaultTransport
	}
	client.Transport = guard
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.Response != nil {
			switch req.Response.Header.Get(scalewayRedirectMarkerHeader) {
			case scalewayRedirectCrossOrigin:
				return errScalewayCrossOriginRedirect
			case scalewayRedirectInvalid:
				return errScalewayInvalidRedirect
			case scalewayRedirectLimit:
				return errScalewayRedirectLimit
			}
		}
		if !sameScalewayOrigin(trusted, req.URL) {
			return errScalewayCrossOriginRedirect
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errScalewayRedirectLimit
		}
		*req = *req.WithContext(context.WithValue(req.Context(), scalewayRedirectHopKey{}, len(via)))
		return nil
	}
	return &client
}

func scalewayRedirectHop(ctx context.Context) int {
	hop, _ := ctx.Value(scalewayRedirectHopKey{}).(int)
	return hop
}

func isScalewayRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func sameScalewayOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveScalewayPort(a) == effectiveScalewayPort(b)
}

func effectiveScalewayPort(value *url.URL) string {
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

func scalewayProfileFromSDKConfig() (*scw.Profile, error) {
	cfg, err := scw.LoadConfig()
	if err != nil {
		var notFound *scw.ConfigFileNotFoundError
		if errors.As(err, &notFound) {
			return &scw.Profile{}, nil
		}
		return nil, core.Exit(3, "Scaleway SDK config load failed: %s", sanitizeSDKError(err))
	}
	profile, err := cfg.GetActiveProfile()
	if err != nil {
		return nil, core.Exit(3, "Scaleway SDK active profile load failed: %s", sanitizeSDKError(err))
	}
	return profile, nil
}

func applyCrabboxScalewayOverrides(profile *scw.Profile, cfg core.Config) {
	if value := strings.TrimSpace(cfg.Scaleway.ProjectID); value != "" {
		profile.DefaultProjectID = scw.StringPtr(value)
	}
	if value := strings.TrimSpace(cfg.Scaleway.OrganizationID); value != "" {
		profile.DefaultOrganizationID = scw.StringPtr(value)
	}
	if value := strings.TrimSpace(cfg.Scaleway.Region); value != "" && core.ScalewayRegionWasExplicit(cfg) {
		profile.DefaultRegion = scw.StringPtr(value)
	}
	if value := strings.TrimSpace(cfg.Scaleway.Zone); value != "" && core.ScalewayZoneWasExplicit(cfg) {
		profile.DefaultZone = scw.StringPtr(value)
	}
}

func applyScalewayLocationDefaults(profile *scw.Profile) {
	if stringPtrValue(profile.DefaultRegion) == "" {
		profile.DefaultRegion = scw.StringPtr(defaultRegion)
	}
	if stringPtrValue(profile.DefaultZone) == "" {
		profile.DefaultZone = scw.StringPtr(defaultZone)
	}
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func sanitizeSDKError(err error, sensitiveValues ...string) string {
	message := fmt.Sprint(err)
	for _, value := range sensitiveValues {
		if value = strings.TrimSpace(value); value != "" {
			message = strings.ReplaceAll(message, value, "<redacted>")
		}
	}
	for _, env := range []string{
		"SCW_ACCESS_KEY",
		"SCW_SECRET_KEY",
		"SCW_DEFAULT_ORGANIZATION_ID",
		"SCW_DEFAULT_PROJECT_ID",
		"SCW_DEFAULT_REGION",
		"SCW_DEFAULT_ZONE",
	} {
		if value := strings.TrimSpace(os.Getenv(env)); value != "" {
			message = strings.ReplaceAll(message, value, "<redacted>")
		}
	}
	return message
}
