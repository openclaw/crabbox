package cua

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	leasePrefix      = "cuabx_"
	scopePrefix      = "cua-account-sha256:"
	labelSandboxName = "cua.sandbox.name"
	labelCreatedAt   = "cua.created-at"
	labelImage       = "cua.image"
	labelKind        = "cua.kind"
	labelRegion      = "cua.region"
	labelWorkdir     = "cua.workdir"
	labelTTLSeconds  = "cua.ttl-seconds"
	labelMissing     = "cua.missing"
)

func newCUALeaseID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return leasePrefix + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000"), ".", "")
	}
	return leasePrefix + hex.EncodeToString(b[:])
}

func cuaScope(cfg Config) (string, error) {
	apiURL, err := cuaAPIURL(cfg)
	if err != nil {
		return "", err
	}
	if apiURL == "" {
		apiURL = "sdk-default"
	}
	return scopePrefix + hashScope(apiURL, cuaAPIKey()), nil
}

func cuaAPIKey() string {
	if key := os.Getenv("CRABBOX_CUA_API_KEY"); key != "" {
		return key
	}
	return os.Getenv("CUA_API_KEY")
}

func claimLabels(cfg Config, sandboxName, createdAt string, missing bool) map[string]string {
	workdir, _ := cuaWorkdir(cfg)
	labels := map[string]string{
		labelSandboxName: sandboxName,
		labelImage:       strings.TrimSpace(blank(cfg.Cua.Image, defaultImage)),
		labelKind:        strings.ToLower(strings.TrimSpace(blank(cfg.Cua.Kind, defaultKind))),
		labelRegion:      strings.TrimSpace(cfg.Cua.Region),
		labelWorkdir:     workdir,
		labelCreatedAt:   strings.TrimSpace(createdAt),
	}
	if cfg.TTL > 0 {
		labels[labelTTLSeconds] = fmt.Sprintf("%d", int64(cfg.TTL/time.Second))
	}
	if missing {
		labels[labelMissing] = "true"
	}
	return labels
}

func claimSandboxName(claim LeaseClaim) string {
	if claim.CloudID != "" {
		return claim.CloudID
	}
	if claim.Labels != nil {
		return strings.TrimSpace(claim.Labels[labelSandboxName])
	}
	return ""
}

func resolveCUALeaseClaim(identifier string, cfg Config) (LeaseClaim, bool, error) {
	scope, err := cuaScope(cfg)
	if err != nil {
		return LeaseClaim{}, false, err
	}
	if claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(identifier, providerName); err != nil {
		return LeaseClaim{}, false, err
	} else if ok {
		if !exact && !strings.HasPrefix(claim.LeaseID, leasePrefix) {
			return LeaseClaim{}, false, nil
		}
		if err := validateClaimScope(claim, scope); err != nil {
			return LeaseClaim{}, false, err
		}
		return claim, true, nil
	}
	if !strings.HasPrefix(identifier, leasePrefix) {
		exact := leasePrefix + identifier
		if claim, err := core.ReadLeaseClaim(exact); err != nil {
			return LeaseClaim{}, false, err
		} else if claim.LeaseID == exact && claim.Provider == providerName {
			if err := validateClaimScope(claim, scope); err != nil {
				return LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	return LeaseClaim{}, false, nil
}

func validateClaimScope(claim LeaseClaim, expected string) error {
	if claim.Provider != providerName {
		return exit(4, "lease %q is not a CUA claim", claim.LeaseID)
	}
	if claim.ProviderScope != expected {
		return exit(4, "CUA lease %q belongs to a different API scope; restore the API URL used to create it", claim.LeaseID)
	}
	return nil
}

func validateSandboxOwnership(claim LeaseClaim, sandbox bridgeSandboxSummary, expectedScope string) error {
	if err := validateClaimScope(claim, expectedScope); err != nil {
		return err
	}
	expectedName := claimSandboxName(claim)
	if expectedName == "" {
		return exit(4, "CUA lease %q is missing its claimed sandbox name", claim.LeaseID)
	}
	identities := []string{strings.TrimSpace(sandbox.Name), strings.TrimSpace(sandbox.ID)}
	seenIdentity := false
	for _, actual := range identities {
		if actual == "" {
			continue
		}
		seenIdentity = true
		if actual != expectedName {
			return exit(4, "CUA sandbox %q does not match local claim %q", actual, expectedName)
		}
	}
	if !seenIdentity {
		return exit(4, "CUA sandbox response has no identity for local claim %q", expectedName)
	}
	expectedCreatedAt := strings.TrimSpace(claim.Labels[labelCreatedAt])
	actualCreatedAt := strings.TrimSpace(sandbox.Metadata["createdAt"])
	if expectedCreatedAt == "" || actualCreatedAt == "" || actualCreatedAt != expectedCreatedAt {
		return exit(4, "CUA sandbox %q creation identity does not match its local claim", expectedName)
	}
	return nil
}

func claimIsMissing(claim LeaseClaim) bool {
	return claim.Labels != nil && claim.Labels[labelMissing] == "true"
}
