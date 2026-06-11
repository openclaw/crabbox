package cli

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var canonicalLeaseIDPattern = regexp.MustCompile(`^cbx_[a-f0-9]{12}$`)

const maxRequestedLeaseSlugLength = 41

var leaseSlugAdjectives = []string{
	"amber",
	"blue",
	"brisk",
	"coral",
	"crimson",
	"golden",
	"harbor",
	"jade",
	"pearl",
	"quick",
	"silver",
	"swift",
	"tidal",
	"violet",
}

var leaseSlugNouns = []string{
	"barnacle",
	"crab",
	"crayfish",
	"hermit",
	"krill",
	"lobster",
	"prawn",
	"shrimp",
}

func newLeaseSlug(leaseID string) string {
	hash := leaseSlugHash(leaseID)
	adjective := leaseSlugAdjectives[int(hash%uint32(len(leaseSlugAdjectives)))]
	noun := leaseSlugNouns[int((hash/uint32(len(leaseSlugAdjectives)))%uint32(len(leaseSlugNouns)))]
	return adjective + "-" + noun
}

func slugWithCollisionSuffix(base, seed string) string {
	base = normalizeLeaseSlug(base)
	if base == "" {
		base = newLeaseSlug(seed)
	}
	if len(base) > maxRequestedLeaseSlugLength {
		base = strings.Trim(base[:maxRequestedLeaseSlugLength], "-")
	}
	return fmt.Sprintf("%s-%04x", base, leaseSlugHash(seed)&0xffff)
}

func normalizeLeaseSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func requestedLeaseSlug(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	slug := normalizeLeaseSlug(value)
	if slug == "" {
		return "", exit(2, "--slug must contain at least one letter or digit")
	}
	if len(slug) > maxRequestedLeaseSlugLength {
		return "", exit(2, "--slug must be %d characters or fewer after normalization", maxRequestedLeaseSlugLength)
	}
	return slug, nil
}

func leaseProviderName(leaseID, slug string) string {
	if slug = normalizeLeaseSlug(slug); slug != "" {
		return fmt.Sprintf("crabbox-%s-%08x", slug, leaseSlugHash(leaseID))
	}
	return strings.ReplaceAll("crabbox-"+leaseID, "_", "-")
}

func allocateDirectLeaseSlug(leaseID, requested string, servers []Server) (string, error) {
	base := normalizeLeaseSlug(requested)
	generated := base == ""
	if base == "" {
		base = newLeaseSlug(leaseID)
	}
	slug := base
	for attempt := 0; attempt < 20; attempt++ {
		inUse := serverSlugInUse(slug, servers)
		if !inUse {
			var err error
			if generated {
				inUse, err = claimSlugInUseBestEffort(slug, leaseID)
			} else {
				inUse, err = claimSlugInUse(slug, leaseID)
			}
			if err != nil {
				return "", err
			}
		}
		if !inUse {
			return slug, nil
		}
		slug = slugWithCollisionSuffix(base, fmt.Sprintf("%s-%d", leaseID, attempt))
	}
	return slugWithCollisionSuffix(base, leaseID), nil
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	base := normalizeLeaseSlug(requested)
	if base == "" {
		return newLeaseSlug(leaseID), nil
	}
	slug := base
	for attempt := 0; attempt < 20; attempt++ {
		inUse, err := claimSlugInUse(slug, leaseID)
		if err != nil {
			return "", err
		}
		if !inUse {
			return slug, nil
		}
		slug = slugWithCollisionSuffix(base, fmt.Sprintf("%s-%d", leaseID, attempt))
	}
	return slugWithCollisionSuffix(base, leaseID), nil
}

func claimSlugInUse(slug, leaseID string) (bool, error) {
	slug = normalizeLeaseSlug(slug)
	if slug == "" {
		return false, nil
	}
	_, ok, err := findLeaseClaim(slug, func(candidate leaseClaim) bool {
		return candidate.LeaseID != "" &&
			candidate.LeaseID != leaseID &&
			normalizeLeaseSlug(candidate.Slug) == slug
	})
	return ok, err
}

func claimSlugInUseBestEffort(slug, leaseID string) (bool, error) {
	slug = normalizeLeaseSlug(slug)
	if slug == "" {
		return false, nil
	}
	dir, err := crabboxStateDir()
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "claims"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, exit(2, "read claims directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		candidateID := strings.TrimSuffix(entry.Name(), ".json")
		candidate, err := readLeaseClaim(candidateID)
		if err != nil {
			continue
		}
		if candidate.LeaseID != "" &&
			candidate.LeaseID != leaseID &&
			normalizeLeaseSlug(candidate.Slug) == slug {
			return true, nil
		}
	}
	return false, nil
}

func serverSlugInUse(slug string, servers []Server) bool {
	slug = normalizeLeaseSlug(slug)
	for _, server := range servers {
		if serverSlug(server) == slug {
			return true
		}
	}
	return false
}

func serverSlug(server Server) string {
	if server.Labels == nil {
		return ""
	}
	return normalizeLeaseSlug(server.Labels["slug"])
}

func isCanonicalLeaseID(value string) bool {
	return canonicalLeaseIDPattern.MatchString(value)
}

func leaseSlugHash(value string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return h.Sum32()
}
