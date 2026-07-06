package cli

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type credentialValueSource uint8

const (
	credentialSourceUnknown credentialValueSource = iota
	credentialSourceTrustedFile
	credentialSourceRepository
	credentialSourceEnvironment
	credentialSourceFlag
)

type credentialDestinationProvenance struct {
	coordinator           credentialValueSource
	coordToken            credentialValueSource
	coordTokenCommand     credentialValueSource
	coordAdminToken       credentialValueSource
	accessClientID        credentialValueSource
	accessClientSecret    credentialValueSource
	accessToken           credentialValueSource
	azSessionsEndpoint    credentialValueSource
	proxmoxAPIURL         credentialValueSource
	proxmoxTokenID        credentialValueSource
	proxmoxTokenSecret    credentialValueSource
	proxmoxInsecureTLS    credentialValueSource
	morphAPIURL           credentialValueSource
	morphAPIKey           credentialValueSource
	morphSSHGatewayHost   credentialValueSource
	daytonaAPIURL         credentialValueSource
	daytonaAPIKey         credentialValueSource
	daytonaJWTToken       credentialValueSource
	daytonaSSHGateway     credentialValueSource
	e2bAPIURL             credentialValueSource
	e2bDomain             credentialValueSource
	e2bAPIKey             credentialValueSource
	railwayAPIURL         credentialValueSource
	railwayAPIToken       credentialValueSource
	fastAPICloudAPIURL    credentialValueSource
	fastAPICloudToken     credentialValueSource
	runpodAPIURL          credentialValueSource
	runpodAPIKey          credentialValueSource
	vastAPIURL            credentialValueSource
	vastAPIKey            credentialValueSource
	isloBaseURL           credentialValueSource
	isloAPIKey            credentialValueSource
	tenkiEndpoint         credentialValueSource
	tenkiGateway          credentialValueSource
	tensorlakeAPIURL      credentialValueSource
	tensorlakeAPIKey      credentialValueSource
	upstashBoxBaseURL     credentialValueSource
	upstashBoxAPIKey      credentialValueSource
	smolvmBaseURL         credentialValueSource
	smolvmAPIKey          credentialValueSource
	asciiBoxBaseURL       credentialValueSource
	asciiBoxAPIKey        credentialValueSource
	cloudflareAPIURL      credentialValueSource
	cloudflareToken       credentialValueSource
	nomadAddress          credentialValueSource
	nomadTokenEnv         credentialValueSource
	semaphoreHost         credentialValueSource
	semaphoreToken        credentialValueSource
	spritesAPIURL         credentialValueSource
	spritesToken          credentialValueSource
	parallelsHost         credentialValueSource
	parallelsHostKey      credentialValueSource
	staticHost            credentialValueSource
	sshKey                credentialValueSource
	exeDevControlHost     credentialValueSource
	externalConfig        credentialValueSource
	externalResource      credentialValueSource
	externalSSHConnection credentialValueSource
	externalSSHHost       credentialValueSource
	externalSSHProxy      credentialValueSource
	externalSSHAllowEnv   credentialValueSource
	externalSSHOutput     credentialValueSource
	externalRouting       credentialValueSource
	externalApproved      externalCredentialApproval
	repositoryRoot        string
}

type externalCredentialApproval struct {
	resource       string
	host           string
	proxy          string
	allowEnv       bool
	envSSH         ExternalSSHConnectionConfig
	providerOutput bool
	outputContract [32]byte
}

type sourcedCredential struct {
	value  string
	source credentialValueSource
}

func credentialSourceForFile(trusted bool) credentialValueSource {
	if trusted {
		return credentialSourceTrustedFile
	}
	return credentialSourceRepository
}

func credentialDestinationSource(value, approved string, source credentialValueSource) credentialValueSource {
	if strings.TrimSpace(value) == "" {
		return credentialSourceUnknown
	}
	if source == credentialSourceRepository && approved != "" && value == approved {
		return credentialSourceTrustedFile
	}
	return source
}

