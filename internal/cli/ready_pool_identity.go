package cli

import (
	"context"
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
	linuxReadinessSchemaV1     = "crabbox-linux-readiness/v1"
	readyPoolSeedFieldMaxBytes = 1024
)

var readyPoolDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func loadReadyPoolIdentity(path string) (CoordinatorReadyPoolIdentityV1, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return CoordinatorReadyPoolIdentityV1{}, exit(2, "typed ready-pool operations require --identity-file")
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
	if !validReadyPoolIdentityName(identity.Profile) {
		return exit(2, "ready-pool identity profile is invalid")
	}
	architecture, err := normalizeArchitecture(identity.Architecture)
	if err != nil || architecture != identity.Architecture {
		return exit(2, "ready-pool identity architecture must be canonical amd64 or arm64")
	}
	if strings.TrimSpace(identity.ImageID) == "" || len(identity.ImageID) > 1024 {
		return exit(2, "ready-pool identity imageID is invalid")
	}
	for name, digest := range map[string]string{
		"recipeDigest":    identity.RecipeDigest,
		"inventoryDigest": identity.InventoryDigest,
		"seedDigest":      identity.SeedDigest,
		"cacheABIDigest":  identity.CacheABIDigest,
	} {
		if !readyPoolDigestPattern.MatchString(digest) {
			return exit(2, "ready-pool identity %s must be sha256:<64 lowercase hex>", name)
		}
	}
	return nil
}

func validReadyPoolIdentityName(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("._/-", r) {
			if i > 0 || ((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				continue
			}
		}
		return false
	}
	return true
}

func readReadyPoolReadinessEvidence(ctx context.Context, target SSHTarget) (CoordinatorReadyPoolReadinessEvidence, error) {
	if target.TargetOS != targetLinux {
		return CoordinatorReadyPoolReadinessEvidence{}, exit(2, "typed ready pools currently require a native Linux target")
	}
	out, err := runSSHOutput(ctx, target, linuxReadinessEvidenceCommand)
	if err != nil {
		return CoordinatorReadyPoolReadinessEvidence{}, exit(7, "verify fresh Linux readiness evidence: %v", err)
	}
	var evidence CoordinatorReadyPoolReadinessEvidence
	decoder := json.NewDecoder(strings.NewReader(out))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return CoordinatorReadyPoolReadinessEvidence{}, exit(7, "decode fresh Linux readiness manifest: %v", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return CoordinatorReadyPoolReadinessEvidence{}, exit(7, "decode fresh Linux readiness manifest: trailing JSON value")
	}
	if evidence.Schema != linuxReadinessSchemaV1 ||
		!validReadyPoolIdentityName(evidence.Profile) ||
		!readyPoolDigestPattern.MatchString(evidence.RecipeDigest) ||
		!readyPoolDigestPattern.MatchString(evidence.InventoryDigest) {
		return CoordinatorReadyPoolReadinessEvidence{}, exit(7, "fresh Linux readiness manifest is invalid")
	}
	return evidence, nil
}

func validateReadyPoolReadinessIdentity(identity CoordinatorReadyPoolIdentityV1, evidence CoordinatorReadyPoolReadinessEvidence) error {
	if identity.Profile != evidence.Profile ||
		identity.RecipeDigest != evidence.RecipeDigest ||
		identity.InventoryDigest != evidence.InventoryDigest {
		return exit(7, "fresh Linux readiness manifest does not match the requested ready-pool identity")
	}
	return nil
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

func validateReadyPoolSeedIdentity(identity CoordinatorReadyPoolIdentityV1, repo, ref, commit, fingerprint string) error {
	got, err := readyPoolSeedDigest(repo, ref, commit, fingerprint)
	if err != nil {
		return err
	}
	if got != identity.SeedDigest {
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
	if lease.Image == nil || strings.TrimSpace(lease.Image.ID) != identity.ImageID {
		return exit(7, "coordinator lease image does not match ready-pool identity")
	}
	if strings.TrimSpace(lease.Architecture) != identity.Architecture {
		return exit(7, "coordinator lease architecture does not match ready-pool identity")
	}
	return nil
}
