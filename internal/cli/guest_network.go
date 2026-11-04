package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/openclaw/crabbox/internal/guestnetwork"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const GuestNetworkVersion = guestnetwork.Version

type GuestNetworkPolicy = guestnetwork.Policy
type GuestNetworkRule = guestnetwork.Rule
type GuestNetworkReceipt = guestnetwork.Receipt
type GuestNetworkPolicyIdentity = guestnetwork.Identity

func guestNetworkPolicyIdentity(policy GuestNetworkPolicy) (GuestNetworkPolicyIdentity, error) {
	return guestnetwork.PolicyIdentity(policy)
}
func validateGuestNetworkIdentityReceipt(expected GuestNetworkPolicyIdentity, receipt *GuestNetworkReceipt, leaseID, provider, resourceID string) error {
	return guestnetwork.ValidateIdentityReceipt(expected, receipt, leaseID, provider, resourceID)
}

// GuestNetworkBackend is an enforcement/inspection capability, not caller attestation.
// Implementations must inspect the effective provider boundary before any guest
// bootstrap/access, including fixed-ID replay. Receipt JSON alone is not evidence.
type GuestNetworkBackend interface{ GuestNetworkVersion() int }

func GuestNetworkPolicyDigest(policy GuestNetworkPolicy) (string, error) {
	return guestnetwork.Digest(policy)
}
func ValidateGuestNetworkReceipt(policy GuestNetworkPolicy, receipt *GuestNetworkReceipt, leaseID, provider, resourceID string) error {
	return guestnetwork.ValidateReceipt(policy, receipt, leaseID, provider, resourceID)
}

func guestWorkloadRequested() (bool, error) {
	value := strings.TrimSpace(os.Getenv("CRABBOX_UNTRUSTED_WORKLOAD"))
	if value == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, errors.New("CRABBOX_UNTRUSTED_WORKLOAD must be boolean")
	}
	return enabled, nil
}

func validateGuestOperatorConfigSource(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("cannot inspect operator configuration source")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || !guestPolicyFileOwnerTrusted(info) {
		return errors.New("guest configuration must be an operator-owned regular file without group/other writes")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return errors.New("cannot resolve operator configuration source")
	}
	boundary, _ := findRepositoryBoundary()
	if boundary.root != "" && configPathWithinRoot(resolved, boundary.root) {
		return errors.New("guest configuration cannot resolve into the workload repository")
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		parent, e := os.Lstat(dir)
		if e != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0022 != 0 || !guestPolicyFileOwnerTrusted(parent) {
			return errors.New("guest configuration directory is not operator controlled")
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return nil
}

func loadOperatorGuestNetworkPolicy() (*GuestNetworkPolicy, error) {
	enabled, err := guestWorkloadRequested()
	if err != nil || !enabled {
		return nil, err
	}
	path := strings.TrimSpace(os.Getenv("CRABBOX_GUEST_NETWORK_POLICY_FILE"))
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("untrusted workloads require an absolute operator guest network policy file")
	}
	boundary, _ := findRepositoryBoundary()
	if boundary.root != "" && configPathWithinRoot(path, boundary.root) {
		return nil, errors.New("guest network policy must be outside the repository")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("cannot read operator guest network policy")
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > 65536 || before.Mode().Perm()&0022 != 0 || !guestPolicyFileOwnerTrusted(before) {
		return nil, errors.New("guest network policy must be an operator-owned regular file without group/other writes")
	}
	// Refuse symlink/writable directory substitution, not just a trusted-looking filename.
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, e := os.Lstat(dir)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || !guestPolicyFileOwnerTrusted(info) {
			return nil, errors.New("guest network policy directory is not operator controlled")
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open operator guest network policy")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New("guest network policy changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	var policy GuestNetworkPolicy
	if err := decoder.Decode(&policy); err != nil {
		return nil, errors.New("invalid operator guest network policy JSON")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("trailing operator guest network policy data")
	}
	if _, err := GuestNetworkPolicyDigest(policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

func validateGuestClientIsolation(cfg Config) error {
	if len(cfg.EnvAllow) != 0 || cfg.Cache.Pnpm || cfg.Cache.Npm || cfg.Cache.Docker || cfg.Cache.Git || len(cfg.Cache.Volumes) != 0 || cfg.Tailscale.Enabled {
		return errors.New("guest workloads require empty environment forwarding, no shared caches/volumes or overlay credentials")
	}
	return nil
}

func validateGuestLeaseTarget(cfg Config, lease LeaseTarget, requestedID, requestedProvider string) error {
	if !guestNetworkRequested(cfg) {
		return nil
	}
	expected := lease.GuestNetworkExpected
	if cfg.GuestNetworkPolicy != nil {
		identity, err := guestNetworkPolicyIdentity(*cfg.GuestNetworkPolicy)
		if err != nil {
			return err
		}
		expected = &identity
	}
	if expected == nil {
		return errors.New("guest policy custody is missing")
	}
	return validateGuestNetworkIdentityReceipt(*expected, lease.GuestNetwork, requestedID, requestedProvider, lease.Server.CloudID)
}

func guestNetworkRequested(cfg Config) bool {
	return cfg.UntrustedWorkload || cfg.GuestNetworkPolicy != nil
}

func validateGuestNetworkBackend(cfg Config, backend Backend, coordinator bool) error {
	if !guestNetworkRequested(cfg) {
		return nil
	}
	if err := validateGuestClientIsolation(cfg); err != nil {
		return err
	}
	if coordinator {
		return nil
	}
	if cfg.GuestNetworkPolicy == nil {
		return errors.New("direct guest network policy custody missing")
	}
	capability, ok := backend.(GuestNetworkBackend)
	if !ok || capability.GuestNetworkVersion() != GuestNetworkVersion {
		return fmt.Errorf("provider=%s does not support guest network enforcement/inspection v%d", backend.Spec().Name, GuestNetworkVersion)
	}
	return nil
}