func firstNonEmptyEnv(names ...string) (string, bool) {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value, true
		}
	}
	return "", false
}

func markCoordinatorDestinationExplicit(cfg *Config) {
	cfg.credentialProvenance.coordinator = credentialSourceFlag
}

func markCredentialDestinationFlagSources(cfg *Config, fs *flag.FlagSet) {
	if fs == nil {
		return
	}
	provenance := &cfg.credentialProvenance
	if flagWasSet(fs, "proxmox-api-url") {
		provenance.proxmoxAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "proxmox-insecure-tls") {
		provenance.proxmoxInsecureTLS = credentialSourceFlag
	}
	if flagWasSet(fs, "morph-api-url") {
		provenance.morphAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "morph-ssh-gateway-host") {
		provenance.morphSSHGatewayHost = credentialSourceFlag
	}
	if flagWasSet(fs, "daytona-api-url") {
		provenance.daytonaAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "daytona-ssh-gateway-host") {
		provenance.daytonaSSHGateway = credentialSourceFlag
	}
	if flagWasSet(fs, "e2b-api-url") {
		provenance.e2bAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "e2b-domain") {
		provenance.e2bDomain = credentialSourceFlag
	}
	if flagWasSet(fs, "railway-url") {
		provenance.railwayAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "fastapi-cloud-url") {
		provenance.fastAPICloudAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "runpod-url") {
		provenance.runpodAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "vast-api-url") {
		provenance.vastAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "islo-base-url") {
		provenance.isloBaseURL = credentialSourceFlag
	}
	if flagWasSet(fs, "tenki-endpoint") {
		provenance.tenkiEndpoint = credentialSourceFlag
	}
	if flagWasSet(fs, "tenki-gateway") {
		provenance.tenkiGateway = credentialSourceFlag
	}
	if flagWasSet(fs, "tensorlake-api-url") {
		provenance.tensorlakeAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "upstash-box-base-url") {
		provenance.upstashBoxBaseURL = credentialSourceFlag
	}
	if flagWasSet(fs, "smolvm-base-url") {
		provenance.smolvmBaseURL = credentialSourceFlag
	}
	if flagWasSet(fs, "ascii-box-base-url") {
		provenance.asciiBoxBaseURL = credentialSourceFlag
	}
	if flagWasSet(fs, "cloudflare-url") {
		provenance.cloudflareAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "nomad-address") {
		provenance.nomadAddress = credentialSourceFlag
	}
	if flagWasSet(fs, "nomad-token-env") {
		provenance.nomadTokenEnv = credentialSourceFlag
	}
	if flagWasSet(fs, "semaphore-host") {
		provenance.semaphoreHost = credentialSourceFlag
	}
	if flagWasSet(fs, "sprites-api-url") {
		provenance.spritesAPIURL = credentialSourceFlag
	}
	if flagWasSet(fs, "azure-dynamic-sessions-endpoint") {
		provenance.azSessionsEndpoint = credentialSourceFlag
	}
	if flagWasSet(fs, "parallels-host") {
		provenance.parallelsHost = credentialSourceFlag
	}
	if flagWasSet(fs, "parallels-host-key") {
		provenance.parallelsHostKey = credentialSourceFlag
	}
	if flagWasSet(fs, "static-host") {
		provenance.staticHost = credentialSourceFlag
	}
	if flagWasSet(fs, "exe-dev-control-host") {
		provenance.exeDevControlHost = credentialSourceFlag
	}
	if flagWasSet(fs, "external-routing-file") {
		provenance.externalRouting = credentialSourceFlag
	}
	if flagWasSet(fs, "external-config-json") {
		provenance.externalConfig = credentialSourceFlag
	}
}

