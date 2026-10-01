package applecontainer

import (
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAppleContainerAdvertisesFixedLeaseIDs(t *testing.T) {
	b := testBackend(&recordingRunner{})
	capability, ok := any(b).(core.IdempotentLeaseIDBackend)
	if !ok || !capability.SupportsRequestedLeaseID() {
		t.Fatal("apple-container must advertise idempotent fixed lease acquisitions")
	}
}

func TestFixedAppleContainerFingerprintBindsCreateIntent(t *testing.T) {
	b := testBackend(&recordingRunner{})
	cfg := b.configForRun()
	req := core.AcquireRequest{
		RequestedLeaseID: "cbx_123456789abc",
		RequestedSlug:    "rivet",
		Keep:             true,
	}
	first, err := fixedAppleContainerFingerprint(cfg, req, "ssh-ed25519 AAAA fixture")
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixedAppleContainerFingerprint(cfg, req, "ssh-ed25519 AAAA fixture")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("fingerprint is not stable sha256: first=%q second=%q", first, second)
	}
	cfg.AppleContainer.Memory = "16g"
	changed, err := fixedAppleContainerFingerprint(cfg, req, "ssh-ed25519 AAAA fixture")
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("memory change did not change fixed create intent fingerprint")
	}
}

func TestValidateFixedAppleContainerRejectsIntentMismatch(t *testing.T) {
	b := testBackend(&recordingRunner{})
	cfg := b.configForRun()
	leaseID := "cbx_123456789abc"
	slug := "rivet"
	fingerprint := strings.Repeat("a", 64)
	name := core.LeaseProviderName(leaseID, slug)
	container := inspectContainer{
		Status: inspectStatus{State: "running"},
		Configuration: inspectConfiguration{
			ID:    name,
			Image: inspectImage{Reference: cfg.AppleContainer.Image},
			Labels: map[string]string{
				"crabbox":             "true",
				"provider":            providerName,
				"lease":               leaseID,
				"slug":                slug,
				"pond":                "",
				"image":               cfg.AppleContainer.Image,
				"fixed_intent_sha256": fingerprint,
			},
		},
	}
	if err := validateFixedAppleContainer(container, cfg, leaseID, slug, fingerprint); err != nil {
		t.Fatalf("matching fixed container rejected: %v", err)
	}
	container.Configuration.Labels["fixed_intent_sha256"] = strings.Repeat("b", 64)
	if err := validateFixedAppleContainer(container, cfg, leaseID, slug, fingerprint); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("intent mismatch error=%v, want lease_id_conflict", err)
	}
}

func TestFixedAppleContainerTerminalClaimCannotReplay(t *testing.T) {
	previous := core.LeaseClaim{
		LeaseID:       "cbx_123456789abc",
		Slug:          "rivet",
		Provider:      providerName,
		ProviderScope: "apple-container:cli:container",
		ClaimedAt:     time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		FixedCreateIntent: &core.FixedCreateIntent{
			Version:       fixedAppleContainerIntentVersion,
			Fingerprint:   strings.Repeat("a", 64),
			ProviderScope: "apple-container:cli:container",
			Slug:          "rivet",
			CreatedAt:     time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
			State:         "acquired",
		},
	}
	terminal := fixedAppleContainerLeaseKind.TerminalClaim(previous, time.Now().UTC())
	if err := fixedAppleContainerLeaseKind.ValidateTerminalClaim(terminal, previous, previous.LeaseID, nil); err != nil {
		t.Fatalf("terminal claim rejected: %v", err)
	}
	if terminal.FixedCreateIntent.State != "released" || terminal.CloudID != "" || len(terminal.Labels) != 0 {
		t.Fatalf("invalid terminal tombstone: %#v", terminal)
	}
}
