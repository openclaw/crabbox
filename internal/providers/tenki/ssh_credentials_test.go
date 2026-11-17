package tenki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestTenkiNativeCredentialPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantKey string
	}{
		{name: "stored", wantKey: "tk_test_stored"},
		{name: "API key overrides stored", env: map[string]string{"TENKI_API_KEY": "tk_test_api"}, wantKey: "tk_test_api"},
		{name: "auth token overrides API key", env: map[string]string{"TENKI_AUTH_TOKEN": "tk_test_auth", "TENKI_API_KEY": "tk_test_api"}, wantKey: "tk_test_auth"},
		{name: "empty auth preserves stored", env: map[string]string{"TENKI_AUTH_TOKEN": ""}, wantKey: "tk_test_stored"},
		{name: "empty auth allows API key", env: map[string]string{"TENKI_AUTH_TOKEN": "", "TENKI_API_KEY": "tk_test_api"}, wantKey: "tk_test_api"},
		{name: "whitespace auth allows API key", env: map[string]string{"TENKI_AUTH_TOKEN": " \t", "TENKI_API_KEY": "tk_test_api"}, wantKey: "tk_test_api"},
		{name: "empty API key preserves stored", env: map[string]string{"TENKI_API_KEY": ""}, wantKey: "tk_test_stored"},
		{name: "whitespace API key preserves stored", env: map[string]string{"TENKI_API_KEY": " \t"}, wantKey: "tk_test_stored"},
		{name: "Luxor API key overrides Tenki auth token", env: map[string]string{"LUXORAI_API_KEY": "lx_test_api", "TENKI_AUTH_TOKEN": "tk_test_auth"}, wantKey: "lx_test_api"},
		{name: "Luxor auth token overrides Luxor API key", env: map[string]string{"LUXORAI_AUTH_TOKEN": "lx_test_auth", "LUXORAI_API_KEY": "lx_test_api"}, wantKey: "lx_test_auth"},
		{name: "blank Luxor values fall back to Tenki", env: map[string]string{"LUXORAI_AUTH_TOKEN": " ", "LUXORAI_API_KEY": "", "TENKI_API_KEY": "tk_test_api"}, wantKey: "tk_test_api"},
		{name: "invalid Luxor key is not skipped", env: map[string]string{"LUXORAI_API_KEY": "invalid_test_key", "TENKI_API_KEY": "tk_test_api"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := tenkiCredentialTestHome(t)
			writeTenkiCredentialTestConfig(t, home, "config.yaml", "auth_token: tk_test_stored\n")
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			got, err := tenkiNativeCredentialContext(core.Config{})
			if tc.wantKey == "" {
				assertTenkiCredentialFailure(t, got, err, 3, "tk_test_stored", "tk_test_api", "invalid_test_key")
				return
			}
			if err != nil || got.key != tc.wantKey || got.endpoint == nil || got.endpoint.String() != tenkiProductionAPI {
				t.Fatalf("credential context: key=%q endpoint=%v err=%v", got.key, got.endpoint, err)
			}
		})
	}
}

