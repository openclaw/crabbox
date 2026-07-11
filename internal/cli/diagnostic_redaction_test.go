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
		"refresh_token=refresh-assignment-value",
		"api-secret=api-assignment-value",
		"refreshToken=refresh-camel-assignment-value",
		"idToken=id-camel-assignment-value",
		"secretAccessKey=aws-camel-assignment-value",
		"apiSecret=api-camel-assignment-value",
		"Authorization: Token token-scheme-value",
		"Proxy-Authorization: Digest digest-scheme-value",
		`Authorization: Digest username="digest-user", response="digest-response"`,
		"Authorization: 1custom digit-scheme-value",
		`{"clientSecret":"json-secret","privateKey":"json-key","accessToken":"access-json-value","refresh_token":"refresh-snake-value\\\"refresh-escaped-tail","refreshToken":"refresh-camel-value","refresh-token":"refresh-kebab-value","id_token":"id-snake-value","idToken":"id-camel-value","id-token":"id-kebab-value","secretAccessKey":"aws-camel-value","secret_access_key":"aws-snake-value","secret-access-key":"aws-kebab-value","apiSecret":"api-camel-value","api_secret":"api-snake-value","api-secret":"api-kebab-value","nextToken":"page-2","token_type":"Bearer","message":"quota exceeded"}`,
		"https://alice:password@example.test/path?access_token=query-token&refresh_token=query-refresh-token&id_token=query-id-token&secret_access_key=query-aws-secret&api_secret=query-api-secret&x-api-key=query-api-key&authorization=query-authorization&proxy-authorization=query-proxy-authorization&session_token=query-session-token&x-amz-signature=signed-value&x-goog-credential=gcs-credential&x-goog-signature=gcs-signature&region=eu",
		"https://single-userinfo-token@other.example.test/path",
		"https://first-userinfo-value@second-userinfo-value@multi.example.test/path",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-material\n-----END OPENSSH PRIVATE KEY-----",
	}, "\n")

	got := RedactDiagnosticSecrets(value, exact)
	for _, leaked := range []string{
		exact, "header-token", "colon-header-token", "folded-header-token", "folded-colon-header-token", "spaced-folded-colon-header-token", "spaced-bearer-token", "colon-bearer-token", "folded-bearer-token", "folded-colon-bearer-token", "spaced-folded-colon-bearer-token", "proxy-value", "refresh-assignment-value", "api-assignment-value", "refresh-camel-assignment-value", "id-camel-assignment-value", "aws-camel-assignment-value", "api-camel-assignment-value", "json-secret", "json-key", "access-json-value", "refresh-snake-value", "refresh-escaped-tail", "refresh-camel-value", "refresh-kebab-value", "id-snake-value", "id-camel-value", "id-kebab-value", "aws-camel-value", "aws-snake-value", "aws-kebab-value", "api-camel-value", "api-snake-value", "api-kebab-value", "alice", "password",
		"token-scheme-value", "digest-scheme-value", "digest-user", "digest-response", "digit-scheme-value", "query-token", "query-refresh-token", "query-id-token", "query-aws-secret", "query-api-secret", "query-api-key", "query-authorization", "query-proxy-authorization", "query-session-token", "signed-value", "gcs-credential", "gcs-signature", "single-userinfo-token", "first-userinfo-value", "second-userinfo-value", "private-material",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("diagnostic redaction leaked %q:\n%s", leaked, got)
		}
	}
	for _, preserved := range []string{"Standalone Bearer [redacted]", "quota exceeded", `"nextToken":"page-2"`, `"token_type":"Bearer"`, "example.test/path", "multi.example.test/path", "region=eu"} {
		if !strings.Contains(got, preserved) {
			t.Fatalf("diagnostic redaction removed %q:\n%s", preserved, got)
		}
	}
	if strings.Count(got, diagnosticRedaction) < 8 {
		t.Fatalf("missing redaction markers:\n%s", got)
	}
	if again := RedactDiagnosticSecrets(got, exact); again != got {
		t.Fatalf("diagnostic redaction is not idempotent:\nfirst: %s\nagain: %s", got, again)
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
	value := "http://<redacted>@broker.example.test https://[redacted]@api.example.test https://host.example.test?email=alice@example.test https://host.example.test#realm@tenant"
	if got := RedactDiagnosticSecrets(value, "redacted"); got != value {
		t.Fatalf("already-redacted URLs changed: %q", got)
	}
}