func validateCoordinatorCredentialDestination(cfg Config) error {
	if cfg.credentialProvenance.coordinator != credentialSourceRepository {
		return nil
	}
	provenance := cfg.credentialProvenance
	coordTokenSource := provenance.coordToken
	if coordTokenSource == credentialSourceUnknown && cfg.CoordToken != "" && cfg.CoordToken == cfg.CoordAdminToken {
		coordTokenSource = provenance.coordAdminToken
	}
	credentials := []sourcedCredential{
		{cfg.CoordToken, coordTokenSource},
		{strings.Join(cfg.CoordTokenCommand, "\x00"), provenance.coordTokenCommand},
		{cfg.CoordAdminToken, provenance.coordAdminToken},
		{cfg.Access.ClientSecret, provenance.accessClientSecret},
		{cfg.Access.Token, provenance.accessToken},
	}
	if inheritedCredential(credentials...) {
		return exit(2, "repository-configured broker.url cannot be combined with inherited coordinator credentials; set CRABBOX_COORDINATOR or pass an explicit coordinator URL to approve the credential destination")
	}
	return nil
}

func validateProviderCredentialDestination(cfg Config) error {
	provenance := cfg.credentialProvenance
	providerName := normalizeProviderName(cfg.Provider)
	if provider, err := ProviderFor(providerName); err == nil {
		providerName = provider.Name()
	}
	switch providerName {
	case "proxmox":
		credentials := []sourcedCredential{
			{cfg.Proxmox.TokenID, provenance.proxmoxTokenID},
			{cfg.Proxmox.TokenSecret, provenance.proxmoxTokenSecret},
		}
		if provenance.proxmoxAPIURL == credentialSourceRepository && inheritedCredential(credentials...) {
			return repositoryCredentialDestinationError("proxmox", "proxmox.apiUrl", "CRABBOX_PROXMOX_API_URL or --proxmox-api-url")
		}
		if cfg.Proxmox.InsecureTLS && provenance.proxmoxInsecureTLS == credentialSourceRepository && inheritedCredential(credentials...) {
			return repositoryCredentialDestinationError("proxmox", "proxmox.insecureTLS", "CRABBOX_PROXMOX_INSECURE_TLS or --proxmox-insecure-tls")
		}
	case "morph":
		credentials := []sourcedCredential{{cfg.Morph.APIKey, provenance.morphAPIKey}}
		if provenance.morphAPIURL == credentialSourceRepository && inheritedCredential(credentials...) {
			return repositoryCredentialDestinationError("morph", "morph.apiUrl", "CRABBOX_MORPH_API_URL or --morph-api-url")
		}
		if provenance.morphSSHGatewayHost == credentialSourceRepository && inheritedCredential(credentials...) {
			return repositoryCredentialDestinationError("morph", "morph.sshGatewayHost", "CRABBOX_MORPH_SSH_GATEWAY_HOST or --morph-ssh-gateway-host")
		}
	case "daytona":
		credentials := []sourcedCredential{
			{cfg.Daytona.APIKey, provenance.daytonaAPIKey},
			{cfg.Daytona.JWTToken, provenance.daytonaJWTToken},
		}
		if provenance.daytonaAPIURL == credentialSourceRepository &&
			inheritedCredential(credentials...) {
			return repositoryCredentialDestinationError("daytona", "daytona.apiUrl", "CRABBOX_DAYTONA_API_URL or --daytona-api-url")
		}
		if provenance.daytonaSSHGateway == credentialSourceRepository &&
			inheritedCredential(credentials...) {
			return repositoryCredentialDestinationError("daytona", "daytona.sshGatewayHost", "CRABBOX_DAYTONA_SSH_GATEWAY_HOST or --daytona-ssh-gateway-host")
		}
	case "e2b":
		credentials := []sourcedCredential{{cfg.E2B.APIKey, provenance.e2bAPIKey}}
		if provenance.e2bAPIURL == credentialSourceRepository && inheritedCredential(credentials...) {
			return repositoryCredentialDestinationError("e2b", "e2b.apiUrl", "CRABBOX_E2B_API_URL or --e2b-api-url")
		}
		if provenance.e2bDomain == credentialSourceRepository && inheritedCredential(credentials...) {
			return repositoryCredentialDestinationError("e2b", "e2b.domain", "CRABBOX_E2B_DOMAIN or --e2b-domain")
		}
	case "railway":
		if provenance.railwayAPIURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.Railway.APIToken, provenance.railwayAPIToken}) {
			return repositoryCredentialDestinationError("railway", "railway.apiUrl", "CRABBOX_RAILWAY_API_URL or --railway-url")
		}
	case "fastapi-cloud":
		if provenance.fastAPICloudAPIURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.FastAPICloud.Token, provenance.fastAPICloudToken}) {
			return repositoryCredentialDestinationError("fastapi-cloud", "fastapiCloud.apiUrl", "CRABBOX_FASTAPI_CLOUD_API_URL or --fastapi-cloud-url")
		}
	case "runpod":
		if provenance.runpodAPIURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.Runpod.APIKey, provenance.runpodAPIKey}) {
			return repositoryCredentialDestinationError("runpod", "runpod.apiUrl", "CRABBOX_RUNPOD_API_URL or --runpod-url")
		}
	case "vast":
		if provenance.vastAPIURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.Vast.APIKey, provenance.vastAPIKey}) {
			return repositoryCredentialDestinationError("vast", "vast.apiUrl", "CRABBOX_VAST_API_URL or --vast-api-url")
		}
	case "islo":
		if provenance.isloBaseURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.Islo.APIKey, provenance.isloAPIKey}) {
			return repositoryCredentialDestinationError("islo", "islo.baseUrl", "CRABBOX_ISLO_BASE_URL or --islo-base-url")
		}
	case "tensorlake":
		if provenance.tensorlakeAPIURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.Tensorlake.APIKey, provenance.tensorlakeAPIKey}) {
			return repositoryCredentialDestinationError("tensorlake", "tensorlake.apiUrl", "CRABBOX_TENSORLAKE_API_URL or --tensorlake-api-url")
		}
	case "upstash-box":
		if provenance.upstashBoxBaseURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.UpstashBox.APIKey, provenance.upstashBoxAPIKey}) {
			return repositoryCredentialDestinationError("upstash-box", "upstashBox.baseUrl", "CRABBOX_UPSTASH_BOX_BASE_URL or --upstash-box-base-url")
		}
	case "smolvm":
		if provenance.smolvmBaseURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.Smolvm.APIKey, provenance.smolvmAPIKey}) {
			return repositoryCredentialDestinationError("smolvm", "smolvm.baseUrl", "CRABBOX_SMOLVM_BASE_URL or --smolvm-base-url")
		}
	case "ascii-box":
		if provenance.asciiBoxBaseURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.AsciiBox.APIKey, provenance.asciiBoxAPIKey}) {
			return repositoryCredentialDestinationError("ascii-box", "asciiBox.baseUrl", "CRABBOX_ASCII_BOX_BASE_URL or --ascii-box-base-url")
		}
	case "cloudflare":
		if provenance.cloudflareAPIURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.Cloudflare.Token, provenance.cloudflareToken}) {
			return repositoryCredentialDestinationError("cloudflare", "cloudflare.apiUrl", "CRABBOX_CLOUDFLARE_RUNNER_URL or --cloudflare-url")
		}
	case "nomad":
		if (provenance.nomadAddress == credentialSourceRepository || provenance.nomadTokenEnv == credentialSourceRepository) &&
			nomadSelectedTokenEnvHasValue(cfg) {
			return repositoryCredentialDestinationError("nomad", "nomad.address or nomad.tokenEnv", "CRABBOX_NOMAD_ADDR/CRABBOX_NOMAD_TOKEN_ENV or --nomad-address/--nomad-token-env")
		}
	case "semaphore":
		if provenance.semaphoreHost == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.Semaphore.Token, provenance.semaphoreToken}) {
			return repositoryCredentialDestinationError("semaphore", "semaphore.host", "CRABBOX_SEMAPHORE_HOST or --semaphore-host")
		}
	case "sprites":
		if provenance.spritesAPIURL == credentialSourceRepository &&
			inheritedCredential(sourcedCredential{cfg.Sprites.Token, provenance.spritesToken}) {
			return repositoryCredentialDestinationError("sprites", "sprites.apiUrl", "CRABBOX_SPRITES_API_URL or --sprites-api-url")
		}
	case "parallels":
		for _, candidate := range ParallelsCandidateConfigs(cfg) {
			candidateProvenance := candidate.credentialProvenance
			if candidateProvenance.parallelsHost == credentialSourceRepository &&
				repositorySSHDestinationUsesInheritedAuth(candidate.Parallels.HostKey, candidateProvenance.parallelsHostKey, candidateProvenance.repositoryRoot) {
				return repositoryCredentialDestinationError("parallels", "parallels.host", "CRABBOX_PARALLELS_HOST or --parallels-host")
			}
		}
	case staticProvider:
		if provenance.staticHost == credentialSourceRepository &&
			repositorySSHDestinationUsesInheritedAuth(cfg.SSHKey, provenance.sshKey, provenance.repositoryRoot) {
			return repositoryCredentialDestinationError(staticProvider, "static.host", "CRABBOX_STATIC_HOST or --static-host")
		}
	case "exe-dev":
		if provenance.exeDevControlHost == credentialSourceRepository {
			return repositoryCredentialDestinationError("exe-dev", "exeDev.controlHost", "CRABBOX_EXE_DEV_CONTROL_HOST or --exe-dev-control-host")
		}
	case "external":
		if cfg.External.Connection.SSH.TrustProviderOutput && provenance.externalSSHOutput == credentialSourceRepository {
			return repositoryCredentialDestinationError("external", "external.connection.ssh.trustProviderOutput", "the same provider-output contract in trusted user config")
		}
		if !externalDeclarativeLifecycleConfigured(cfg.External) {
			break
		}
		if cfg.External.Connection.SSH.AllowEnv && provenance.externalSSHAllowEnv == credentialSourceRepository {
			return repositoryCredentialDestinationError("external", "external.connection.ssh.allowEnv", "the same opt-in in trusted user config")
		}
		resourceSource := externalTemplateCredentialSource(
			cfg.External.Connection.ResourceName,
			provenance.externalResource,
			credentialSourceUnknown,
			provenance.externalConfig,
		)
		hostSource := provenance.externalSSHHost
		if strings.TrimSpace(cfg.External.Connection.SSH.Host) == "" {
			hostSource = resourceSource
		} else {
			hostSource = externalTemplateCredentialSource(
				cfg.External.Connection.SSH.Host, hostSource, resourceSource, provenance.externalConfig,
			)
		}
		proxySource := externalTemplateCredentialSource(
			cfg.External.Connection.SSH.ProxyCommand,
			provenance.externalSSHProxy,
			resourceSource,
			provenance.externalConfig,
		)
		if proxySource == credentialSourceRepository {
			return repositoryCredentialDestinationError("external", "external.connection.ssh.proxyCommand", "the same proxy command in trusted user config")
		}
		if hostSource == credentialSourceRepository {
			return repositoryCredentialDestinationError("external", "external.connection.ssh.host", "the same destination in trusted user config")
		}
		if externalSSHEndpointUsesRepositoryInput(
			cfg.External.Connection,
			provenance.externalApproved,
			provenance.externalSSHConnection,
			resourceSource,
			provenance.externalConfig,
		) {
			return repositoryCredentialDestinationError("external", "external.connection.ssh endpoint", "trusted endpoint templates and trusted template inputs")
		}
		if provenance.externalSSHConnection == credentialSourceRepository &&
			!externalSSHEndpointApprovalMatches(cfg.External.Connection, provenance.externalApproved) {
			return repositoryCredentialDestinationError("external", "external.connection.ssh endpoint", "the same user, host, key, port, fallback ports, and proxy settings in trusted user config")
		}
	}
	return nil
}

