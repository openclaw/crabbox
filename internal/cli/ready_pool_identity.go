package cli

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	readyPoolIdentitySchemaV1  = "crabbox-ready-pool-identity/v1"
	readyPoolSeedFieldMaxBytes = 1024
	readyPoolIdentityValueMax  = 1024
)

var readyPoolDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func loadReadyPoolIdentity(path string) (CoordinatorReadyPoolIdentityV1, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return CoordinatorReadyPoolIdentityV1{}, exit(2, "typed ready-pool operations require a nonempty --identity-file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CoordinatorReadyPoolIdentityV1{}, fmt.Errorf("read ready-pool identity: %w", err)
	}
	var identity CoordinatorReadyPoolIdentityV1
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		return CoordinatorReadyPoolIdentityV1{}, fmt.Errorf("decode ready-pool identity: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return CoordinatorReadyPoolIdentityV1{}, fmt.Errorf("decode ready-pool identity: trailing JSON value")
	}
	if err := validateReadyPoolIdentity(identity); err != nil {
		return CoordinatorReadyPoolIdentityV1{}, err
	}
	return identity, nil
}

func validateReadyPoolIdentity(identity CoordinatorReadyPoolIdentityV1) error {
	if identity.Schema != readyPoolIdentitySchemaV1 {
		return exit(2, "unsupported ready-pool identity schema %q", identity.Schema)
	}
	if _, err := validateReadyPoolIdentityProvider(identity.Image.Provider); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"image.scope":        identity.Image.Scope,
		"image.id":           identity.Image.ID,
		"cacheCompatibility": identity.CacheCompatibility,
	} {
		if strings.TrimSpace(value) != value || value == "" || len(value) > readyPoolIdentityValueMax || !utf8.ValidString(value) {
			return exit(2, "ready-pool identity %s must be a nonempty canonical UTF-8 value", name)
		}
	}
	architecture, err := normalizeArchitecture(identity.Architecture)
	if err != nil || architecture != identity.Architecture {
		return exit(2, "ready-pool identity architecture must be canonical amd64 or arm64")
	}
	if !readyPoolDigestPattern.MatchString(identity.SeedDigest) {
		return exit(2, "ready-pool identity seedDigest must be sha256:<64 lowercase hex>")
	}
	return nil
}

func validateReadyPoolIdentityProvider(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", exit(2, "ready-pool identity image.provider must be a canonical provider name")
	}
	provider, err := ProviderFor(value)
	if err != nil || provider.Name() != value {
		return "", exit(2, "ready-pool identity image.provider must be a canonical provider name")
	}
	if provider.Spec().Coordinator != CoordinatorSupported {
		return "", exit(2, "ready-pool identity provider %q does not support coordinator-managed leases", value)
	}
	return provider.Name(), nil
}

func readyPoolSeedDigest(repo, ref, commit, fingerprint string) (string, error) {
	hash := sha256.New()
	_, _ = hash.Write([]byte("crabbox-ready-pool-seed/v1\x00"))
	for index, field := range []struct {
		name  string
		value string
	}{
		{name: "repo", value: repo},
		{name: "ref", value: ref},
		{name: "commit", value: commit},
		{name: "fingerprint", value: fingerprint},
	} {
		if !utf8.ValidString(field.value) {
			return "", exit(2, "ready-pool seed %s must be valid UTF-8", field.name)
		}
		data := []byte(field.value)
		if len(data) > readyPoolSeedFieldMaxBytes {
			return "", exit(2, "ready-pool seed %s exceeds %d UTF-8 bytes", field.name, readyPoolSeedFieldMaxBytes)
		}
		_, _ = hash.Write([]byte{byte(index + 1)})
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(data)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(data)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func validateReadyPoolSeedIdentity(identity CoordinatorReadyPoolIdentityV1, input map[string]any) error {
	seedField := func(name string) string {
		value, _ := input[name].(string)
		return value
	}
	digest, err := readyPoolSeedDigest(
		seedField("repo"), seedField("ref"), seedField("commit"), seedField("fingerprint"),
	)
	if err != nil {
		return err
	}
	if digest != identity.SeedDigest {
		return exit(2, "ready-pool identity seedDigest does not match repo/ref/commit/fingerprint")
	}
	return nil
}

func readyPoolIdentitiesEqual(left, right CoordinatorReadyPoolIdentityV1) bool {
	return left == right
}

func validateTypedReadyPoolResponseIdentity(response CoordinatorReadyPoolResponse, expected CoordinatorReadyPoolIdentityV1) error {
	if response.Entry.Identity == nil || !readyPoolIdentitiesEqual(*response.Entry.Identity, expected) {
		return exit(7, "coordinator returned a mismatched typed ready-pool identity")
	}
	return nil
}

func readyPoolIdentityMatchesLease(identity CoordinatorReadyPoolIdentityV1, lease CoordinatorLease) error {
	if lease.TargetOS != targetLinux {
		return exit(2, "typed ready pools currently require a native Linux lease")
	}
	provider, err := ProviderFor(identity.Image.Provider)
	if err != nil {
		return exit(7, "coordinator lease provider, immutable image, or scope does not match ready-pool identity")
	}
	if err := readyPoolIdentityMatchesLeaseWithProvider(provider, identity, lease); err != nil {
		return err
	}
	if lease.Architecture != identity.Architecture {
		return exit(7, "coordinator lease architecture does not match ready-pool identity")
	}
	return nil
}

func readyPoolIdentityMatchesLeaseWithProvider(provider Provider, identity CoordinatorReadyPoolIdentityV1, lease CoordinatorLease) error {
	capability, ok := provider.(ProviderReadyPoolImageIdentityCapability)
	if !ok || !capability.ReadyPoolImageIdentityMatchesLease(ProviderReadyPoolImageIdentityRequest{
		Identity: identity.Image,
		Lease: ProviderReadyPoolLeaseImageIdentity{
			Provider: lease.Provider,
			Region:   lease.Region,
			Project:  lease.ProviderProject,
			Image:    lease.Image,
		},
	}) {
		return exit(7, "coordinator lease provider, immutable image, or scope does not match ready-pool identity")
	}
	return nil
}
