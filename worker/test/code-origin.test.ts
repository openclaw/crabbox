import { describe, expect, it } from "vitest";

import { codeOriginForLease, isIsolatedCodeRequest } from "../src/code-origin";
import type { Env } from "../src/types";

describe("isolated Code origins", () => {
  it("derives a stable opaque HTTPS origin per lease", async () => {
    const env = {
      CRABBOX_CODE_ORIGIN_TEMPLATE: "https://{lease}.code.example.test",
    } as Env;
    const first = await codeOriginForLease(env, "cbx_000000000001");
    const second = await codeOriginForLease(env, "cbx_000000000002");

    expect(first).toMatch(/^https:\/\/cbx-[a-f0-9]{32}\.code\.example\.test$/);
    expect(second).toMatch(/^https:\/\/cbx-[a-f0-9]{32}\.code\.example\.test$/);
    expect(second).not.toBe(first);
    await expect(codeOriginForLease(env, "cbx_000000000001")).resolves.toBe(first);
  });

  it("rejects invalid templates so browser Code can fail closed", async () => {
    const templates = [
      "http://{lease}.code.example.test",
      "https://code.example.test/{lease}",
      "https://code.example.test",
      "https://{lease}.code.example.test/path",
      "https://{lease}.code.example.test?token=value",
    ];
    await expect(
      Promise.all(
        templates.map((template) =>
          codeOriginForLease({ CRABBOX_CODE_ORIGIN_TEMPLATE: template }, "cbx_000000000001"),
        ),
      ),
    ).resolves.toEqual(templates.map(() => undefined));
  });

  it("matches only the configured lease path and its derived origin", async () => {
    const env = {
      CRABBOX_CODE_ORIGIN_TEMPLATE: "https://{lease}.code.example.test",
    } as Env;
    const origin = await codeOriginForLease(env, "cbx_000000000001");

    await expect(
      isIsolatedCodeRequest(
        new Request(`${origin}/portal/leases/cbx_000000000001/code/static/app.js`),
        env,
      ),
    ).resolves.toBe(true);
    await expect(
      isIsolatedCodeRequest(
        new Request(`${origin}/portal/leases/cbx_000000000002/code/static/app.js`),
        env,
      ),
    ).resolves.toBe(false);
    await expect(
      isIsolatedCodeRequest(new Request(`${origin}/portal/leases/cbx_000000000001`), env),
    ).resolves.toBe(false);
  });
});
