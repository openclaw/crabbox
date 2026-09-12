import { createHash } from "node:crypto";

import { describe, expect, it } from "vitest";

import {
  base64ToBytes,
  base64URL,
  base64URLDecode,
  bytesToBase64,
  bytesToHex,
  sha256Hex,
} from "../src/encoding";

describe("binary encoding", () => {
  it("matches standard base64 and hex for every byte value", () => {
    const bytes = Uint8Array.from({ length: 256 }, (_, index) => index);
    expect(bytesToBase64(bytes)).toBe(Buffer.from(bytes).toString("base64"));
    expect(bytesToHex(bytes)).toBe(Buffer.from(bytes).toString("hex"));
    expect(base64ToBytes(bytesToBase64(bytes))).toEqual(bytes);
    expect(base64URL(bytes)).toBe(Buffer.from(bytes).toString("base64url"));
    expect(base64URLDecode(base64URL(bytes))).toEqual(bytes);
  });

  it("encodes large and sliced views without including bytes outside the view", () => {
    const data = Uint8Array.from({ length: 200_003 }, (_, index) => index % 256);
    const view = data.subarray(1, -1);
    expect(bytesToBase64(view)).toBe(Buffer.from(view).toString("base64"));
    expect(base64ToBytes(bytesToBase64(view))).toEqual(view);
  });

  it("preserves empty values and padded or unpadded URL base64", () => {
    expect(bytesToHex(new Uint8Array())).toBe("");
    expect(bytesToBase64(new Uint8Array())).toBe("");
    expect(base64URLDecode("")).toEqual(new Uint8Array());
    for (const encoded of ["YQ", "YQ=="]) {
      expect(base64URLDecode(encoded)).toEqual(new Uint8Array([97]));
    }
    expect(() => base64ToBytes("!")).toThrow(DOMException);
    expect(() => base64URLDecode("A")).toThrow(DOMException);
  });
});

describe("SHA-256 encoding", () => {
  it("matches known text vectors and UTF-8", async () => {
    expect(await sha256Hex("")).toBe(
      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    );
    expect(await sha256Hex("abc")).toBe(
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
    );
    const text = "é🦀\u0000";
    expect(await sha256Hex(text)).toBe(createHash("sha256").update(text, "utf8").digest("hex"));
  });

  it("hashes binary views as bytes, including offsets and invalid UTF-8", async () => {
    const bytes = new Uint8Array([0, 255, 128, 97, 0]);
    const view = bytes.subarray(1, 4);
    const expected = createHash("sha256").update(view).digest("hex");
    expect(await sha256Hex(view)).toBe(expected);
    expect(await sha256Hex(new DataView(bytes.buffer, 1, 3))).toBe(expected);
    expect(await sha256Hex(bytes.buffer)).toBe(createHash("sha256").update(bytes).digest("hex"));
  });
});
