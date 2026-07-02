export function providerKeyForLease(leaseID: string): string {
  return `crabbox-${leaseID.replaceAll("_", "-")}`;
}

export function leaseIDForProviderKey(providerKey: string): string | undefined {
  const match = /^crabbox-cbx-([a-f0-9]{12})$/.exec(providerKey);
  return match ? `cbx_${match[1]}` : undefined;
}

export function providerKeyOwnershipLabels(leaseID: string): Record<string, string> {
  return { crabbox: "true", created_by: "crabbox", lease: leaseID };
}

export function providerKeyOwnedByLease(labels: Record<string, string>, leaseID: string): boolean {
  const expected = providerKeyOwnershipLabels(leaseID);
  return Object.entries(expected).every(([key, value]) => labels[key] === value);
}

export function sshPublicKeyIdentity(publicKey: string): string {
  const [type, encoded] = publicKey.trim().split(/\s+/, 3);
  return type && encoded ? `${type} ${encoded}` : publicKey.trim();
}
