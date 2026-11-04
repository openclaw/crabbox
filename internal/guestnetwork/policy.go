// Package guestnetwork defines provider-neutral operator policy and receipt validation.
package guestnetwork

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"strings"
)

const Version = 1

// Policy is operator-owned connection policy, never repository input.
// Providers must report their platform exceptions explicitly; this contract
// does not itself imply complete packet or DNS isolation.
type Policy struct {
	// Empty preserves the original no-userdata/disabled-metadata contract.
	BootstrapMode      string   `json:"bootstrapMode,omitempty"`
	Version            int      `json:"version"`
	ID                 string   `json:"id"`
	Revision           string   `json:"revision"`
	Egress             []Rule   `json:"egress"`
	SSHSources         []string `json:"sshSources"`
	PlatformExceptions []string `json:"platformExceptions,omitempty"`
}
type Rule struct {
	CIDR  string `json:"cidr"`
	Ports []int  `json:"ports"`
}
type Receipt struct {
	BootstrapMode  string            `json:"bootstrapMode,omitempty"`
	Version        int               `json:"version"`
	PolicyID       string            `json:"policyId"`
	PolicyRevision string            `json:"policyRevision"`
	PolicyDigest   string            `json:"policyDigest"`
	LeaseID        string            `json:"leaseId"`
	Provider       string            `json:"provider"`
	ResourceID     string            `json:"resourceId"`
	BoundaryID     string            `json:"boundaryId"`
	Facts          map[string]string `json:"facts"`
}

func Digest(policy Policy) (string, error) {
	if policy.BootstrapMode != "" && policy.BootstrapMode != "ssh-public-key-v1" {
		return "", errors.New("unknown guest bootstrap mode")
	}
	if policy.Version != Version || strings.TrimSpace(policy.ID) == "" || strings.TrimSpace(policy.Revision) == "" || len(policy.ID) > 128 || len(policy.Revision) > 128 {
		return "", errors.New("invalid guest network policy identity/version")
	}
	if len(policy.Egress) > 64 || len(policy.SSHSources) == 0 || len(policy.SSHSources) > 64 {
		return "", errors.New("invalid guest network rule count")
	}
	policy.Egress = slices.Clone(policy.Egress)
	policy.SSHSources = slices.Clone(policy.SSHSources)
	policy.PlatformExceptions = slices.Clone(policy.PlatformExceptions)
	if len(policy.PlatformExceptions) > 8 {
		return "", errors.New("too many platform exceptions")
	}
	slices.Sort(policy.PlatformExceptions)
	for i, exception := range policy.PlatformExceptions {
		if exception == "" || len(exception) > 128 || strings.TrimSpace(exception) != exception || i > 0 && exception == policy.PlatformExceptions[i-1] {
			return "", errors.New("invalid platform exception identity")
		}
	}
	for i, rule := range policy.Egress {
		p, err := netip.ParsePrefix(rule.CIDR)
		if err != nil || !p.Addr().Is4() || p != p.Masked() || !guestPublicPrefix(p) {
			return "", errors.New("guest network egress must be canonical public IPv4 CIDRs")
		}
		if len(rule.Ports) == 0 || len(rule.Ports) > 64 {
			return "", errors.New("guest network TCP ports required")
		}
		rule.Ports = slices.Clone(rule.Ports)
		slices.Sort(rule.Ports)
		for j, port := range rule.Ports {
			if port < 1 || port > 65535 || j > 0 && port == rule.Ports[j-1] {
				return "", errors.New("invalid guest network TCP port")
			}
		}
		policy.Egress[i] = rule
	}
	for _, cidr := range policy.SSHSources {
		p, err := netip.ParsePrefix(cidr)
		if err != nil || !p.Addr().Is4() || p != p.Masked() || p.Bits() < 8 {
			return "", errors.New("guest network SSH sources must be bounded canonical IPv4 CIDRs")
		}
	}
	slices.Sort(policy.SSHSources)
	slices.SortFunc(policy.Egress, func(a, b Rule) int {
		if n := strings.Compare(a.CIDR, b.CIDR); n != 0 {
			return n
		}
		aa, _ := json.Marshal(a.Ports)
		bb, _ := json.Marshal(b.Ports)
		return strings.Compare(string(aa), string(bb))
	})
	data, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func guestPublicPrefix(p netip.Prefix) bool {
	for _, cidr := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4"} {
		if p.Overlaps(netip.MustParsePrefix(cidr)) {
			return false
		}
	}
	return true
}

type Identity struct {
	Version        int    `json:"version"`
	PolicyID       string `json:"policyId"`
	PolicyRevision string `json:"policyRevision"`
	PolicyDigest   string `json:"policyDigest"`
	BootstrapMode  string `json:"bootstrapMode,omitempty"`
}

func PolicyIdentity(policy Policy) (Identity, error) {
	digest, err := Digest(policy)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Version: Version, PolicyID: policy.ID, PolicyRevision: policy.Revision, PolicyDigest: digest, BootstrapMode: policy.BootstrapMode}, nil
}
func ValidateIdentityReceipt(identity Identity, receipt *Receipt, leaseID, provider, resourceID string) error {
	if identity.Version != Version || identity.PolicyID == "" || identity.PolicyRevision == "" || len(identity.PolicyDigest) != 64 || receipt == nil || receipt.Version != identity.Version || receipt.PolicyID != identity.PolicyID || receipt.PolicyRevision != identity.PolicyRevision || receipt.PolicyDigest != identity.PolicyDigest || receipt.BootstrapMode != identity.BootstrapMode || receipt.LeaseID != leaseID || receipt.Provider != provider || receipt.ResourceID != resourceID || leaseID == "" || provider == "" || resourceID == "" || receipt.BoundaryID == "" {
		return errors.New("guest network boundary receipt missing or mismatched")
	}
	return nil
}

func ValidateReceipt(policy Policy, receipt *Receipt, leaseID, provider, resourceID string) error {
	digest, err := Digest(policy)
	if err != nil {
		return err
	}
	if receipt == nil || receipt.BootstrapMode != policy.BootstrapMode || receipt.Version != Version || receipt.PolicyID != policy.ID || receipt.PolicyRevision != policy.Revision || receipt.PolicyDigest != digest || receipt.LeaseID != leaseID || receipt.Provider != provider || receipt.ResourceID != resourceID || leaseID == "" || provider == "" || resourceID == "" || receipt.BoundaryID == "" {
		return errors.New("guest network boundary receipt missing or mismatched")
	}
	return nil
}
