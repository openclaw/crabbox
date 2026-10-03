package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash/fnv"
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

func NewLeaseSlug(leaseID string) string {
	hash := leaseSlugHash(leaseID)
	adjective := leaseSlugAdjectives[int(hash%uint32(len(leaseSlugAdjectives)))]
	noun := leaseSlugNouns[int((hash/uint32(len(leaseSlugAdjectives)))%uint32(len(leaseSlugNouns)))]
	return adjective + "-" + noun
}

func generatedLeaseSlug(leaseID string) string {
	fingerprint := sha256.Sum256([]byte(leaseID))
	return fmt.Sprintf("%s-%x", NewLeaseSlug(leaseID), fingerprint[:4])
}

func SlugWithCollisionSuffix(base, seed string) string {
	base = NormalizeLeaseSlug(base)
	if base == "" {
		base = NewLeaseSlug(seed)
	}
	if len(base) > maxRequestedLeaseSlugLength {
		base = strings.Trim(base[:maxRequestedLeaseSlugLength], "-")
	}
	return fmt.Sprintf("%s-%04x", base, leaseSlugHash(seed)&0xffff)
}

func NormalizeLeaseSlug(value string) string {
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
	slug := NormalizeLeaseSlug(value)
	if slug == "" {
		return "", Exit(2, "--slug must contain at least one letter or digit")
	}
	if len(slug) > maxRequestedLeaseSlugLength {
		return "", Exit(2, "--slug must be %d characters or fewer after normalization", maxRequestedLeaseSlugLength)
	}
	return slug, nil
}

func LeaseProviderName(leaseID, slug string) string {
	if slug = NormalizeLeaseSlug(slug); slug != "" {
		return fmt.Sprintf("crabbox-%s-%08x", slug, leaseSlugHash(leaseID))
	}
	return strings.ReplaceAll("crabbox-"+leaseID, "_", "-")
}

func AllocateDirectLeaseSlug(leaseID, requested string, servers []Server) (string, error) {
	return AllocateDirectLeaseSlugContext(context.Background(), leaseID, requested, servers)
}

func AllocateDirectLeaseSlugContext(ctx context.Context, leaseID, requested string, servers []Server) (string, error) {
	return allocateDirectLeaseSlug(ctx, leaseID, requested, servers, claimSlugInUse)
}

func allocateDirectLeaseSlug(ctx context.Context, leaseID, requested string, servers []Server, claimInUse func(context.Context, string, string) (bool, error)) (string, error) {
	base := NormalizeLeaseSlug(requested)
	generated := base == ""
	if base == "" {
		// The ID-derived suffix spreads generated names without scanning stale
		// local claims; the coordinator/provider remains authoritative.
		base = generatedLeaseSlug(leaseID)
	}
	slug := base
	for attempt := 0; attempt < 20; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		inUse := serverSlugInUse(slug, servers)
		if !inUse && !generated {
			var err error
			inUse, err = claimInUse(ctx, slug, leaseID)
			if err != nil {
				return "", err
			}
		}
		if !inUse {
			return slug, nil
		}
		slug = SlugWithCollisionSuffix(base, fmt.Sprintf("%s-%d", leaseID, attempt))
	}
	fallback := SlugWithCollisionSuffix(base, leaseID)
	inUse := serverSlugInUse(fallback, servers)
	if !inUse && !generated {
		var err error
		inUse, err = claimInUse(ctx, fallback, leaseID)
		if err != nil {
			return "", err
		}
	}
	if inUse {
		return "", Exit(2, "could not allocate a unique lease slug for %s", leaseID)
	}
	return fallback, nil
}

func AllocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return AllocateDirectLeaseSlug(leaseID, requested, nil)
}

func AllocateClaimLeaseSlugContext(ctx context.Context, leaseID, requested string) (string, error) {
	return AllocateDirectLeaseSlugContext(ctx, leaseID, requested, nil)
}

func claimSlugInUse(ctx context.Context, slug, leaseID string) (bool, error) {
	slug = NormalizeLeaseSlug(slug)
	if slug == "" {
		return false, nil
	}
	_, ok, err := findLeaseClaim(ctx, slug, func(candidate leaseClaim) bool {
		return candidate.LeaseID != "" &&
			candidate.LeaseID != leaseID &&
			NormalizeLeaseSlug(candidate.Slug) == slug
	})
	return ok, err
}

func serverSlugInUse(slug string, servers []Server) bool {
	slug = NormalizeLeaseSlug(slug)
	for _, server := range servers {
		if ServerSlug(server) == slug {
			return true
		}
	}
	return false
}

func ServerSlug(server Server) string {
	return NormalizeLeaseSlug(server.Labels["slug"])
}

func IsCanonicalLeaseID(value string) bool {
	return canonicalLeaseIDPattern.MatchString(value)
}

func leaseSlugHash(value string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return h.Sum32()
}
