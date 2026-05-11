package cli

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAzureLoginWritesConfigFields(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)

	file := fileConfig{}
	if file.Azure == nil {
		file.Azure = &fileAzureConfig{}
	}
	file.Azure.SubscriptionID = "00000000-0000-0000-0000-000000000001"
	file.Azure.TenantID = "00000000-0000-0000-0000-000000000002"
	file.Azure.Location = "westus2"
	file.Provider = "azure"

	written, err := writeUserFileConfig(file)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != cfgPath {
		t.Fatalf("got path=%q want %q", written, cfgPath)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var readBack fileConfig
	if err := yaml.Unmarshal(data, &readBack); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if readBack.Provider != "azure" {
		t.Fatalf("got provider=%q want azure", readBack.Provider)
	}
	if readBack.Azure == nil {
		t.Fatal("azure config section is nil")
	}
	if readBack.Azure.SubscriptionID != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("got subscriptionId=%q", readBack.Azure.SubscriptionID)
	}
	if readBack.Azure.TenantID != "00000000-0000-0000-0000-000000000002" {
		t.Fatalf("got tenantId=%q", readBack.Azure.TenantID)
	}
	if readBack.Azure.Location != "westus2" {
		t.Fatalf("got location=%q", readBack.Azure.Location)
	}
}

func TestAzureLoginPreservesExistingConfig(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)

	initial := fileConfig{
		Provider: "hetzner",
		Broker:   &fileBrokerConfig{URL: "https://crabbox.openclaw.ai", Token: "tok"},
	}
	data, _ := yaml.Marshal(initial)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	file, err := readFileConfig(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if file.Azure == nil {
		file.Azure = &fileAzureConfig{}
	}
	file.Azure.SubscriptionID = "sub-1"
	file.Azure.TenantID = "tenant-1"
	file.Azure.Location = "eastus"
	// Don't overwrite provider since it's already set
	if _, err := writeUserFileConfig(file); err != nil {
		t.Fatalf("write config: %v", err)
	}

	readBack, err := readFileConfig(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if readBack.Provider != "hetzner" {
		t.Fatalf("provider was overwritten: got %q", readBack.Provider)
	}
	if readBack.Broker == nil || readBack.Broker.Token != "tok" {
		t.Fatal("broker config was lost")
	}
	if readBack.Azure == nil || readBack.Azure.SubscriptionID != "sub-1" {
		t.Fatal("azure config was not written")
	}
}
