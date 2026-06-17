package scaleway

import (
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNewClientReportsMissingAuthWithoutSecrets(t *testing.T) {
	clearScalewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := newClient(core.Config{}, core.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "SCW_ACCESS_KEY and SCW_SECRET_KEY") {
		t.Fatalf("newClient err=%v", err)
	}
}

func TestNewClientReportsPartialAuthWithoutSecretValue(t *testing.T) {
	clearScalewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SCW_ACCESS_KEY", "SCW11111111111111111")
	_, err := newClient(core.Config{}, core.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "SCW_SECRET_KEY") {
		t.Fatalf("newClient err=%v", err)
	}
	if strings.Contains(err.Error(), "SCW11111111111111111") {
		t.Fatalf("partial auth error leaked access key: %v", err)
	}
}

func TestNewClientSanitizesSDKValidationError(t *testing.T) {
	clearScalewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SCW_ACCESS_KEY", "invalid-access-key")
	t.Setenv("SCW_SECRET_KEY", "invalid-secret-key")
	t.Setenv("CRABBOX_SCALEWAY_PROJECT_ID", "project-1")
	_, err := newClient(core.Config{Scaleway: core.ScalewayConfig{ProjectID: "project-1"}}, core.Runtime{})
	if err == nil {
		t.Fatal("newClient unexpectedly succeeded")
	}
	text := err.Error()
	for _, secret := range []string{"invalid-access-key", "invalid-secret-key"} {
		if strings.Contains(text, secret) {
			t.Fatalf("SDK error leaked %q: %v", secret, err)
		}
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("SDK error did not include redaction marker: %v", err)
	}
}

func clearScalewayEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SCW_ACCESS_KEY",
		"SCW_SECRET_KEY",
		"SCW_DEFAULT_ORGANIZATION_ID",
		"SCW_DEFAULT_PROJECT_ID",
		"SCW_DEFAULT_REGION",
		"SCW_DEFAULT_ZONE",
		"SCW_PROFILE",
		"SCW_CONFIG_PATH",
		"CRABBOX_SCALEWAY_PROJECT_ID",
		"CRABBOX_SCALEWAY_ORGANIZATION_ID",
		"CRABBOX_SCALEWAY_REGION",
		"CRABBOX_SCALEWAY_ZONE",
	} {
		t.Setenv(key, "")
	}
}
