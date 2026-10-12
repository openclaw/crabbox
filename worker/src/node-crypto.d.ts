// The Worker uses these nodejs_compat APIs without Node's conflicting globals.
declare module "node:crypto" {
  export function generateKeyPairSync(
    type: "ed25519",
    options: {
      publicKeyEncoding: { type: "spki"; format: "der" };
      privateKeyEncoding: { type: "pkcs8"; format: "der" };
    },
  ): { publicKey: Uint8Array; privateKey: Uint8Array };
  export function randomBytes(size: number): Uint8Array;
}
