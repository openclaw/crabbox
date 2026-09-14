package cli

// ProviderAuthenticationMethod describes a possible provider-access interface,
// not credential presence, permission, or a successful authentication check.
type ProviderAuthenticationMethod string

const (
	ProviderAuthenticationAPIKey           ProviderAuthenticationMethod = "api_key"
	ProviderAuthenticationAPIToken         ProviderAuthenticationMethod = "api_token"
	ProviderAuthenticationSessionToken     ProviderAuthenticationMethod = "session_token"
	ProviderAuthenticationAPICredentials   ProviderAuthenticationMethod = "api_credentials"
	ProviderAuthenticationUsernamePassword ProviderAuthenticationMethod = "username_password"
	ProviderAuthenticationCLI              ProviderAuthenticationMethod = "cli"
	ProviderAuthenticationSDKCredentials   ProviderAuthenticationMethod = "sdk_credentials"
	ProviderAuthenticationNativeConfig     ProviderAuthenticationMethod = "native_config"
	ProviderAuthenticationSSH              ProviderAuthenticationMethod = "ssh"
	ProviderAuthenticationLocalContext     ProviderAuthenticationMethod = "local_context"
	ProviderAuthenticationExternalContract ProviderAuthenticationMethod = "external_contract"
	ProviderAuthenticationSharedSecret     ProviderAuthenticationMethod = "shared_secret"
	ProviderAuthenticationIdentityToken    ProviderAuthenticationMethod = "identity_token"
	ProviderAuthenticationCoordinator      ProviderAuthenticationMethod = "coordinator"
	ProviderAuthenticationNone             ProviderAuthenticationMethod = "none"
)

// ProviderAuthenticationRoute declares possible interfaces for a route. Methods
// are not necessarily alternatives or jointly required; Description qualifies
// conditional combinations. No route is selected or checked by this metadata.
type ProviderAuthenticationRoute struct {
	Route       string
	Methods     []ProviderAuthenticationMethod
	Description string
}

// ProviderAuthentication covers provider control-plane access (or the sole
// connection for SSH/external providers). Guest/bootstrap, image-registry, and
// deployment credentials remain separate requirements. Local context is not a
// claim that the complete provider lifecycle needs no credentials.
type ProviderAuthentication []ProviderAuthenticationRoute

// DirectProviderAuthentication declares possible direct-access interfaces and
// snapshots the method list without resolving credentials or invoking a client.
func DirectProviderAuthentication(methods ...ProviderAuthenticationMethod) ProviderAuthentication {
	return ProviderAuthentication{{
		Route:       "direct",
		Methods:     append([]ProviderAuthenticationMethod(nil), methods...),
		Description: "Possible direct-access interfaces; authentication and readiness are not checked.",
	}}
}