// ValidateProviderCredentialDestination enforces source-bound credential
// routing for provider entry points that can load configuration after the
// normal CLI validation phase.
func ValidateProviderCredentialDestination(cfg Config) error {
	return validateProviderCredentialDestination(cfg)
}

// ValidateExternalProviderSSHOutput requires a trusted, source-bound contract
// before adapter output may supply SSH coordinates directly.
func ValidateExternalProviderSSHOutput(cfg Config) error {
	if !cfg.External.Connection.SSH.TrustProviderOutput {
		return exit(2, "external provider SSH output requires external.connection.ssh.trustProviderOutput in trusted user config")
	}
	if cfg.credentialProvenance.externalSSHOutput == credentialSourceRepository {
		return repositoryCredentialDestinationError("external", "external.connection.ssh.trustProviderOutput", "the same provider-output contract in trusted user config")
	}
	return nil
}

func externalTemplateCredentialSource(value string, source, resourceSource, configSource credentialValueSource) credentialValueSource {
	if strings.Contains(value, "{{repo.") {
		return credentialSourceRepository
	}
	if configSource == credentialSourceRepository && strings.Contains(value, "{{config.") {
		return credentialSourceRepository
	}
	if resourceSource == credentialSourceRepository && strings.Contains(value, "{{resourceName}}") {
		return credentialSourceRepository
	}
	return source
}

