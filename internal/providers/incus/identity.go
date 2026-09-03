package incus

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lxc/incus/v7/shared/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

type connectionIdentity struct {
	Endpoint    string `json:"endpoint"`
	Project     string `json:"project"`
	Certificate string `json:"certificate"`
}

func (s connectionIdentity) scope() string {
	data, _ := json.Marshal(s)
	return "incus:" + string(data)
}

const connectionLabel = "incus_connection"
const identityLabel = "incus_identity"

func connectionMetadata(cfg Config, identity connectionIdentity) map[string]string {
	// Config stores paths to existing trust material, never certificate keys or tokens.
	config := cfg.Incus
	config.CheckpointMetadata = nil
	data, _ := json.Marshal(config)
	return map[string]string{connectionLabel: string(data), identityLabel: identity.scope()}
}

func configFromMetadata(metadata map[string]string) (Config, error) {
	cfg := core.BaseConfig()
	if metadata[connectionLabel] == "" || metadata[identityLabel] == "" {
		return cfg, fmt.Errorf("Incus resource has no recorded connection identity")
	}
	if err := json.Unmarshal([]byte(metadata[connectionLabel]), &cfg.Incus); err != nil {
		return cfg, fmt.Errorf("decode Incus connection: %w", err)
	}
	cfg.Provider = providerName
	applyDefaults(&cfg)
	return cfg, nil
}

func verifyConnection(client instanceClient, expected string) error {
	identity, err := client.Identity()
	if err != nil {
		return err
	}
	if expected == "" || identity.scope() != expected {
		return core.Exit(4, "Incus connection identity mismatch (endpoint, project, or daemon certificate); refusing to retarget recorded resource")
	}
	return nil
}

func validateClaimInstance(client instanceClient, claim core.LeaseClaim, inst api.Instance) error {
	labels := labelsFromInstance(inst)
	if claim.FixedCreateIntent == nil {
		// Retained leases made before durable Incus intents keep their original scope.
		if claim.Provider != providerName || claim.ProviderScope != instanceScope(inst.Name) || claim.LeaseID != labels["lease"] {
			return core.Exit(4, "Incus legacy lease claim scope mismatch")
		}
		return nil
	}
	if err := verifyConnection(client, claim.ProviderScope); err != nil {
		return err
	}
	intent := claim.FixedCreateIntent
	if claim.Provider != providerName || labels["provider"] != providerName || claim.LeaseID != labels["lease"] || !isCrabboxInstance(inst) ||
		intent.ProviderScope != claim.ProviderScope || intent.State == "released" || claim.Slug != intent.Slug ||
		inst.Name != intent.Attempt["name"] || inst.Config["volatile.uuid"] != intent.Attempt["uuid"] || intent.Attempt["uuid"] == "" ||
		labels["fixed_intent_sha256"] != intent.Fingerprint || labels[identityLabel] != claim.ProviderScope || labels["incus_uuid"] != intent.Attempt["uuid"] ||
		labels["slug"] != intent.Slug || (claim.CloudID != "" && claim.CloudID != inst.Name) ||
		(claim.CloudImmutableID != "" && claim.CloudImmutableID != inst.Config["volatile.uuid"]) {
		return core.Exit(4, "Incus lease ownership mismatch for %s; refusing to retarget resource", inst.Name)
	}
	return nil
}

func claimScopeForInstance(inst api.Instance) string {
	if scope := strings.TrimSpace(inst.Config[labelKey(identityLabel)]); scope != "" {
		return scope
	}
	return instanceScope(inst.Name)
}

func validateResolvedInstance(client instanceClient, inst api.Instance, leaseID string) error {
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !exists {
		if inst.Config[labelKey(identityLabel)] != "" {
			return core.Exit(4, "Incus instance %s requires its durable ownership claim", inst.Name)
		}
		return nil
	}
	return validateClaimInstance(client, claim, inst)
}

// Acquisition commits readiness only after fork identity and SSH preparation.
// Inspect and release still validate ownership without requiring readiness.
func requireAcquiredIntent(claim core.LeaseClaim) error {
	if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.State != "acquired" {
		return core.Exit(4, "Incus lease acquisition is incomplete; replay warmup with the original intent or stop the lease")
	}
	return nil
}