func TestTenkiNativeCredentialConfigSelection(t *testing.T) {
	t.Run("native home ignores XDG", func(t *testing.T) {
		home := tenkiCredentialTestHome(t)
		writeTenkiCredentialTestConfig(t, home, "config.yaml", "auth_token: tk_test_native\napi_endpoint: https://native.example.test\n")
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		if err := os.MkdirAll(filepath.Join(xdg, "tenki"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(xdg, "tenki", "config.yaml"), []byte("auth_token: tk_test_wrong_home\napi_endpoint: https://wrong.example.test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := tenkiNativeCredentialContext(core.Config{})
		if err != nil || got.key != "tk_test_native" || got.endpoint == nil || got.endpoint.String() != "https://native.example.test" {
			t.Fatalf("native config not selected: key=%q endpoint=%v err=%v", got.key, got.endpoint, err)
		}
	})
	t.Run("custom basename", func(t *testing.T) {
		home := tenkiCredentialTestHome(t)
		writeTenkiCredentialTestConfig(t, home, "config.yaml", "auth_token: tk_test_default\n")
		writeTenkiCredentialTestConfig(t, home, "custom.yaml", "auth_token: tk_test_custom\napi_endpoint: https://custom.example.test/api\n")
		t.Setenv("TENKI_CONFIG_FILE", "custom.yaml")
		got, err := tenkiNativeCredentialContext(core.Config{})
		if err != nil || got.key != "tk_test_custom" || got.endpoint == nil || got.endpoint.String() != "https://custom.example.test/api" {
			t.Fatalf("custom config not selected: key=%q endpoint=%v err=%v", got.key, got.endpoint, err)
		}
	})
	t.Run("missing selected config does not fall back", func(t *testing.T) {
		home := tenkiCredentialTestHome(t)
		writeTenkiCredentialTestConfig(t, home, "config.yaml", "auth_token: tk_test_default\napi_endpoint: https://default.example.test\n")
		t.Setenv("TENKI_CONFIG_FILE", "missing.yaml")
		got, err := tenkiNativeCredentialContext(core.Config{})
		assertTenkiCredentialFailure(t, got, err, 3, "tk_test_default")
		t.Setenv("TENKI_API_KEY", "tk_test_environment")
		got, err = tenkiNativeCredentialContext(core.Config{})
		if err != nil || got.key != "tk_test_environment" || got.endpoint == nil || got.endpoint.String() != tenkiProductionAPI {
			t.Fatalf("missing custom config fell back: key=%q endpoint=%v err=%v", got.key, got.endpoint, err)
		}
	})
	t.Run("missing default config", func(t *testing.T) {
		tenkiCredentialTestHome(t)
		got, err := tenkiNativeCredentialContext(core.Config{})
		assertTenkiCredentialFailure(t, got, err, 3)
	})
	for _, name := range []string{".", "..", "/absolute.yaml", "nested/config.yaml", `nested\config.yaml`, "config\r.yaml", "config\n.yaml"} {
		t.Run("reject filename "+name, func(t *testing.T) {
			tenkiCredentialTestHome(t)
			t.Setenv("TENKI_CONFIG_FILE", name)
			t.Setenv("TENKI_API_KEY", "tk_test_environment")
			got, err := tenkiNativeCredentialContext(core.Config{})
			assertTenkiCredentialFailure(t, got, err, 2, "tk_test_environment")
		})
	}
}

func TestTenkiNativeCredentialMalformedConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "invalid YAML", data: "auth_token: tk_test_malformed\nprivate_fixture: [unterminated\n"},
		{name: "wrong credential type", data: "auth_token: [tk_test_malformed]\n"},
		{name: "duplicate credential", data: "auth_token: tk_test_malformed\nauth_token: tk_test_other\n"},
		{name: "oversize", data: "auth_token: tk_test_malformed\n#" + strings.Repeat("x", 1<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := tenkiCredentialTestHome(t)
			writeTenkiCredentialTestConfig(t, home, "config.yaml", tc.data)
			t.Setenv("TENKI_API_KEY", "tk_test_environment")
			got, err := tenkiNativeCredentialContext(core.Config{})
			assertTenkiCredentialFailure(t, got, err, 2, "tk_test_malformed", "tk_test_environment", "private_fixture")
		})
	}
	t.Run("directory instead of config", func(t *testing.T) {
		home := tenkiCredentialTestHome(t)
		if err := os.MkdirAll(filepath.Join(home, ".config", "tenki", "config.yaml"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TENKI_API_KEY", "tk_test_environment")
		got, err := tenkiNativeCredentialContext(core.Config{})
		assertTenkiCredentialFailure(t, got, err, 2, "tk_test_environment")
	})
}

func TestTenkiNativeCredentialEndpointPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		override string
		want     string
	}{
		{name: "stored endpoint", want: "https://stored.example.test"},
		{name: "environment endpoint", env: map[string]string{"TENKI_API_ENDPOINT": "https://environment.example.test"}, want: "https://environment.example.test"},
		{name: "empty environment preserves stored", env: map[string]string{"TENKI_API_ENDPOINT": ""}, want: "https://stored.example.test"},
		{name: "Luxor environment overrides Tenki", env: map[string]string{"LUXORAI_API_ENDPOINT": "https://luxor.example.test", "TENKI_API_ENDPOINT": "https://environment.example.test"}, want: "https://luxor.example.test"},
		{name: "blank Luxor environment falls back to Tenki", env: map[string]string{"LUXORAI_API_ENDPOINT": " ", "TENKI_API_ENDPOINT": "https://environment.example.test"}, want: "https://environment.example.test"},
		{name: "SDK API URL ignored", env: map[string]string{"TENKI_API_URL": "https://wrong.example.test"}, want: "https://stored.example.test"},
		{name: "explicit endpoint overrides both", env: map[string]string{"TENKI_API_ENDPOINT": "https://environment.example.test", "TENKI_API_URL": "https://wrong.example.test"}, override: "https://explicit.example.test/api/", want: "https://explicit.example.test/api/"},
		{name: "explicit endpoint overrides malformed environment", env: map[string]string{"TENKI_API_ENDPOINT": " "}, override: "https://explicit.example.test", want: "https://explicit.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := tenkiCredentialTestHome(t)
			writeTenkiCredentialTestConfig(t, home, "config.yaml", "auth_token: tk_test_stored\napi_endpoint: https://stored.example.test\n")
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			got, err := tenkiNativeCredentialContext(core.Config{Tenki: core.TenkiConfig{Endpoint: tc.override}})
			if err != nil || got.key != "tk_test_stored" || got.endpoint == nil || got.endpoint.String() != tc.want {
				t.Fatalf("endpoint precedence: key=%q endpoint=%v err=%v", got.key, got.endpoint, err)
			}
		})
	}
}

func TestTenkiNativeCredentialRejectsUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"http://api.example.test", "//api.example.test", "https:///path", "https://",
		"https://private-user:private-password@api.example.test", "https://api.example.test?private_query=value",
		"https://api.example.test#private_fragment", "https://api.example.test/%zz",
	} {
		t.Run(endpoint, func(t *testing.T) {
			tenkiCredentialTestHome(t)
			t.Setenv("TENKI_API_KEY", "tk_test_environment")
			t.Setenv("TENKI_API_ENDPOINT", endpoint)
			got, err := tenkiNativeCredentialContext(core.Config{})
			assertTenkiCredentialFailure(t, got, err, 2, "tk_test_environment", "api.example.test", "private-user", "private-password", "private_query", "private_fragment")
		})
	}
}

func TestTenkiNativeCredentialRejectsInvalidKeysWithoutEcho(t *testing.T) {
	for _, key := range []string{"legacy-private-token", "tk_test_private token", "tk_test_private\r\nInjected: value"} {
		t.Run("invalid key", func(t *testing.T) {
			tenkiCredentialTestHome(t)
			t.Setenv("TENKI_API_KEY", key)
			got, err := tenkiNativeCredentialContext(core.Config{})
			assertTenkiCredentialFailure(t, got, err, 3, key, "private", "Injected")
		})
	}
}

func tenkiCredentialTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{
		"LUXORAI_CONFIG_FILE", "LUXORAI_AUTH_TOKEN", "LUXORAI_API_KEY", "LUXORAI_API_ENDPOINT", "LUXORAI_API_URL",
		"TENKI_CONFIG_FILE", "TENKI_AUTH_TOKEN", "TENKI_API_KEY", "TENKI_API_ENDPOINT", "TENKI_API_URL", "XDG_CONFIG_HOME",
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func writeTenkiCredentialTestConfig(t *testing.T, home, name, data string) {
	t.Helper()
	writeTenkiCredentialTestConfigIn(t, home, "tenki", name, data)
}

func writeTenkiCredentialTestConfigIn(t *testing.T, home, dirName, name, data string) {
	t.Helper()
	dir := filepath.Join(home, ".config", dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertTenkiCredentialFailure(t *testing.T, got tenkiCredentialContext, err error, code int, concealed ...string) {
	t.Helper()
	if err == nil || core.ExitCodeForError(err, 0) != code {
		t.Fatalf("want exit code %d; got %v", code, err)
	}
	if got.key != "" || got.endpoint != nil {
		t.Fatal("failure returned partial credential context")
	}
	for _, value := range concealed {
		if strings.Contains(err.Error(), value) {
			t.Fatal("error exposed credential or endpoint details")
		}
	}
}

func TestTenkiNativeCredentialConfigDirectory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		luxor   string
		marker  bool
		tenki   string
		wantKey string
	}{
		{name: "Luxor directory", luxor: "lx_test_luxor", wantKey: "lx_test_luxor"},
		{name: "Luxor preferred over legacy", luxor: "lx_test_luxor", tenki: "tk_test_legacy", wantKey: "lx_test_luxor"},
		{name: "legacy before migration", tenki: "tk_test_legacy", wantKey: "tk_test_legacy"},
		{name: "legacy while Luxor directory lacks config", luxor: "-", tenki: "tk_test_legacy", wantKey: "tk_test_legacy"},
		{name: "migrated Luxor logout ignores legacy", luxor: "-", marker: true, tenki: "tk_test_legacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := tenkiCredentialTestHome(t)
			if tc.luxor != "" {
				if err := os.MkdirAll(filepath.Join(home, ".config", "luxor"), 0o700); err != nil {
					t.Fatal(err)
				}
				if tc.luxor != "-" {
					writeTenkiCredentialTestConfigIn(t, home, "luxor", "config.yaml", "auth_token: "+tc.luxor+"\n")
				}
			}
			if tc.marker {
				writeTenkiCredentialTestConfigIn(t, home, "luxor", ".migrated-from-tenki", "")
			}
			if tc.tenki != "" {
				writeTenkiCredentialTestConfig(t, home, "config.yaml", "auth_token: "+tc.tenki+"\n")
			}
			got, err := tenkiNativeCredentialContext(core.Config{})
			if tc.wantKey == "" {
				assertTenkiCredentialFailure(t, got, err, 3, "tk_test_legacy")
				return
			}
			if err != nil || got.key != tc.wantKey {
				t.Fatalf("credential context: key=%q err=%v", got.key, err)
			}
		})
	}
}

func TestTenkiNativeCredentialLuxorConfigFile(t *testing.T) {
	home := tenkiCredentialTestHome(t)
	writeTenkiCredentialTestConfigIn(t, home, "luxor", "config.yaml", "auth_token: lx_test_default\n")
	writeTenkiCredentialTestConfigIn(t, home, "luxor", "luxor.yaml", "auth_token: lx_test_luxor_file\n")
	writeTenkiCredentialTestConfigIn(t, home, "luxor", "tenki.yaml", "auth_token: tk_test_tenki_file\n")
	t.Setenv("TENKI_CONFIG_FILE", "tenki.yaml")
	t.Setenv("LUXORAI_CONFIG_FILE", "luxor.yaml")
	got, err := tenkiNativeCredentialContext(core.Config{})
	if err != nil || got.key != "lx_test_luxor_file" {
		t.Fatalf("credential context: key=%q err=%v", got.key, err)
	}
}