func externalDeclarativeLifecycleConfigured(cfg ExternalConfig) bool {
	return len(cfg.Lifecycle.Acquire.Argv) > 0 || len(cfg.Lifecycle.Acquire.Steps) > 0
}

// MarkExternalRoutingCredentialSources applies the routing file's trust source
// to connection values after the External provider loads that private state.
// An unknown source is reserved for claim-bound automatic routing.
func MarkExternalRoutingCredentialSources(cfg *Config) {
	source := cfg.credentialProvenance.externalRouting
	if source == credentialSourceUnknown {
		source = credentialSourceTrustedFile
	}
	connection := cfg.External.Connection
	provenance := &cfg.credentialProvenance
	provenance.externalConfig = source
	provenance.externalResource = credentialDestinationSource(connection.ResourceName, provenance.externalApproved.resource, source)
	provenance.externalSSHConnection = source
	provenance.externalSSHHost = credentialDestinationSource(connection.SSH.Host, provenance.externalApproved.host, source)
	provenance.externalSSHProxy = credentialDestinationSource(connection.SSH.ProxyCommand, provenance.externalApproved.proxy, source)
	provenance.externalSSHAllowEnv = credentialSourceForBool(connection.SSH.AllowEnv, source)
	if source == credentialSourceRepository && connection.SSH.AllowEnv && provenance.externalApproved.allowEnv &&
		externalSSHEnvApprovalMatches(connection, provenance.externalApproved) {
		provenance.externalSSHAllowEnv = credentialSourceTrustedFile
	}
	provenance.externalSSHOutput = credentialSourceForBool(connection.SSH.TrustProviderOutput, source)
	outputContract, outputContractOK := externalProviderOutputContract(cfg.External)
	if source == credentialSourceRepository && connection.SSH.TrustProviderOutput && provenance.externalApproved.providerOutput &&
		outputContractOK && outputContract == provenance.externalApproved.outputContract {
		provenance.externalSSHOutput = credentialSourceTrustedFile
	}
}

