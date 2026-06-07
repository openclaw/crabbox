export const defaultOSImage = "ubuntu:26.04";

export interface OSImageSpec {
  selector: string;
  awsName: string;
  awsArm64Name: string;
  awsLabel: string;
  azureImage: string;
  azureArm64Image: string;
  gcpImage: string;
  hetznerImage: string;
}

const specs: Record<string, OSImageSpec> = {
  "ubuntu:24.04": {
    selector: "ubuntu:24.04",
    awsName: "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*",
    awsArm64Name: "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*",
    awsLabel: "Ubuntu 24.04",
    azureImage: "Canonical:ubuntu-24_04-lts:server:latest",
    azureArm64Image: "Canonical:ubuntu-24_04-lts:server-arm64:latest",
    gcpImage: "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
    hetznerImage: "ubuntu-24.04",
  },
  "ubuntu:26.04": {
    selector: "ubuntu:26.04",
    awsName: "ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-amd64-server-*",
    awsArm64Name: "ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-arm64-server-*",
    awsLabel: "Ubuntu 26.04",
    azureImage: "Canonical:ubuntu-26_04-lts:server:latest",
    azureArm64Image: "Canonical:ubuntu-26_04-lts:server-arm64:latest",
    gcpImage: "projects/ubuntu-os-cloud/global/images/family/ubuntu-2604-lts-amd64",
    hetznerImage: "ubuntu-24.04",
  },
};

export function normalizeOSImage(value: string | undefined): string {
  let normalized = (value ?? "").trim().toLowerCase();
  if (!normalized) {
    normalized = defaultOSImage;
  }
  normalized = normalized.replaceAll("_", ".").replaceAll("-", ":");
  if (normalized === "ubuntu2404" || normalized === "ubuntu:2404") {
    normalized = "ubuntu:24.04";
  } else if (normalized === "ubuntu2604" || normalized === "ubuntu:2604") {
    normalized = "ubuntu:26.04";
  }
  if (!specs[normalized]) {
    throw new Error(
      `unsupported os ${JSON.stringify(value)}; supported: ubuntu:26.04, ubuntu:24.04`,
    );
  }
  return normalized;
}

export function osImageSpec(value: string | undefined): OSImageSpec {
  const spec = specs[normalizeOSImage(value)];
  if (!spec) {
    throw new Error(
      `unsupported os ${JSON.stringify(value)}; supported: ubuntu:26.04, ubuntu:24.04`,
    );
  }
  return spec;
}
