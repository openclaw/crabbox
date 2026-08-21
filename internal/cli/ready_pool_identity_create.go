package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	readyPoolAWSImagePattern        = regexp.MustCompile(`^ami-[0-9a-z]+$`)
	readyPoolAWSInstanceTypePattern = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
)

type readyPoolIdentityCreateExpected struct {
	ImageID      string
	ServerType   string
	Architecture string
	Profile      string
	RecipeDigest string
}

func (a App) readyPoolIdentityCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("pool identity create", a.Stderr)
	id := fs.String("id", "", "live lease id")
	repo := fs.String("repo", "", "repository owner/name")
	ref := fs.String("ref", "", "source ref")
	commit := fs.String("commit", "", "source commit")
	fingerprint := fs.String("fingerprint", "", "repo setup fingerprint")
	expectedImage := fs.String("expected-image", "", "expected immutable AMI id")
	expectedType := fs.String("expected-type", "", "expected AWS instance type")
	expectedArchitecture := fs.String("expected-architecture", "", "expected architecture")
	expectedProfile := fs.String("expected-profile", "", "expected readiness profile")
	expectedRecipeDigest := fs.String("expected-recipe-digest", "", "expected readiness recipe digest")
	cacheABIDigest := fs.String("cache-abi-digest", "", "cache ABI digest")
	output := fs.String("output", "", "new identity JSON path")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"--id":                     *id,
		"--repo":                   *repo,
		"--ref":                    *ref,
		"--commit":                 *commit,
		"--fingerprint":            *fingerprint,
		"--expected-image":         *expectedImage,
		"--expected-type":          *expectedType,
		"--expected-architecture":  *expectedArchitecture,
		"--expected-profile":       *expectedProfile,
		"--expected-recipe-digest": *expectedRecipeDigest,
		"--cache-abi-digest":       *cacheABIDigest,
		"--output":                 *output,
	} {
		if strings.TrimSpace(value) == "" {
			return exit(2, "%s is required", name)
		}
	}
	if !readyPoolDigestPattern.MatchString(*expectedRecipeDigest) {
		return exit(2, "--expected-recipe-digest must be sha256:<64 lowercase hex>")
	}
	if !readyPoolDigestPattern.MatchString(*cacheABIDigest) {
		return exit(2, "--cache-abi-digest must be sha256:<64 lowercase hex>")
	}
	if !readyPoolAWSImagePattern.MatchString(strings.TrimSpace(*expectedImage)) {
		return exit(2, "--expected-image must be an exact AMI id")
	}
	if !readyPoolAWSInstanceTypePattern.MatchString(strings.TrimSpace(*expectedType)) {
		return exit(2, "--expected-type must be an exact AWS instance type")
	}
	if !isGitCommitSHA(strings.TrimSpace(*commit)) {
		return exit(2, "--commit must be an exact 40-character Git commit")
	}
	architecture, err := normalizeArchitecture(*expectedArchitecture)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	coord, err := readyPoolCoordinatorFromConfig(cfg)
	if err != nil {
		return err
	}
	lease, err := coord.GetLease(ctx, strings.TrimSpace(*id))
	if err != nil {
		return err
	}
	expected := readyPoolIdentityCreateExpected{
		ImageID:      strings.TrimSpace(*expectedImage),
		ServerType:   strings.TrimSpace(*expectedType),
		Architecture: architecture,
		Profile:      strings.TrimSpace(*expectedProfile),
		RecipeDigest: strings.TrimSpace(*expectedRecipeDigest),
	}
	if err := validateReadyPoolIdentityCreateLease(lease, expected); err != nil {
		return err
	}
	_, target, _ := leaseToServerTarget(lease, cfg)
	if err := prepareLeaseSSHTrust(&target, lease.ID); err != nil {
		return err
	}
	evidence, err := readReadyPoolReadinessEvidence(ctx, target)
	if err != nil {
		return err
	}
	if evidence.Profile != expected.Profile || evidence.RecipeDigest != expected.RecipeDigest {
		return exit(7, "fresh Linux readiness manifest does not match the expected profile and recipe")
	}
	seedDigest, err := readyPoolSeedDigest(
		strings.TrimSpace(*repo),
		strings.TrimSpace(*ref),
		strings.TrimSpace(*commit),
		strings.TrimSpace(*fingerprint),
	)
	if err != nil {
		return err
	}
	identity := CoordinatorReadyPoolIdentityV1{
		Schema:          readyPoolIdentitySchemaV1,
		Profile:         evidence.Profile,
		RecipeDigest:    evidence.RecipeDigest,
		InventoryDigest: evidence.InventoryDigest,
		ImageID:         expected.ImageID,
		Architecture:    expected.Architecture,
		SeedDigest:      seedDigest,
		CacheABIDigest:  strings.TrimSpace(*cacheABIDigest),
	}
	if err := validateReadyPoolIdentity(identity); err != nil {
		return err
	}
	if err := writeReadyPoolIdentityAtomic(*output, identity); err != nil {
		return err
	}
	return json.NewEncoder(a.Stdout).Encode(identity)
}

func validateReadyPoolIdentityCreateLease(lease CoordinatorLease, expected readyPoolIdentityCreateExpected) error {
	if lease.Provider != "aws" {
		return exit(7, "ready-pool identity creation requires an AWS lease")
	}
	if lease.TargetOS != targetLinux {
		return exit(7, "ready-pool identity creation requires a native Linux lease")
	}
	if strings.TrimSpace(lease.SSHHostKey) == "" {
		return exit(7, "ready-pool identity creation requires an authoritative coordinator SSH host key")
	}
	if lease.Image == nil || strings.TrimSpace(lease.Image.ID) != expected.ImageID {
		return exit(7, "coordinator lease image does not match the expected AMI")
	}
	if strings.TrimSpace(lease.ServerType) != expected.ServerType {
		return exit(7, "coordinator lease type does not match the expected AWS instance type")
	}
	if strings.TrimSpace(lease.Architecture) == "" {
		return exit(7, "coordinator lease architecture does not match the expected architecture")
	}
	architecture, err := normalizeArchitecture(lease.Architecture)
	if err != nil || architecture != expected.Architecture {
		return exit(7, "coordinator lease architecture does not match the expected architecture")
	}
	if !validReadyPoolIdentityName(expected.Profile) {
		return exit(2, "--expected-profile is invalid")
	}
	return nil
}

func writeReadyPoolIdentityAtomic(output string, identity CoordinatorReadyPoolIdentityV1) error {
	output = filepath.Clean(strings.TrimSpace(output))
	if output == "." {
		return exit(2, "--output must name a file")
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode ready-pool identity: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(output)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(output)+".tmp-*")
	if err != nil {
		return exit(2, "create temporary ready-pool identity: %v", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return exit(2, "protect temporary ready-pool identity: %v", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return exit(2, "write temporary ready-pool identity: %v", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return exit(2, "sync temporary ready-pool identity: %v", err)
	}
	if err := temp.Close(); err != nil {
		return exit(2, "close temporary ready-pool identity: %v", err)
	}
	if err := os.Link(tempPath, output); err != nil {
		if _, statErr := os.Stat(output); statErr == nil {
			return exit(2, "ready-pool identity output already exists: %s", output)
		}
		return exit(2, "install ready-pool identity: %v", err)
	}
	return nil
}
