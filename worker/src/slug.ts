const adjectives = [
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
];

const nouns = ["barnacle", "crab", "crayfish", "hermit", "krill", "lobster", "prawn", "shrimp"];

export function leaseSlugFromID(leaseID: string): string {
  const hash = slugHash(leaseID);
  const adjective = adjectives[hash % adjectives.length] ?? "blue";
  const noun = nouns[Math.trunc(hash / adjectives.length) % nouns.length] ?? "crab";
  return `${adjective}-${noun}`;
}

/**
 * Bounds a requested slug so `leaseProviderName` always fits a 63-character provider
 * resource name: 41 + 5 (collision suffix) + 17 ("crabbox-", the separator, and the
 * 8-hex lease hash) = 63. Mirrors maxRequestedLeaseSlugLength in internal/cli/slug.go.
 */
export const maxRequestedLeaseSlugLength = 41;

export class InvalidLeaseSlugError extends Error {
  readonly reason: "empty" | "too_long";

  constructor(reason: "empty" | "too_long") {
    super(
      reason === "empty"
        ? "slug must contain at least one letter or digit"
        : `slug must be ${maxRequestedLeaseSlugLength} characters or fewer after normalization`,
    );
    this.name = "InvalidLeaseSlugError";
    this.reason = reason;
  }
}

/** Validates a client-requested slug; normalization alone cannot bound length. */
export function requestedLeaseSlug(value: string | undefined): string {
  if (!value?.trim()) return "";
  const slug = normalizeLeaseSlug(value);
  if (!slug) {
    throw new InvalidLeaseSlugError("empty");
  }
  if (slug.length > maxRequestedLeaseSlugLength) {
    throw new InvalidLeaseSlugError("too_long");
  }
  return slug;
}

export function normalizeLeaseSlug(value: string | undefined): string {
  let out = "";
  let lastDash = false;
  for (const char of (value ?? "").trim().toLowerCase()) {
    const code = char.charCodeAt(0);
    const ok = (code >= 97 && code <= 122) || (code >= 48 && code <= 57);
    if (ok) {
      out += char;
      lastDash = false;
      continue;
    }
    if (!lastDash) {
      out += "-";
      lastDash = true;
    }
  }
  return trimDashes(out);
}

export function slugWithCollisionSuffix(base: string, seed: string): string {
  const normalized = normalizeLeaseSlug(base) || leaseSlugFromID(seed);
  const bounded =
    normalized.length > maxRequestedLeaseSlugLength
      ? trimDashes(normalized.slice(0, maxRequestedLeaseSlugLength))
      : normalized;
  return `${bounded}-${(slugHash(seed) & 0xffff).toString(16).padStart(4, "0")}`;
}

export function leaseProviderName(leaseID: string, slug: string | undefined): string {
  const normalized = normalizeLeaseSlug(slug);
  return normalized
    ? `crabbox-${normalized}-${slugHash(leaseID).toString(16).padStart(8, "0")}`
    : `crabbox-${leaseID}`.replaceAll("_", "-");
}

function slugHash(value: string): number {
  let hash = 0x811c9dc5;
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash >>> 0;
}

function trimDashes(value: string): string {
  let start = 0;
  let end = value.length;
  while (start < end && value[start] === "-") {
    start += 1;
  }
  while (end > start && value[end - 1] === "-") {
    end -= 1;
  }
  return value.slice(start, end);
}
