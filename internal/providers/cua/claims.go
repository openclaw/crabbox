package cua

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	core "github.com/openclaw/crabbox/internal/cli"
	"strings"
	"time"
)

const (
	leasePrefix       = "cuabx_"
	sandboxNamePrefix = "crabbox-cua-"
	scopePrefix       = "cua-api-sha256:"
	labelSandboxName  = "cua.sandbox.name"
	labelImage        = "cua.image"
	labelKind         = "cua.kind"
	labelRegion       = "cua.region"
	labelWorkdir      = "cua.workdir"
	labelMissing      = "cua.missing"
	labelState        = "state"
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
	return scopePrefix + hashScope(apiURL), nil
}

func newSandboxName(leaseID, slug string) string {
	base := normalizeLeaseSlug(slug)
	if base == "" {
		base = normalizeLeaseSlug(leaseID)
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(sandboxNamePrefix, "-")+"-")
	const suffixLen = 6
	maxBase := 63 - len(sandboxNamePrefix) - suffixLen - 1
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "lease"
	}
	return fmt.Sprintf("%s%s-%s", sandboxNamePrefix, base, hashScope(leaseID)[:suffixLen])
}

func claimLabels(cfg Config, sandboxName string, missing bool) map[string]string {
	workdir, _ := cuaWorkdir(cfg)
	labels := map[string]string{
		labelSandboxName: sandboxName,
		labelImage:       strings.TrimSpace(blank(cfg.Cua.Image, defaultImage)),
		labelKind:        strings.ToLower(strings.TrimSpace(blank(cfg.Cua.Kind, defaultKind))),
		labelRegion:      strings.TrimSpace(cfg.Cua.Region),
		labelWorkdir:     workdir,
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

func resolveClaim(identifier string, cfg Config) (LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return LeaseClaim{}, false, exit(2, "provider=cua requires a Crabbox-created sandbox slug or %s lease id", leasePrefix)
	}
	if claim, ok, err := resolveCUALeaseClaim(identifier, cfg); err != nil || ok {
		return claim, ok, err
	}
	return LeaseClaim{}, false, exit(4, "CUA sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s lease id", identifier, leasePrefix)
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
	actualName := strings.TrimSpace(blank(sandbox.Name, sandbox.ID))
	if actualName != "" && actualName != expectedName {
		return exit(4, "CUA sandbox %q does not match local claim %q", actualName, expectedName)
	}
	if !strings.HasPrefix(expectedName, sandboxNamePrefix) {
		return exit(4, "CUA sandbox %q is not in the Crabbox-owned name prefix", expectedName)
	}
	return nil
}

func claimIsMissing(claim LeaseClaim) bool {
	return claim.Labels != nil && claim.Labels[labelMissing] == "true"
}

func claimCleanupInProgress(claim LeaseClaim) bool {
	return claim.Labels != nil && strings.EqualFold(strings.TrimSpace(claim.Labels[labelState]), "cleanup")
}

func normalizeLeaseSlug(value string) string {
	return strings.TrimSpace(coreNormalizeLeaseSlug(value))
}

func coreNormalizeLeaseSlug(value string) string {
	return strings.ToLower(strings.Trim(strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			if r >= 'A' && r <= 'Z' {
				return r + ('a' - 'A')
			}
			return r
		}
		return '-'
	}, value), "-"))
}