// MarkExternalRoutingFileExplicit records an operator-selected routing path
// before the External provider loads it during flag application.
func MarkExternalRoutingFileExplicit(cfg *Config) {
	cfg.credentialProvenance.externalRouting = credentialSourceFlag
}

// MarkExternalProviderOutputFlagExplicit records that an operator changed the
// adapter contract through a trusted flag.
func MarkExternalProviderOutputFlagExplicit(cfg *Config) {
	markExternalProviderOutputExplicit(cfg, credentialSourceFlag)
}

func markExternalProviderOutputExplicit(cfg *Config, source credentialValueSource) {
	if cfg.External.Connection.SSH.TrustProviderOutput && cfg.credentialProvenance.externalSSHOutput != credentialSourceRepository {
		cfg.credentialProvenance.externalSSHOutput = source
	}
}

func credentialSourceForBool(value bool, source credentialValueSource) credentialValueSource {
	if !value {
		return credentialSourceUnknown
	}
	return source
}

func externalSSHEnvTemplatesMatch(left, right ExternalSSHConnectionConfig) bool {
	return left.User == right.User &&
		left.Host == right.Host &&
		left.Key == right.Key &&
		left.Port == right.Port &&
		slices.Equal(left.FallbackPorts, right.FallbackPorts) &&
		left.ReadyCheck == right.ReadyCheck &&
		left.ProxyCommand == right.ProxyCommand
}

