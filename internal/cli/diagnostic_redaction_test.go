package cli

import (
	"strings"
	"testing"
)

type staticDiagnosticSecretSource []string

func (s staticDiagnosticSecretSource) DiagnosticSecrets(Config) []string { return s }

func TestRedactDiagnosticSecretsCoversCredentialEncodings(t *testing.T) {
	exact := "configured-provider-secret"
	value := strings.Join([]string{
		"exact=" + exact,
		"Authorization: Bearer header-token",
		"Authorization: Bearer: colon-header-token",
		"Authorization: Bearer\n folded-header-token",
		"Authorization: Bearer:\r\n folded-colon-header-token",
		"Authorization: Bearer :\r\n spaced-folded-colon-header-token",
		"Standalone Bearer   spaced-bearer-token",
		"Standalone Bearer: colon-bearer-token",
		"Standalone Bearer\n folded-bearer-token",
		"Standalone Bearer:\r\n folded-colon-bearer-token",
		"Standalone Bearer :\r\n spaced-folded-colon-bearer-token",
		"Standalone Bearer [redacted]",
		"Proxy-Authorization=Basic proxy-value",
		`{"clientSecret":"json-secret","privateKey":"json-key","message":"quota exceeded"}`,
		"https://alice:password@example.test/path?access_token=query-token&x-api-key=query-api-key&authorization=query-authorization&proxy-authorization=query-proxy-authorization&session_token=query-session-token&x-amz-signature=signed-value&x-goog-credential=gcs-credential&x-goog-signature=gcs-signature&region=eu",
		"https://single-userinfo-token@other.example.test/path",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-material\n-----END OPENSSH PRIVATE KEY-----",
	}, "\n")

	got := RedactDiagnosticSecrets(value, exact)
	for _, leaked := range []string{
		exact, "header-token", "colon-header-token", "folded-header-token", "folded-colon-header-token", "spaced-folded-colon-header-token", "spaced-bearer-token", "colon-bearer-token", "folded-bearer-token", "folded-colon-bearer-token", "spaced-folded-colon-bearer-token", "proxy-value", "json-secret", "json-key", "alice", "password",
		"query-token", "query-api-key", "query-authorization", "query-proxy-authorization", "query-session-token", "signed-value", "gcs-credential", "gcs-signature", "single-userinfo-token", "private-material",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("diagnostic redaction leaked %q:\n%s", leaked, got)
		}
	}
	for _, preserved := range []string{"Standalone Bearer [redacted]", "quota exceeded", "example.test/path", "region=eu"} {
		if !strings.Contains(got, preserved) {
			t.Fatalf("diagnostic redaction removed %q:\n%s", preserved, got)
		}
	}
	if strings.Count(got, diagnosticRedaction) < 8 {
		t.Fatalf("missing redaction markers:\n%s", got)
	}
}

func TestRedactDiagnosticSecretsConsumesPunctuationBearingCredentials(t *testing.T) {
	for _, separator := range []string{",", ";", "'", "}", "&", "#", "?", `\"`} {
		credential := "prefix" + separator + "secret-suffix"
		for _, test := range []struct {
			name  string
			input string
			want  string
		}{
			{name: "header", input: "Authorization: Bearer " + credential + " route=iad", want: "Authorization: [redacted] route=iad"},
			{name: "bearer", input: "upstream Bearer " + credential + " request=retry", want: "upstream Bearer [redacted] request=retry"},
		} {
			t.Run(test.name+"/"+separator, func(t *testing.T) {
				if got := RedactDiagnosticSecrets(test.input); got != test.want {
					t.Fatalf("got %q, want %q", got, test.want)
				}
			})
		}
	}
	if got := RedactDiagnosticSecrets("provider:Authorization: Bearer prefix}secret route=iad"); got != "provider:Authorization: [redacted] route=iad" {
		t.Fatalf("prefixed header redaction lost its key: %q", got)
	}
}

func TestRedactDiagnosticSecretsPreservesSafeURLPlaceholders(t *testing.T) {
	value := "http://<redacted>@broker.example.test https://[redacted]@api.example.test"
	if got := RedactDiagnosticSecrets(value); got != value {
		t.Fatalf("already-redacted URLs changed: %q", got)
	}
}

func TestConfiguredDiagnosticSecretsFindsConfigAndSelectedEnvironment(t *testing.T) {
	t.Setenv("AWS_SESSION_TOKEN", "environment-session-secret")
	t.Setenv("CUSTOM_NOMAD_TOKEN", "nomad-environment-secret")
	cfg := Config{
		CoordToken: "coordinator-secret",
		Morph:      MorphConfig{APIKey: "morph-secret"},
		Nomad:      NomadConfig{TokenEnv: "CUSTOM_NOMAD_TOKEN"},
		Profiles: map[string]ProfileConfig{
			"test": {Env: map[string]string{"SERVICE_TOKEN": "profile-secret"}},
		},
	}
	secrets := configuredDiagnosticSecrets(cfg)
	redacted := RedactDiagnosticSecrets(strings.Join([]string{
		"coordinator-secret", "morph-secret", "environment-session-secret", "nomad-environment-secret", "profile-secret",
	}, " "), secrets...)
	if strings.Contains(redacted, "secret") || strings.Count(redacted, diagnosticRedaction) != 5 {
		t.Fatalf("configured secrets were not collected: %s", redacted)
	}
}

func TestCollectProviderDiagnosticSecretsFindsRuntimeOnlyValues(t *testing.T) {
	const runtimeSecret = "runtime-only-provider-secret"
	seen := map[string]bool{}
	collectProviderDiagnosticSecrets(Config{}, []ProviderDiagnosticSecretSource{
		staticDiagnosticSecretSource{" ", "abc", " " + runtimeSecret + " ", runtimeSecret},
	}, seen)

	got := RedactDiagnosticSecrets("opaque diagnostic value "+runtimeSecret+" region=eu", secretsFromSet(seen)...)
	if strings.Contains(got, runtimeSecret) || !strings.Contains(got, "region=eu") {
		t.Fatalf("provider diagnostic secret collection failed: %q", got)
	}
	if len(seen) != 1 {
		t.Fatalf("seen=%v want one normalized secret", seen)
	}
}

func secretsFromSet(values map[string]bool) []string {
	secrets := make([]string, 0, len(values))
	for value := range values {
		secrets = append(secrets, value)
	}
	return secrets
}
