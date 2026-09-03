import type { Env } from "./types";

const encoder = new TextEncoder();
const maxBootstrapBytes = 64 * 1024;

export interface ProvisioningMaterialBinding {
  schema: 1;
  leaseID: string;
  operationID: string;
  generation: string;
  scope: string;
}

// These fields never belong in a LeaseRecord, provider attempt, or API response.
export interface ProvisioningMaterial {
  adminPassword: string;
  bootstrap: string;
}

export interface SealedProvisioningMaterial {
  schema: 1;
  iv: string;
  ciphertext: string;
}

export class ProvisioningMaterialUnavailableError extends Error {
  constructor() {
    super("protected provisioning material is unavailable; forward replay is blocked");
    this.name = "ProvisioningMaterialUnavailableError";
  }
}

export function provisioningMaterialConfigured(env: Env): boolean {
  const secret = env.CRABBOX_SESSION_SECRET;
  return Boolean(secret && secret.trim().length >= 32 && secret !== env.CRABBOX_SHARED_TOKEN);
}

export function provisioningMaterialKey(operationID: string): string {
  return `provisioning-material:${operationID}`;
}

export async function sealProvisioningMaterial(
  env: Env,
  binding: ProvisioningMaterialBinding,
  material: ProvisioningMaterial,
): Promise<SealedProvisioningMaterial> {
  validateMaterial(material);
  const key = await encryptionKey(env, ["encrypt"]);
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ciphertext = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv, additionalData: aad(binding) },
    key,
    encoder.encode(
      JSON.stringify({ adminPassword: material.adminPassword, bootstrap: material.bootstrap }),
    ),
  );
  return { schema: 1, iv: encode(iv), ciphertext: encode(new Uint8Array(ciphertext)) };
}

export async function openProvisioningMaterial(
  env: Env,
  binding: ProvisioningMaterialBinding,
  sealed: SealedProvisioningMaterial | undefined,
): Promise<ProvisioningMaterial> {
  try {
    if (
      !sealed ||
      sealed.schema !== 1 ||
      sealed.iv.length !== 16 ||
      sealed.ciphertext.length > 128 * 1024
    ) {
      throw new ProvisioningMaterialUnavailableError();
    }
    const key = await encryptionKey(env, ["decrypt"]);
    const plaintext = await crypto.subtle.decrypt(
      { name: "AES-GCM", iv: decode(sealed.iv), additionalData: aad(binding) },
      key,
      decode(sealed.ciphertext),
    );
    const material: ProvisioningMaterial = JSON.parse(
      new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(plaintext),
    );
    validateMaterial(material);
    return material;
  } catch {
    // Crypto/JSON errors may include input; never attach them as a cause.
    throw new ProvisioningMaterialUnavailableError();
  }
}

function validateMaterial(material: ProvisioningMaterial): void {
  if (
    !material ||
    Object.keys(material).some((key) => key !== "adminPassword" && key !== "bootstrap") ||
    typeof material.adminPassword !== "string" ||
    material.adminPassword.length < 16 ||
    material.adminPassword.length > 128 ||
    typeof material.bootstrap !== "string" ||
    encoder.encode(material.bootstrap).length > maxBootstrapBytes
  ) {
    throw new ProvisioningMaterialUnavailableError();
  }
}

function aad(binding: ProvisioningMaterialBinding): Uint8Array {
  return encoder.encode(
    JSON.stringify([
      "crabbox/provisioning/material",
      binding.schema,
      binding.leaseID,
      binding.operationID,
      binding.generation,
      binding.scope,
    ]),
  );
}

async function encryptionKey(env: Env, usages: ("encrypt" | "decrypt")[]): Promise<CryptoKey> {
  if (!provisioningMaterialConfigured(env)) throw new ProvisioningMaterialUnavailableError();
  const material = await crypto.subtle.digest(
    "SHA-256",
    encoder.encode(`crabbox/provisioning/aes-gcm/v1\0${env.CRABBOX_SESSION_SECRET}`),
  );
  return crypto.subtle.importKey("raw", material, "AES-GCM", false, usages);
}

function encode(bytes: Uint8Array): string {
  return btoa(Array.from(bytes, (byte) => String.fromCharCode(byte)).join(""));
}

function decode(value: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(atob(value), (character) => character.charCodeAt(0));
}