func externalSSHEnvApprovalMatches(connection ExternalConnectionConfig, approval externalCredentialApproval) bool {
	if !externalSSHEnvTemplatesMatch(connection.SSH, approval.envSSH) {
		return false
	}
	if externalSSHReferencesResourceName(connection.SSH) && connection.ResourceName != approval.resource {
		return false
	}
	return true
}

func externalSSHEndpointApprovalMatches(connection ExternalConnectionConfig, approval externalCredentialApproval) bool {
	ssh := connection.SSH
	approved := approval.envSSH
	if ssh.User != approved.User ||
		ssh.Host != approved.Host ||
		ssh.Key != approved.Key ||
		ssh.Port != approved.Port ||
		!slices.Equal(ssh.FallbackPorts, approved.FallbackPorts) ||
		ssh.AuthSecret != approved.AuthSecret ||
		ssh.NoControlMaster != approved.NoControlMaster ||
		ssh.SSHConfigProxy != approved.SSHConfigProxy ||
		ssh.ProxyCommand != approved.ProxyCommand {
		return false
	}
	if externalSSHEndpointReferencesResourceName(ssh) && connection.ResourceName != approval.resource {
		return false
	}
	return true
}

func externalSSHEndpointUsesRepositoryInput(
	connection ExternalConnectionConfig,
	approval externalCredentialApproval,
	connectionSource, resourceSource, configSource credentialValueSource,
) bool {
	ssh := connection.SSH
	approved := approval.envSSH
	for _, field := range []struct {
		value    string
		approved string
	}{
		{ssh.User, approved.User},
		{ssh.Key, approved.Key},
		{ssh.Port, approved.Port},
	} {
		source := credentialDestinationSource(field.value, field.approved, connectionSource)
		if externalTemplateCredentialSource(field.value, source, resourceSource, configSource) == credentialSourceRepository {
			return true
		}
	}
	fallbackSource := connectionSource
	if fallbackSource == credentialSourceRepository && slices.Equal(ssh.FallbackPorts, approved.FallbackPorts) {
		fallbackSource = credentialSourceTrustedFile
	}
	for _, value := range ssh.FallbackPorts {
		if externalTemplateCredentialSource(value, fallbackSource, resourceSource, configSource) == credentialSourceRepository {
			return true
		}
	}
	return false
}

func externalSSHEndpointReferencesResourceName(ssh ExternalSSHConnectionConfig) bool {
	values := []string{ssh.User, ssh.Host, ssh.Key, ssh.Port, ssh.ProxyCommand}
	values = append(values, ssh.FallbackPorts...)
	for _, value := range values {
		if strings.Contains(value, "{{resourceName}}") {
			return true
		}
	}
	return false
}

