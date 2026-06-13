import { describe, expect, it } from "vitest";

// The shim is CommonJS because ssh2 loads the original module with require().
const createPoly1305 = require("../src/ssh2-poly1305.cjs") as () => Promise<{
  HEAPU8: Uint8Array;
  _malloc(size: number): number;
  cwrap(
    name: string,
  ): (
    output: number,
    first: Uint8Array,
    firstLength: number,
    second: Uint8Array,
    secondLength: number,
    key: Uint8Array,
  ) => void;
}>;

describe("ssh2 Poly1305 compatibility", () => {
  it("matches the RFC 8439 test vector across split input", async () => {
    const module = await createPoly1305();
    const output = module["_malloc"](16);
    const authenticate = module.cwrap("poly1305_auth");
    const message = Buffer.from("Cryptographic Forum Research Group");
    const key = Buffer.from(
      "85d6be7857556d337f4452fe42d506a80103808afb0db2fd4abff6af4149f51b",
      "hex",
    );

    authenticate(output, message.subarray(0, 4), 4, message.subarray(4), message.length - 4, key);

    expect(Buffer.from(module.HEAPU8.subarray(output, output + 16)).toString("hex")).toBe(
      "a8061dc1305136c6c22b8baf0c0127a9",
    );
  });
});
