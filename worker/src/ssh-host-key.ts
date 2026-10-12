import { generateKeyPairSync, randomBytes } from "node:crypto";

import { bytesToBase64 } from "./encoding";

const encoder = new TextEncoder();
const publicPrefix = Uint8Array.of(
  0x30,
  0x2a,
  0x30,
  0x05,
  0x06,
  0x03,
  0x2b,
  0x65,
  0x70,
  0x03,
  0x21,
  0x00,
);
const privatePrefix = Uint8Array.of(
  0x30,
  0x2e,
  0x02,
  0x01,
  0x00,
  0x30,
  0x05,
  0x06,
  0x03,
  0x2b,
  0x65,
  0x70,
  0x04,
  0x22,
  0x04,
  0x20,
);

function ed25519Bytes(der: Uint8Array, prefix: Uint8Array): Uint8Array {
  // RFC 8410 Ed25519 SPKI/PKCS8 has no algorithm parameters. The SPKI
  // prefix includes exactly one unused-bit byte; subsequent zeros are key data.
  if (der.length !== prefix.length + 32 || !prefix.every((byte, index) => der[index] === byte)) {
    throw new Error("Unexpected Ed25519 key encoding");
  }
  return der.subarray(prefix.length);
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const bytes = new Uint8Array(parts.reduce((length, part) => length + part.length, 0));
  let offset = 0;
  for (const part of parts) {
    bytes.set(part, offset);
    offset += part.length;
  }
  return bytes;
}

function sshString(value: Uint8Array): Uint8Array {
  const length = new Uint8Array(4);
  new DataView(length.buffer).setUint32(0, value.length);
  return concat(length, value);
}

export function generateSSHHostKeyPair(comment: string): { private: string; public: string } {
  const keys = generateKeyPairSync("ed25519", {
    publicKeyEncoding: { type: "spki", format: "der" },
    privateKeyEncoding: { type: "pkcs8", format: "der" },
  });
  const publicKey = ed25519Bytes(keys.publicKey, publicPrefix);
  const seed = ed25519Bytes(keys.privateKey, privatePrefix);
  const keyType = sshString(encoder.encode("ssh-ed25519"));
  const publicBlob = concat(keyType, sshString(publicKey));
  const check = randomBytes(4);
  const privateBody = concat(
    check,
    check,
    keyType,
    sshString(publicKey),
    sshString(concat(seed, publicKey)),
    sshString(encoder.encode(comment)),
  );
  const padding = Uint8Array.from(
    { length: (8 - (privateBody.length % 8)) % 8 },
    (_, index) => index + 1,
  );
  const encoded = bytesToBase64(
    concat(
      encoder.encode("openssh-key-v1\0"),
      sshString(encoder.encode("none")),
      sshString(encoder.encode("none")),
      sshString(new Uint8Array(0)),
      Uint8Array.of(0, 0, 0, 1),
      sshString(publicBlob),
      sshString(concat(privateBody, padding)),
    ),
  );
  return {
    private: `-----BEGIN OPENSSH PRIVATE KEY-----\n${encoded.replace(/.{64}/g, "$&\n").trimEnd()}\n-----END OPENSSH PRIVATE KEY-----\n`,
    public: `ssh-ed25519 ${bytesToBase64(publicBlob)}${comment ? ` ${comment}` : ""}`,
  };
}