func externalSSHReferencesResourceName(ssh ExternalSSHConnectionConfig) bool {
	values := []string{ssh.User, ssh.Host, ssh.Key, ssh.Port, ssh.ReadyCheck, ssh.ProxyCommand}
	values = append(values, ssh.FallbackPorts...)
	for _, value := range values {
		if strings.Contains(value, "{{resourceName}}") {
			return true
		}
	}
	return false
}

func externalProviderOutputContract(cfg ExternalConfig) ([32]byte, bool) {
	contract := struct {
		Command    string                   `json:"command,omitempty"`
		Args       []string                 `json:"args,omitempty"`
		Config     map[string]any           `json:"config,omitempty"`
		Lifecycle  ExternalLifecycleConfig  `json:"lifecycle,omitempty"`
		Connection ExternalConnectionConfig `json:"connection,omitempty"`
	}{
		Command:    cfg.Command,
		Args:       cfg.Args,
		Config:     cfg.Config,
		Lifecycle:  cfg.Lifecycle,
		Connection: cfg.Connection,
	}
	data, err := json.Marshal(contract)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(data), true
}

// ValidateNativeCredentialDestination is called only when a provider is about
// to use credentials from its native CLI store.
func ValidateNativeCredentialDestination(cfg Config, provider string) error {
	provenance := cfg.credentialProvenance
	switch normalizeProviderName(provider) {
	case "azure-dynamic-sessions":
		if provenance.azSessionsEndpoint == credentialSourceRepository {
			return repositoryCredentialDestinationError("azure-dynamic-sessions", "azureDynamicSessions.endpoint", "CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT or --azure-dynamic-sessions-endpoint")
		}
	case "daytona":
		if provenance.daytonaAPIURL == credentialSourceRepository {
			return repositoryCredentialDestinationError("daytona", "daytona.apiUrl", "CRABBOX_DAYTONA_API_URL or --daytona-api-url")
		}
		if provenance.daytonaSSHGateway == credentialSourceRepository {
			return repositoryCredentialDestinationError("daytona", "daytona.sshGatewayHost", "CRABBOX_DAYTONA_SSH_GATEWAY_HOST or --daytona-ssh-gateway-host")
		}
	case "tenki":
		if provenance.tenkiEndpoint == credentialSourceRepository {
			return repositoryCredentialDestinationError("tenki", "tenki.endpoint", "CRABBOX_TENKI_ENDPOINT or --tenki-endpoint")
		}
		if provenance.tenkiGateway == credentialSourceRepository {
			return repositoryCredentialDestinationError("tenki", "tenki.gateway", "CRABBOX_TENKI_GATEWAY or --tenki-gateway")
		}
	}
	return nil
}

func inheritedCredential(credentials ...sourcedCredential) bool {
	for _, credential := range credentials {
		if strings.TrimSpace(credential.value) != "" && credential.source != credentialSourceRepository {
			return true
		}
	}
	return false
}

func repositorySSHDestinationUsesInheritedAuth(key string, source credentialValueSource, repositoryRoot string) bool {
	// An empty key delegates authentication to ambient SSH config or an agent.
	if strings.TrimSpace(key) == "" || source != credentialSourceRepository {
		return true
	}
	return !repositoryContainsSSHKey(repositoryRoot, key)
}

func repositoryContainsSSHKey(repositoryRoot, key string) bool {
	key = strings.TrimSpace(key)
	if repositoryRoot == "" || key == "" || filepath.IsAbs(key) {
		return false
	}
	root, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, key))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.Mode().IsRegular()
}

func nomadSelectedTokenEnvHasValue(cfg Config) bool {
	envName := strings.TrimSpace(cfg.Nomad.TokenEnv)
	if envName == "" {
		envName = "NOMAD_TOKEN"
	}
	return strings.TrimSpace(os.Getenv(envName)) != ""
}

func repositoryCredentialDestinationError(provider, field, override string) error {
	return exit(2, "provider=%s refuses repository-configured %s with inherited credentials; set %s to explicitly approve the credential destination", provider, field, override)
}