func TestRedactDiagnosticSecretsRemovesWholeStructuredExactSecrets(t *testing.T) {
	for _, test := range []struct {
		value  string
		secret string
		want   string
	}{
		{value: "prefix Bearer opaque-suffix", secret: "prefix Bearer opaque-suffix", want: diagnosticRedaction},
		{value: "abc?token=xyz", secret: "abc?token=xyz", want: diagnosticRedaction},
		{value: "?token=xyz&supersecretSuffix", secret: "token=xyz&supersecretSuffix", want: "?" + diagnosticRedaction},
	} {
		if got := RedactDiagnosticSecrets(test.value, test.secret); got != test.want {
			t.Fatalf("structured exact secret was only partially redacted: got %q", got)
		}
	}
}

func TestRedactDiagnosticSecretsStabilizesNewStructuralBoundaries(t *testing.T) {
	const secret = "refresh_token"
	got := RedactDiagnosticSecrets(`refresh_tokenBearer b?token=""`, secret)
	if strings.Contains(got, "Bearer b") {
		t.Fatalf("exact redaction exposed a new structural credential: %q", got)
	}
	if again := RedactDiagnosticSecrets(got, secret); again != got {
		t.Fatalf("combined structural and exact redaction is not idempotent: first=%q again=%q", got, again)
	}
}

func TestRedactDiagnosticSecretsHandlesStructuralKeySecretOverlap(t *testing.T) {
	const minted = "upstream-minted-value"
	got := RedactDiagnosticSecrets(`{"refresh_token":"`+minted+`","region":"iad"}`, "refresh_token")
	if strings.Contains(got, minted) || !strings.Contains(got, "region") {
		t.Fatalf("structural key overlap leaked a credential or routing context: %q", got)
	}
}

func TestRedactDiagnosticSecretsHandlesExactSecretMarkerOverlap(t *testing.T) {
	for _, test := range []struct {
		value   string
		secrets []string
	}{
		{value: "[redacted]suffix route=iad", secrets: []string{"[redacted]suffix"}},
		{value: "prefix[redacted]suffix route=iad", secrets: []string{"redacted]suffix"}},
		{value: "[redacted]suffix route=iad", secrets: []string{"redacted", "acted]suffix"}},
		{value: "triggersuffix route=iad", secrets: []string{"trigger", "redacted]suffix"}},
	} {
		got := RedactDiagnosticSecrets(test.value, test.secrets...)
		if strings.Contains(got, "suffix") || !strings.Contains(got, "route=iad") {
			t.Fatalf("marker-overlapping exact secret leaked or lost context: %q", got)
		}
		if strings.Contains(got, "[[redacted]") || strings.Contains(got, "[red[redacted]") {
			t.Fatalf("marker-overlapping exact secret corrupted the marker: %q", got)
		}
		if again := RedactDiagnosticSecrets(got, test.secrets...); again != got {
			t.Fatalf("marker-overlapping exact redaction is not idempotent: first=%q again=%q", got, again)
		}
	}
}

func TestRedactDiagnosticSecretsFailsClosedAtPassLimit(t *testing.T) {
	value := "trigger" + strings.Repeat("x", maxDiagnosticRedactionPasses+8)
	if got := RedactDiagnosticSecrets(value, "trigger", "redacted]x"); got != diagnosticRedaction {
		t.Fatalf("redaction pass limit did not fail closed: %q", got)
	}
}

func TestRedactDiagnosticSecretsConsumesUnterminatedSecretJSON(t *testing.T) {
	for _, input := range []string{
		`{"refresh_token":"unterminated-refresh-value`,
		`{"refresh_token":"dangling-backslash-refresh-value\`,
		`{"refresh_token":"[redacted]`,
	} {
		if got, want := RedactDiagnosticSecrets(input), `{"refresh_token":"[redacted]"`; got != want {
			t.Fatalf("unterminated JSON secret redaction = %q, want %q", got, want)
		}
	}
	malformed := `{"refresh_token":"unterminated-refresh-value`
	if got := RedactDiagnosticSecrets(malformed, malformed); got != diagnosticRedaction {
		t.Fatalf("whole malformed JSON secret redaction = %q, want %q", got, diagnosticRedaction)
	}
	if got, want := RedactDiagnosticSecrets(`Authorization: Digest `+malformed), `Authorization: [redacted]`; got != want {
		t.Fatalf("header-wrapped malformed JSON secret redaction = %q, want %q", got, want)
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
