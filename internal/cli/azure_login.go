package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

func (a App) azureLogin(ctx context.Context, args []string) error {
	fs := newFlagSet("azure login", a.Stderr)
	subscription := fs.String("subscription", "", "Azure subscription ID or name (default: active az CLI subscription)")
	location := fs.String("location", "eastus", "Azure location for provisioning")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	info, err := azAccountShow(ctx, *subscription)
	if err != nil {
		return exit(3, "%v", err)
	}

	fmt.Fprintf(a.Stderr, "validating azure credentials for subscription %q (%s)...\n", info.Name, info.ID)
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return exit(3, "azure credential: %v", err)
	}
	_, err = cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return exit(3, "azure token acquisition failed: %v\nRun 'az login' to authenticate.", err)
	}

	path := writableConfigPath()
	if path == "" {
		return exit(2, "user config directory is unavailable")
	}
	file, err := readFileConfig(path)
	if err != nil {
		return err
	}
	if file.Azure == nil {
		file.Azure = &fileAzureConfig{}
	}
	file.Azure.SubscriptionID = info.ID
	file.Azure.TenantID = info.TenantID
	if *location != "" {
		file.Azure.Location = *location
	}
	if file.Provider == "" {
		file.Provider = "azure"
	}
	written, err := writeUserFileConfig(file)
	if err != nil {
		return err
	}

	if *jsonOut {
		result := map[string]string{
			"subscription": info.ID,
			"tenant":       info.TenantID,
			"name":         info.Name,
			"location":     *location,
			"configPath":   written,
		}
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Fprintf(a.Stdout, "ok  subscription=%s (%s) tenant=%s location=%s\n", info.ID, info.Name, info.TenantID, *location)
	fmt.Fprintf(a.Stdout, "config written to %s\n", written)
	fmt.Fprintf(a.Stdout, "\nYou can now run:\n  crabbox warmup --provider azure\n  crabbox run --provider azure -- <command>\n")
	return nil
}
