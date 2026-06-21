export class InvalidAWSRegionError extends Error {
  constructor(field: string) {
    super(`${field} must be an AWS region name`);
    this.name = "InvalidAWSRegionError";
  }
}

export function sanitizeAWSRegion(value: unknown): string {
  if (typeof value !== "string") {
    return "";
  }
  const region = value.trim().toLowerCase();
  return /^[a-z]{2}-[a-z-]+-[0-9]$/.test(region) ? region : "";
}

export function requireAWSRegion(value: unknown, field = "region"): string {
  const region = sanitizeAWSRegion(value);
  if (!region) {
    throw new InvalidAWSRegionError(field);
  }
  return region;
}
