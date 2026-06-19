package scaleway

import (
	"errors"
	"fmt"
	"os"
	"strings"

	iam "github.com/scaleway/scaleway-sdk-go/api/iam/v1alpha1"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	marketplace "github.com/scaleway/scaleway-sdk-go/api/marketplace/v2"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
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

	opts := []scw.ClientOption{scw.WithProfile(profile)}
	if rt.HTTP != nil {
		opts = append(opts, scw.WithHTTPClient(rt.HTTP))
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
