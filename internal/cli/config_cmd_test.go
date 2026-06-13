package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceInstanceConfigShowRedactsEndpointCredentials(t *testing.T) {
	cfg := baseConfig()
	cfg.NamespaceInstance.Endpoint = "https://user:secret@api.example.test/path"
	data, err := json.Marshal(configShowView(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	writeConfigShowText(&text, cfg)
	for name, output := range map[string]string{"json": string(data), "text": text.String()} {
		if strings.Contains(output, "secret") || strings.Contains(output, "user@") {
			t.Fatalf("%s output leaked endpoint credentials: %s", name, output)
		}
		if !strings.Contains(output, "api.example.test/path") || !strings.Contains(output, "redacted") {
			t.Fatalf("%s output missing redacted endpoint: %s", name, output)
		}
	}
}

func TestConfigSetBrokerRegisteredMode(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configSetBroker([]string{
		"--url", "https://broker.example.test",
		"--provider", "external",
		"--mode", "registered",
		"--auto-webvnc=false",
	}); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if file.Broker == nil || file.Broker.URL != "https://broker.example.test" || file.Provider != "external" || file.Broker.Mode != "registered" || file.Broker.AutoWebVNC == nil || *file.Broker.AutoWebVNC {
		t.Fatalf("config=%#v", file)
	}
	if !strings.Contains(stdout.String(), "mode=registered") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestConfigSetBrokerRegisteredModeAcceptsDirectProvider(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")

	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.configSetBroker([]string{
		"--url", "https://broker.example.test",
		"--provider", "xcp-ng",
		"--mode", "registered",
	}); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if file.Provider != "xcp-ng" || file.Broker == nil || file.Broker.Provider != "xcp-ng" {
		t.Fatalf("config=%#v", file)
	}
}

func TestConfigSetBrokerRegisteredModeRejectsUnknownProvider(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")

	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err := app.configSetBroker([]string{
		"--url", "https://broker.example.test",
		"--provider", "missing-provider",
		"--mode", "registered",
	})
	if err == nil || !strings.Contains(err.Error(), `unknown provider "missing-provider"`) {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config should not be written, stat err=%v", statErr)
	}
}

func TestConfigSetBrokerUsesPersistedRegisteredModeForProviderValidation(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")
	if err := os.WriteFile(configPath, []byte("broker:\n  url: https://old.example.test\n  mode: Registered\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.configSetBroker([]string{
		"--url", "https://new.example.test",
		"--provider", "xcp-ng",
	}); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if file.Broker == nil || file.Broker.Mode != "Registered" || file.Broker.Provider != "xcp-ng" {
		t.Fatalf("config=%#v", file)
	}
}

func TestConfigSetBrokerRejectsPersistedDirectProviderWhenSwitchingToManaged(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")
	original := "provider: xcp-ng\nbroker:\n  url: https://old.example.test\n  mode: registered\n  provider: xcp-ng\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err := app.configSetBroker([]string{
		"--url", "https://new.example.test",
		"--mode", "managed",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be used with a broker") {
		t.Fatalf("err=%v, want managed provider rejection", err)
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != original {
		t.Fatalf("config changed after rejection:\n%s", data)
	}
}

func TestConfigSetBrokerDoesNotPromoteTopLevelProviderWhenOmitted(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")
	if err := os.WriteFile(configPath, []byte("provider: xcp-ng\nbroker:\n  url: https://old.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.configSetBroker([]string{"--url", "https://new.example.test"}); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if file.Provider != "xcp-ng" || file.Broker == nil || file.Broker.Provider != "" {
		t.Fatalf("config=%#v", file)
	}
}

func TestConfigShowIncludesRunPreflightTools(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("run:\n  preflightTools: [node, bun]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "run preflight_tools=node,bun") {
		t.Fatalf("config show missing run preflight tools: %q", stdout.String())
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Run struct {
			PreflightTools []string `json:"preflightTools"`
		} `json:"run"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Run.PreflightTools, ",") != "node,bun" {
		t.Fatalf("json run.preflightTools=%v", got.Run.PreflightTools)
	}
}

func TestConfigShowRedactsCloudflareDynamicWorkers(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte(`provider: aws
cloudflareDynamicWorkers:
  loaderUrl: https://user:pass@loader.example.test?token=query-secret#fragment-secret
  token: secret-token
  compatibilityDate: "2026-06-01"
  compatibilityFlags: [nodejs_compat]
  cacheMode: stable
  egress: blocked
  cpuMs: 50
  subrequests: 12
  timeoutSecs: 30
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if strings.Contains(text, "secret-token") || strings.Contains(text, "user:pass") || strings.Contains(text, "query-secret") || strings.Contains(text, "fragment-secret") {
		t.Fatalf("config show leaked secret: %q", text)
	}
	if !strings.Contains(text, "cloudflare_dynamic_workers loader_url=https://<redacted>@loader.example.test") || !strings.Contains(text, "auth=configured") {
		t.Fatalf("config show missing dynamic workers redacted details: %q", text)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		CloudflareDynamicWorkers struct {
			LoaderURL string `json:"loaderUrl"`
			Auth      string `json:"auth"`
		} `json:"cloudflareDynamicWorkers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CloudflareDynamicWorkers.LoaderURL != "https://<redacted>@loader.example.test" || got.CloudflareDynamicWorkers.Auth != "configured" {
		t.Fatalf("json dynamic workers=%#v", got.CloudflareDynamicWorkers)
	}
	if strings.Contains(stdout.String(), "secret-token") || strings.Contains(stdout.String(), "user:pass") || strings.Contains(stdout.String(), "query-secret") || strings.Contains(stdout.String(), "fragment-secret") {
		t.Fatalf("config show json leaked secret: %q", stdout.String())
	}
}

func TestRedactedConfigURLWithoutQueryStripsQueryAndFragmentWithoutUserinfo(t *testing.T) {
	got := redactedConfigURLWithoutQuery("https://loader.example.test/v1?token=query-secret#fragment-secret")
	if got != "https://loader.example.test/v1" {
		t.Fatalf("redacted URL=%q", got)
	}
}

func TestRedactedConfigURLWithoutQueryFailsClosedForMalformedURL(t *testing.T) {
	for _, raw := range []string{
		"https://loader.example.test/%zz?token=@query-secret",
		"https://api-token:443#pass@host/%zz",
	} {
		got := redactedConfigURLWithoutQuery(raw)
		if got != "<redacted>" || strings.Contains(got, "query-secret") || strings.Contains(got, "api-token") {
			t.Fatalf("redacted URL for %q=%q", raw, got)
		}
	}
}

func TestConfigSetBrokerRejectsDirectOnlyProvider(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	err := app.configSetBroker([]string{"--url", "https://broker.example.test", "--provider", "xcp-ng"})
	if err == nil || !strings.Contains(err.Error(), "cannot be used with a broker") {
		t.Fatalf("err=%v, want brokered provider rejection", err)
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config file exists after rejected provider: %v", statErr)
	}
}

func TestConfigShowIncludesJobHydrateGitHubRunner(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("jobs:\n  smoke:\n    architecture: arm64\n    hydrate:\n      actions: true\n      githubRunner: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Jobs map[string]struct {
			Architecture string `json:"architecture"`
			Hydrate      struct {
				GitHubRunner bool `json:"githubRunner"`
			} `json:"hydrate"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Jobs["smoke"].Hydrate.GitHubRunner {
		t.Fatalf("json jobs.smoke.hydrate.githubRunner=false in %s", stdout.String())
	}
	if got.Jobs["smoke"].Architecture != "arm64" {
		t.Fatalf("json jobs.smoke.architecture=%q in %s", got.Jobs["smoke"].Architecture, stdout.String())
	}
}

func TestConfigShowIncludesCloudflareWithoutSecret(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_CLOUDFLARE_RUNNER_TOKEN", "cloudflare-secret-token")
	if err := os.WriteFile(configPath, []byte("cloudflare:\n  apiUrl: https://cloudflare.example.test\n  workdir: /workspace/test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if !strings.Contains(text, "cloudflare api_url=https://cloudflare.example.test workdir=/workspace/test auth=configured") {
		t.Fatalf("config show missing cloudflare summary: %q", text)
	}
	if strings.Contains(text, "cloudflare-secret-token") {
		t.Fatalf("config show leaked Cloudflare token: %q", text)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Cloudflare struct {
			APIURL  string `json:"apiUrl"`
			Auth    string `json:"auth"`
			Workdir string `json:"workdir"`
		} `json:"cloudflare"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cloudflare.APIURL != "https://cloudflare.example.test" || got.Cloudflare.Workdir != "/workspace/test" || got.Cloudflare.Auth != "configured" {
		t.Fatalf("unexpected cloudflare json: %#v", got.Cloudflare)
	}
	if strings.Contains(stdout.String(), "cloudflare-secret-token") {
		t.Fatalf("config show json leaked Cloudflare token: %q", stdout.String())
	}
}

func TestConfigShowIncludesSuperserveWithoutSecret(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "superserve-secret-token")
	if err := os.WriteFile(configPath, []byte("superserve:\n  baseUrl: https://user:base-url-secret@superserve.example.test\n  template: superserve/custom\n  workdir: /workspace/test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if !strings.Contains(text, "superserve base_url=https://<redacted>@superserve.example.test template=superserve/custom snapshot=- workdir=/workspace/test") || !strings.Contains(text, "auth=configured") {
		t.Fatalf("config show missing superserve summary: %q", text)
	}
	if strings.Contains(text, "superserve-secret-token") || strings.Contains(text, "base-url-secret") {
		t.Fatalf("config show leaked Superserve credential: %q", text)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Superserve struct {
			BaseURL  string `json:"baseUrl"`
			Auth     string `json:"auth"`
			Template string `json:"template"`
			Workdir  string `json:"workdir"`
		} `json:"superserve"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Superserve.BaseURL != "https://<redacted>@superserve.example.test" || got.Superserve.Template != "superserve/custom" || got.Superserve.Workdir != "/workspace/test" || got.Superserve.Auth != "configured" {
		t.Fatalf("unexpected superserve json: %#v", got.Superserve)
	}
	if strings.Contains(stdout.String(), "superserve-secret-token") || strings.Contains(stdout.String(), "base-url-secret") {
		t.Fatalf("config show json leaked Superserve credential: %q", stdout.String())
	}
}

func TestConfigShowIncludesDigitalOceanProviderConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("provider: digitalocean\ndigitalocean:\n  region: sfo3\n  image: ubuntu-24-04-x64\n  vpc: vpc-123\n  sshCIDRs: [203.0.113.0/24, 2001:db8::/64]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if !strings.Contains(text, "digitalocean region=sfo3 image=ubuntu-24-04-x64 vpc=vpc-123 ssh_cidrs=203.0.113.0/24,2001:db8::/64") {
		t.Fatalf("config show missing digitalocean summary: %q", text)
	}
	if !strings.Contains(text, "ssh=root@<host>:22 fallback_ports=-") {
		t.Fatalf("config show missing effective digitalocean ssh defaults: %q", text)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		SSHUser          string   `json:"sshUser"`
		SSHPort          string   `json:"sshPort"`
		SSHFallbackPorts []string `json:"sshFallbackPorts"`
		DigitalOcean     struct {
			Region   string   `json:"region"`
			Image    string   `json:"image"`
			VPC      string   `json:"vpc"`
			SSHCIDRs []string `json:"sshCIDRs"`
		} `json:"digitalocean"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DigitalOcean.Region != "sfo3" ||
		got.DigitalOcean.Image != "ubuntu-24-04-x64" ||
		got.DigitalOcean.VPC != "vpc-123" ||
		strings.Join(got.DigitalOcean.SSHCIDRs, ",") != "203.0.113.0/24,2001:db8::/64" {
		t.Fatalf("unexpected digitalocean json: %#v", got.DigitalOcean)
	}
	if got.SSHUser != "root" || got.SSHPort != "22" || len(got.SSHFallbackPorts) != 0 {
		t.Fatalf("unexpected digitalocean ssh json: %#v", got)
	}
}

func TestConfigShowIncludesHostingerWithoutSecret(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("HOSTINGER_API_TOKEN", "hostinger-secret-token")
	if err := os.WriteFile(configPath, []byte(`hostinger:
  apiUrl: https://hostinger.example.test
  itemId: hostingercom-vps-kvm1-usd-1m
  paymentMethodId: "42"
  templateId: "1077"
  dataCenterId: "24"
  hostnamePrefix: cbx
  user: root
  workRoot: /work/crabbox
  allowPurchase: true
  releaseAction: stop
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if !strings.Contains(text, "hostinger api_url=https://hostinger.example.test item_id=hostingercom-vps-kvm1-usd-1m payment_method_id=42 template_id=1077 data_center_id=24 hostname_prefix=cbx user=root work_root=/work/crabbox allow_purchase=true release_action=stop auth=configured") {
		t.Fatalf("config show missing hostinger summary: %q", text)
	}
	if strings.Contains(text, "hostinger-secret-token") {
		t.Fatalf("config show leaked Hostinger token: %q", text)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Hostinger struct {
			APIURL          string `json:"apiUrl"`
			Auth            string `json:"auth"`
			ItemID          string `json:"itemId"`
			PaymentMethodID string `json:"paymentMethodId"`
			TemplateID      string `json:"templateId"`
			DataCenterID    string `json:"dataCenterId"`
			HostnamePrefix  string `json:"hostnamePrefix"`
			User            string `json:"user"`
			WorkRoot        string `json:"workRoot"`
			AllowPurchase   bool   `json:"allowPurchase"`
			ReleaseAction   string `json:"releaseAction"`
		} `json:"hostinger"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Hostinger.APIURL != "https://hostinger.example.test" ||
		got.Hostinger.Auth != "configured" ||
		got.Hostinger.ItemID != "hostingercom-vps-kvm1-usd-1m" ||
		got.Hostinger.PaymentMethodID != "42" ||
		got.Hostinger.TemplateID != "1077" ||
		got.Hostinger.DataCenterID != "24" ||
		got.Hostinger.HostnamePrefix != "cbx" ||
		got.Hostinger.User != "root" ||
		got.Hostinger.WorkRoot != "/work/crabbox" ||
		!got.Hostinger.AllowPurchase ||
		got.Hostinger.ReleaseAction != "stop" {
		t.Fatalf("unexpected hostinger json: %#v", got.Hostinger)
	}
	if strings.Contains(stdout.String(), "hostinger-secret-token") {
		t.Fatalf("config show json leaked Hostinger token: %q", stdout.String())
	}
}

func TestConfigShowIncludesNvidiaBrevWithoutSecretSurface(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_NVIDIA_BREV_TOKEN", "ignored-brev-secret")
	if err := os.WriteFile(configPath, []byte(`nvidiaBrev:
  cli: /usr/local/bin/brev
  org: example-org
  type: gpu
  gpuName: L40S
  provider: aws
  mode: vm
  launchable: pytorch
  startupScript: setup.sh
  releaseAction: stop
  target: host
  user: ubuntu
  workRoot: /work/brev
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	want := "nvidia_brev cli=/usr/local/bin/brev org=example-org type=gpu gpu_name=L40S provider=aws mode=vm launchable=pytorch startup_script=setup.sh release_action=stop target=host user=ubuntu work_root=/work/brev auth=cli"
	if !strings.Contains(text, want) {
		t.Fatalf("config show missing nvidia-brev summary: %q", text)
	}
	for _, secretFragment := range []string{"ignored-brev-secret", "token", "api_key", "password", "private_key"} {
		if strings.Contains(strings.ToLower(text), secretFragment) {
			t.Fatalf("config show text exposed %q: %q", secretFragment, text)
		}
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		NvidiaBrev struct {
			CLI           string `json:"cli"`
			Auth          string `json:"auth"`
			Org           string `json:"org"`
			Type          string `json:"type"`
			GPUName       string `json:"gpuName"`
			Provider      string `json:"provider"`
			Mode          string `json:"mode"`
			Launchable    string `json:"launchable"`
			StartupScript string `json:"startupScript"`
			ReleaseAction string `json:"releaseAction"`
			Target        string `json:"target"`
			User          string `json:"user"`
			WorkRoot      string `json:"workRoot"`
		} `json:"nvidiaBrev"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.NvidiaBrev.CLI != "/usr/local/bin/brev" ||
		got.NvidiaBrev.Auth != "cli" ||
		got.NvidiaBrev.Org != "example-org" ||
		got.NvidiaBrev.Type != "gpu" ||
		got.NvidiaBrev.GPUName != "L40S" ||
		got.NvidiaBrev.Provider != "aws" ||
		got.NvidiaBrev.Mode != "vm" ||
		got.NvidiaBrev.Launchable != "pytorch" ||
		got.NvidiaBrev.StartupScript != "setup.sh" ||
		got.NvidiaBrev.ReleaseAction != "stop" ||
		got.NvidiaBrev.Target != "host" ||
		got.NvidiaBrev.User != "ubuntu" ||
		got.NvidiaBrev.WorkRoot != "/work/brev" {
		t.Fatalf("unexpected nvidia-brev json: %#v", got.NvidiaBrev)
	}
	if strings.Contains(stdout.String(), "ignored-brev-secret") {
		t.Fatalf("config show json leaked ignored NVIDIA Brev secret env: %q", stdout.String())
	}
	nvidiaBrevJSON, err := json.Marshal(got.NvidiaBrev)
	if err != nil {
		t.Fatal(err)
	}
	for _, secretFragment := range []string{"token", "apiKey", "password", "privateKey"} {
		if strings.Contains(string(nvidiaBrevJSON), secretFragment) {
			t.Fatalf("nvidia-brev config show json exposed %q: %s", secretFragment, nvidiaBrevJSON)
		}
	}
}

func TestConfigShowAppliesNvidiaBrevGenericWorkRoot(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "nvidia-brev"
	cfg.WorkRoot = "/srv/crabbox"
	MarkWorkRootExplicit(&cfg)
	got := effectiveConfigForShow(cfg)
	if got.WorkRoot != "/srv/crabbox" || got.NvidiaBrev.WorkRoot != "/srv/crabbox" {
		t.Fatalf("workRoot=%q nvidiaBrev.workRoot=%q", got.WorkRoot, got.NvidiaBrev.WorkRoot)
	}
}

func TestConfigShowAppliesHostingerPerUserWorkRootDefault(t *testing.T) {
	other := effectiveConfigForShow(baseConfig())
	if other.Hostinger.WorkRoot != "/home/root/crabbox" ||
		other.WorkRoot != defaultPOSIXWorkRoot {
		t.Fatalf("unexpected inactive Hostinger defaults: %#v", other)
	}

	explicit := baseConfig()
	explicit.Hostinger.WorkRoot = " /home/root/crabbox "
	if got := effectiveConfigForShow(explicit).Hostinger.WorkRoot; got != explicit.Hostinger.WorkRoot {
		t.Fatalf("explicit Hostinger work root changed: %q", got)
	}

	cfg := baseConfig()
	cfg.Provider = "hostinger"
	cfg.Hostinger.User = "ubuntu"

	got := effectiveConfigForShow(cfg)
	if got.WorkRoot != "/home/ubuntu/crabbox" ||
		got.SSHUser != "ubuntu" ||
		got.Hostinger.WorkRoot != "/home/ubuntu/crabbox" {
		t.Fatalf("unexpected effective Hostinger config: %#v", got)
	}
}

func TestConfigShowPreservesExplicitDigitalOceanSSHBaseValues(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("provider: digitalocean\nssh:\n  user: crabbox\n  port: \"2222\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		SSHUser string `json:"sshUser"`
		SSHPort string `json:"sshPort"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SSHUser != "crabbox" || got.SSHPort != "2222" {
		t.Fatalf("unexpected explicit digitalocean ssh values: %#v", got)
	}
}

func TestConfigShowIncludesMorphWithoutSecret(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("MORPH_API_KEY", "morph-secret-token")
	if err := os.WriteFile(configPath, []byte("morph:\n  apiUrl: https://morph.example.test\n  snapshot: snapshot_123\n  sshGatewayHost: ssh.morph.example.test\n  workRoot: /tmp/morph\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if !strings.Contains(text, "morph api_url=https://morph.example.test snapshot=snapshot_123 ssh_gateway_host=ssh.morph.example.test work_root=/tmp/morph delete_on_release=false wake_on_ssh=true auth=configured") {
		t.Fatalf("config show missing morph summary: %q", text)
	}
	if strings.Contains(text, "morph-secret-token") {
		t.Fatalf("config show leaked Morph token: %q", text)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Morph struct {
			APIURL          string `json:"apiUrl"`
			Auth            string `json:"auth"`
			Snapshot        string `json:"snapshot"`
			SSHGatewayHost  string `json:"sshGatewayHost"`
			WorkRoot        string `json:"workRoot"`
			DeleteOnRelease bool   `json:"deleteOnRelease"`
			WakeOnSSH       bool   `json:"wakeOnSSH"`
		} `json:"morph"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Morph.APIURL != "https://morph.example.test" || got.Morph.Snapshot != "snapshot_123" || got.Morph.SSHGatewayHost != "ssh.morph.example.test" || got.Morph.WorkRoot != "/tmp/morph" || got.Morph.Auth != "configured" || got.Morph.DeleteOnRelease || !got.Morph.WakeOnSSH {
		t.Fatalf("unexpected morph json: %#v", got.Morph)
	}
	if strings.Contains(stdout.String(), "morph-secret-token") {
		t.Fatalf("config show json leaked Morph token: %q", stdout.String())
	}
}

func TestConfigShowSurfacesUnsupportedAzureDynamicSessionsPool(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("azureDynamicSessions:\n  endpoint: https://pool.env.eastus.azurecontainerapps.io\n  pool: legacy-pool\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "unsupported_pool=legacy-pool") {
		t.Fatalf("config show hid unsupported Azure Dynamic Sessions pool: %q", stdout.String())
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		AzureDynamicSessions struct {
			UnsupportedPool string `json:"unsupportedPool"`
		} `json:"azureDynamicSessions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AzureDynamicSessions.UnsupportedPool != "legacy-pool" {
		t.Fatalf("json azureDynamicSessions.unsupportedPool=%q", got.AzureDynamicSessions.UnsupportedPool)
	}
}

func TestConfigShowIncludesSyncInclude(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("sync:\n  include:\n    - src\n    - scripts\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "includes=2") {
		t.Fatalf("config show text missing includes count: %q", stdout.String())
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sync struct {
			Include []string `json:"include"`
		} `json:"sync"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sync.Include) != 2 || got.Sync.Include[0] != "src" || got.Sync.Include[1] != "scripts" {
		t.Fatalf("config show json sync.include = %#v, want [src scripts]", got.Sync.Include)
	}
}

func TestConfigShowIncludesXCPNgWithoutSecret(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	config := []byte(`xcpNg:
  apiUrl: https://xcp-ng.example.test
  username: root
  password: xcp-ng-secret
  template: ubuntu-template
  templateUuid: tpl-0001
  sr: default-sr
  srUuid: sr-0001
  network: pool-network
  networkUuid: net-0001
  host: host-0001
  user: runner
  workRoot: /work/xcp-ng
  insecureTLS: true
`)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	wantText := "xcp_ng api_url=https://xcp-ng.example.test username=root template=ubuntu-template template_uuid=tpl-0001 sr=default-sr sr_uuid=sr-0001 network=pool-network network_uuid=net-0001 host=host-0001 user=runner work_root=/work/xcp-ng insecure_tls=true auth=configured"
	if !strings.Contains(text, wantText) {
		t.Fatalf("config show missing xcp-ng summary: %q", text)
	}
	if strings.Contains(text, "xcp-ng-secret") {
		t.Fatalf("config show leaked XCP-ng password: %q", text)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		XCPNg struct {
			APIURL       string `json:"apiUrl"`
			Username     string `json:"username"`
			Auth         string `json:"auth"`
			Template     string `json:"template"`
			TemplateUUID string `json:"templateUuid"`
			SR           string `json:"sr"`
			SRUUID       string `json:"srUuid"`
			Network      string `json:"network"`
			NetworkUUID  string `json:"networkUuid"`
			Host         string `json:"host"`
			User         string `json:"user"`
			WorkRoot     string `json:"workRoot"`
			InsecureTLS  bool   `json:"insecureTLS"`
		} `json:"xcpNg"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.XCPNg.APIURL != "https://xcp-ng.example.test" || got.XCPNg.Username != "root" || got.XCPNg.Auth != "configured" || got.XCPNg.Template != "ubuntu-template" || got.XCPNg.TemplateUUID != "tpl-0001" || got.XCPNg.SR != "default-sr" || got.XCPNg.SRUUID != "sr-0001" || got.XCPNg.Network != "pool-network" || got.XCPNg.NetworkUUID != "net-0001" || got.XCPNg.Host != "host-0001" || got.XCPNg.User != "runner" || got.XCPNg.WorkRoot != "/work/xcp-ng" || !got.XCPNg.InsecureTLS {
		t.Fatalf("unexpected xcp-ng json: %#v", got.XCPNg)
	}
	if strings.Contains(stdout.String(), "xcp-ng-secret") {
		t.Fatalf("config show json leaked XCP-ng password: %q", stdout.String())
	}
}

func TestConfigShowRedactsXCPNgAPIURLUserinfo(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	config := []byte(`xcpNg:
  apiUrl: https://pool-user:pool-pass@xcp-ng.example.test/path?view=1
  username: root
  password: xcp-ng-secret
`)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	wantURL := "https://<redacted>@xcp-ng.example.test/path?view=1"
	if !strings.Contains(text, "xcp_ng api_url="+wantURL) {
		t.Fatalf("config show text missing redacted XCP-ng API URL: %q", text)
	}
	for _, secret := range []string{"pool-user", "pool-pass", "pool-user:pool-pass", "xcp-ng-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("config show text leaked %q: %q", secret, text)
		}
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		XCPNg struct {
			APIURL string `json:"apiUrl"`
			Auth   string `json:"auth"`
		} `json:"xcpNg"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.XCPNg.APIURL != wantURL || got.XCPNg.Auth != "configured" {
		t.Fatalf("unexpected xcp-ng json: %#v", got.XCPNg)
	}
	for _, secret := range []string{"pool-user", "pool-pass", "pool-user:pool-pass", "xcp-ng-secret"} {
		if strings.Contains(stdout.String(), secret) {
			t.Fatalf("config show json leaked %q: %q", secret, stdout.String())
		}
	}
}

func TestConfigShowRedactsSchemeLessXCPNgAPIURLUserinfo(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	config := []byte(`xcpNg:
  apiUrl: pool-user:pool-pass@xcp-ng.example.test/path?view=1
  username: root
  password: xcp-ng-secret
`)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	wantURL := "<redacted>@xcp-ng.example.test/path?view=1"
	if !strings.Contains(text, "xcp_ng api_url="+wantURL) {
		t.Fatalf("config show text missing redacted scheme-less XCP-ng API URL: %q", text)
	}
	for _, secret := range []string{"pool-user", "pool-pass", "pool-user:pool-pass", "xcp-ng-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("config show text leaked %q: %q", secret, text)
		}
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		XCPNg struct {
			APIURL string `json:"apiUrl"`
			Auth   string `json:"auth"`
		} `json:"xcpNg"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.XCPNg.APIURL != wantURL || got.XCPNg.Auth != "configured" {
		t.Fatalf("unexpected xcp-ng json: %#v", got.XCPNg)
	}
	for _, secret := range []string{"pool-user", "pool-pass", "pool-user:pool-pass", "xcp-ng-secret"} {
		if strings.Contains(stdout.String(), secret) {
			t.Fatalf("config show json leaked %q: %q", secret, stdout.String())
		}
	}
}

func TestRedactedConfigURLRedactsUserinfoOnMalformedURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"full URL with bad escape in host", "https://pool-user:pool-pass@%zz"},
		{"full URL with bad escape in path", "https://pool-user:pool-pass@xcp-ng.example.test/%zz"},
		{"full URL with bad port", "https://pool-user:pool-pass@xcp-ng.example.test:abc"},
		{"full URL with extra at in password", "https://pool-user:pool@pass@%zz"},
		{"full URL with slash in password", "https://pool-user:pool/pass@host/%zz"},
		{"full URL with query delimiter in password", "https://pool-user:pool?pass@host/%zz"},
		{"full URL with fragment delimiter in password", "https://pool-user:pool#pass@host/%zz"},
		{"scheme-less URL with bad escape in host", "pool-user:pool-pass@%zz"},
		{"scheme-less URL with extra at in password", "pool-user:pool@pass@%zz"},
		{"scheme-less URL with bad escape in path", "pool-user:pool-pass@%zz/path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactedConfigURL(tc.raw)
			for _, secret := range []string{"pool-user", "pool-pass", "pool/pass", "pool?pass", "pool#pass", "pool@pass", "pool-user:pool-pass"} {
				if strings.Contains(got, secret) {
					t.Fatalf("redacted URL leaked %q for %q: %s", secret, tc.raw, got)
				}
			}
		})
	}
}

func TestRoutingSafeURLRedactsUserinfoOnMalformedURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"full URL with bad escape in host", "https://pool-user:pool-pass@%zz"},
		{"full URL with bad escape in path", "https://pool-user:pool-pass@xcp-ng.example.test/%zz"},
		{"scheme-less URL with bad escape in host", "pool-user:pool-pass@%zz"},
		{"scheme-less URL with bad escape in path", "pool-user:pool-pass@%zz/path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := routingSafeURL(tc.raw)
			for _, secret := range []string{"pool-user", "pool-pass", "pool-user:pool-pass"} {
				if strings.Contains(got, secret) {
					t.Fatalf("routing URL leaked %q for %q: %s", secret, tc.raw, got)
				}
			}
		})
	}
}

func TestConfigShowIncludesDockerSandboxConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")
	if err := os.WriteFile(configPath, []byte(`provider: docker-sandbox
dockerSandbox:
  cliPath: /opt/sbx
  agent: shell
  template: ubuntu
  cpus: 2
  memory: 4g
  clone: true
  workdir: /workspace/my-app
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES", "/tmp/extra")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_MCP", "context7,all")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_KIT", "example-org/base")

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	for _, want := range []string{
		"provider=docker-sandbox",
		"docker_sandbox cli=/opt/sbx agent=shell template=ubuntu cpus=2 memory=4g clone=true workdir=/workspace/my-app",
		"extra_workspaces=/tmp/extra",
		"mcp=context7,all",
		"kit=example-org/base",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config show text missing %q: %q", want, text)
		}
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Provider      string `json:"provider"`
		DockerSandbox struct {
			CLIPath         string   `json:"cliPath"`
			Agent           string   `json:"agent"`
			Template        string   `json:"template"`
			CPUs            float64  `json:"cpus"`
			Memory          string   `json:"memory"`
			Clone           bool     `json:"clone"`
			Workdir         string   `json:"workdir"`
			ExtraWorkspaces []string `json:"extraWorkspaces"`
			MCP             []string `json:"mcp"`
			Kit             []string `json:"kit"`
		} `json:"dockerSandbox"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "docker-sandbox" || got.DockerSandbox.CLIPath != "/opt/sbx" || got.DockerSandbox.Agent != "shell" || got.DockerSandbox.Template != "ubuntu" || got.DockerSandbox.CPUs != 2 || got.DockerSandbox.Memory != "4g" || !got.DockerSandbox.Clone || got.DockerSandbox.Workdir != "/workspace/my-app" {
		t.Fatalf("unexpected dockerSandbox json: %#v", got)
	}
	if strings.Join(got.DockerSandbox.ExtraWorkspaces, ",") != "/tmp/extra" || strings.Join(got.DockerSandbox.MCP, ",") != "context7,all" || strings.Join(got.DockerSandbox.Kit, ",") != "example-org/base" {
		t.Fatalf("unexpected dockerSandbox lists: %#v", got.DockerSandbox)
	}
}

func TestConfigShowRejectsInvalidDockerSandboxCPUConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "docker-sandbox")
	if err := os.WriteFile(configPath, []byte("profile: default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CLI", "/opt/docker-sbx")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_AGENT", "shell")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_TEMPLATE", "ubuntu")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CPUS", "2.5")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_MEMORY", "6g")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CLONE", "true")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_WORKDIR", "/workspace/my-app")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES", "/tmp/extra")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_KIT", "example-org/base")

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err == nil || !strings.Contains(err.Error(), "docker-sandbox cpus must be a whole number") {
		t.Fatalf("configShow err=%v, want docker-sandbox whole-number validation", err)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err == nil || !strings.Contains(err.Error(), "docker-sandbox cpus must be a whole number") {
		t.Fatalf("configShow --json err=%v, want docker-sandbox whole-number validation", err)
	}
}
