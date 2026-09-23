package azuredynamicsessions

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "azure-dynamic-sessions"
	targetLinux  = core.TargetLinux

	networkPublic = core.NetworkPublic

	dynamicSessionsAudience = "https://dynamicsessions.io"
	tokenEnvName            = "CRABBOX_AZURE_DYNAMIC_SESSIONS_TOKEN"
	exitSentinel            = "__crabbox_azds_exit_code__="
)

func validateNativeCredentialDestination(cfg core.Config) error {
	return core.ValidateNativeCredentialDestination(cfg, providerName)
}

func newSessionID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "azds-" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000"), ".", "")
	}
	return "azds-" + hex.EncodeToString(b[:])
}

func azureDynamicSessionsCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " " + core.ShellQuote(leaseID)
}

func providerError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", providerName, action, err)
}
