import crypto from "node:crypto";
import zlib from "node:zlib";

export const runnerTargets = ["darwin", "linux", "windows"].flatMap((os) =>
  ["amd64", "arm64"].map((arch) => ({
    os,
    arch,
    name: `crabbox-runner-${os}-${arch}${os === "windows" ? ".exe" : ""}`,
  })),
);
export const runnerBundleMagic = Buffer.from("CBXRPK01", "ascii");
export const maxRunnerBytes = 32 * 1024 * 1024;
export const maxRunnerBundleBytes = 6 * maxRunnerBytes + 65548;
export const runnerSHA256 = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
export const isRunnerBuildID = (value) =>
  typeof value === "string" && /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(value);

function exactKeys(value, expected) {
  if (
    !value ||
    Array.isArray(value) ||
    JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...expected].sort())
  ) {
    throw new Error("runner bundle metadata has unexpected fields");
  }
}

export function unpackRunnerBundle(bytes, expectedBuildId) {
  if (
    bytes.length < 12 ||
    bytes.length > maxRunnerBundleBytes ||
    !bytes.subarray(0, 8).equals(runnerBundleMagic)
  ) {
    throw new Error("invalid runner bundle size or magic");
  }
  const headerSize = bytes.readUInt32BE(8);
  if (headerSize < 1 || headerSize > 65536 || headerSize > bytes.length - 12)
    throw new Error("invalid runner bundle header size");
  const headerBytes = bytes.subarray(12, 12 + headerSize);
  const manifest = JSON.parse(headerBytes.toString("utf8"));
  // The packer emits canonical JSON. Exact re-encoding rejects duplicate keys,
  // alternative spellings and noncanonical metadata without another parser.
  if (!Buffer.from(JSON.stringify(manifest)).equals(headerBytes))
    throw new Error("runner bundle metadata is not canonical");
  exactKeys(manifest, ["version", "buildId", "entries"]);
  if (
    manifest.version !== 1 ||
    !isRunnerBuildID(expectedBuildId) ||
    manifest.buildId !== expectedBuildId ||
    !Array.isArray(manifest.entries) ||
    manifest.entries.length !== runnerTargets.length
  ) {
    throw new Error("runner bundle identity or inventory mismatch");
  }
  const canonical = {
    version: manifest.version,
    buildId: manifest.buildId,
    entries: manifest.entries.map((entry) =>
      Object.fromEntries(
        ["os", "arch", "sha256", "size", "packedSha256", "packedSize", "offset"].map((key) => [
          key,
          entry?.[key],
        ]),
      ),
    ),
  };
  if (!Buffer.from(JSON.stringify(canonical)).equals(headerBytes))
    throw new Error("runner bundle metadata is not canonical");
  const payload = bytes.subarray(12 + headerSize);
  const members = [];
  let offset = 0;
  for (const [index, target] of runnerTargets.entries()) {
    const entry = manifest.entries[index];
    exactKeys(entry, ["os", "arch", "sha256", "size", "packedSha256", "packedSize", "offset"]);
    if (
      entry.os !== target.os ||
      entry.arch !== target.arch ||
      entry.offset !== offset ||
      !Number.isSafeInteger(entry.size) ||
      entry.size < 1 ||
      entry.size > maxRunnerBytes ||
      !Number.isSafeInteger(entry.packedSize) ||
      entry.packedSize < 18 ||
      entry.packedSize > maxRunnerBytes ||
      entry.packedSize > payload.length - offset ||
      !/^[0-9a-f]{64}$/.test(entry.sha256) ||
      !/^[0-9a-f]{64}$/.test(entry.packedSha256)
    ) {
      throw new Error("invalid runner bundle member");
    }
    const packed = payload.subarray(offset, offset + entry.packedSize);
    if (runnerSHA256(packed) !== entry.packedSha256)
      throw new Error("runner compressed digest mismatch");
    // Protected packing emits one gzip member with no optional header fields.
    // Raw inflater consumption catches empty extra members and zero trailers
    // which a general gzip decoder may otherwise accept.
    if (packed.readUInt32LE(0) !== 0x00088b1f) throw new Error("noncanonical runner gzip header");
    const deflate = packed.subarray(10, packed.length - 8);
    const inflated = zlib.inflateRawSync(deflate, { maxOutputLength: entry.size + 1, info: true });
    if (inflated.engine.bytesWritten !== deflate.length)
      throw new Error("runner gzip has trailing data");
    const raw = zlib.gunzipSync(packed, { maxOutputLength: entry.size + 1 });
    if (raw.length !== entry.size || runnerSHA256(raw) !== entry.sha256)
      throw new Error("runner raw identity mismatch");
    members.push({ ...target, metadata: entry, bytes: raw });
    offset += entry.packedSize;
  }
  if (offset !== payload.length) throw new Error("runner bundle has trailing data");
  return { manifest, members, sha256: runnerSHA256(bytes), size: bytes.length };
}

export function extractRunnerBundle(binary, expected) {
  if (
    !expected ||
    !Number.isSafeInteger(expected.size) ||
    expected.size < 12 ||
    expected.size > maxRunnerBundleBytes ||
    !/^[0-9a-f]{64}$/.test(expected.sha256)
  ) {
    throw new Error("invalid expected runner bundle identity");
  }
  const matches = [];
  let validBundles = 0;
  let candidates = 0;
  for (
    let offset = binary.indexOf(runnerBundleMagic);
    offset !== -1;
    offset = binary.indexOf(runnerBundleMagic, offset + 1)
  ) {
    if (++candidates > 64) throw new Error("too many runner bundle candidates");
    // A matching appended decoy must not hide a different operational bundle.
    // Count every structurally valid bundle, not only the approved digest.
    if (offset + 12 <= binary.length) {
      const headerSize = binary.readUInt32BE(offset + 8);
      if (headerSize > 0 && headerSize <= 65536 && headerSize <= binary.length - offset - 12) {
        try {
          const manifest = JSON.parse(binary.subarray(offset + 12, offset + 12 + headerSize));
          const last = manifest.entries?.at(-1);
          const size = 12 + headerSize + last?.offset + last?.packedSize;
          if (Number.isSafeInteger(size) && size <= binary.length - offset) {
            unpackRunnerBundle(binary.subarray(offset, offset + size), manifest.buildId);
            validBundles++;
          }
        } catch {
          /* An ordinary magic string in program code is not a bundle. */
        }
      }
    }
    const end = offset + expected.size;
    if (end > binary.length) continue;
    const candidate = binary.subarray(offset, end);
    if (runnerSHA256(candidate) === expected.sha256) matches.push(candidate);
  }
  if (validBundles !== 1)
    throw new Error(`expected exactly one valid runner bundle, found ${validBundles}`);
  if (matches.length !== 1)
    throw new Error(`expected one provenance-matched runner bundle, found ${matches.length}`);
  return unpackRunnerBundle(matches[0], expected.buildId);
}
