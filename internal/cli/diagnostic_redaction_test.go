package cli

import (
	"strings"
	"testing"
)

func TestRedactDiagnosticSecretsCoversCredentialEncodings(t *testing.T) {
	exact := "configured-provider-secret"
	value := strings.Join([]string{
		"exact=" + exact,
		"Authorization: Bearer header-token",
		"Proxy-Authorization=Basic proxy-value",
		`{"clientSecret":"json-secret","privateKey":"json-key","message":"quota exceeded"}`,
		"https://alice:password@example.test/path?access_token=query-token&x-amz-signature=signed-value&x-goog-credential=gcs-credential&x-goog-signature=gcs-signature&region=eu",
		"https://single-userinfo-token@other.example.test/path",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-material\n-----END OPENSSH PRIVATE KEY-----",
	}, "\n")

	got := RedactDiagnosticSecrets(value, exact)
	for _, leaked := range []string{
		exact, "header-token", "proxy-value", "json-secret", "json-key", "alice", "password",
		"query-token", "signed-value", "gcs-credential", "gcs-signature", "single-userinfo-token", "private-material",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("diagnostic redaction leaked %q:\n%s", leaked, got)
		}
	}
	for _, preserved := range []string{"quota exceeded", "example.test/path", "region=eu"} {
		if !strings.Contains(got, preserved) {
			t.Fatalf("diagnostic redaction removed %q:\n%s", preserved, got)
		}
	}
	if strings.Count(got, diagnosticRedaction) < 8 {
		t.Fatalf("missing redaction markers:\n%s", got)
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
