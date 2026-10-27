const durationPolicyFields = new Set([
  "defaultTTLSeconds",
  "defaultIdleTimeoutSeconds",
  "maxTTLSeconds",
]);
const switchPolicyFields = new Set(["extension", "autoExtend"]);

// Deployments must accept exactly the override documents the runtime can apply.
export function isLeasePolicyOverrides(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  for (const [group, entries] of Object.entries(value)) {
    if (
      !["orgs", "owners"].includes(group) ||
      !entries ||
      typeof entries !== "object" ||
      Array.isArray(entries)
    )
      return false;
    for (const entry of Object.values(entries)) {
      if (!entry || typeof entry !== "object" || Array.isArray(entry)) return false;
      for (const [key, field] of Object.entries(entry)) {
        if (durationPolicyFields.has(key)) {
          if (!Number.isInteger(field) || typeof field !== "number" || field < 1 || field > 86_400)
            return false;
        } else if (switchPolicyFields.has(key)) {
          if (field !== "allow" && field !== "deny") return false;
        } else return false;
      }
    }
  }
  return true;
}
