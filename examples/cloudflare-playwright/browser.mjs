// Two independent contexts, a synthetic page, and no external browser requests.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { performance } from "node:perf_hooks";
import { chromium } from "playwright";

const output = process.argv[2];
if (!output || process.argv.length !== 3) throw new Error("usage: node browser.mjs <new-output-directory>");
await mkdir(dirname(output), { recursive: true });
await mkdir(output); // Refuse stale evidence or an accidental rerun into the same directory.
const started = performance.now();
const browser = await chromium.launch({ headless: true });
const runs = [];
const browserVersion = browser.version();
try {
  for (const ordinal of [1, 2]) {
    const start = performance.now();
    const id = `fixture-${ordinal}`;
    const context = await browser.newContext({ viewport: { width: 640, height: 480 }, serviceWorkers: "block" });
    try {
      await context.route("**/*", route => {
        if (route.request().url() !== "http://fixture.test/") return route.abort();
        return route.fulfill({ contentType: "text/html", body: `<!doctype html>
          <meta charset="utf-8"><title>Browser fixture</title>
          <h1>Browser fixture</h1><p id="case">${id}</p>
          <button onclick="localStorage.setItem('completed', document.querySelector('#case').textContent); this.textContent='Saved'">Save</button>` });
      });
      const page = await context.newPage();
      await page.goto("http://fixture.test/");
      assert.equal(await page.locator("h1").textContent(), "Browser fixture");
      assert.equal(await page.locator("#case").textContent(), id);
      assert.equal(await page.evaluate(() => localStorage.getItem("completed")), null);
      assert.deepEqual(await context.cookies(), []);
      await page.getByRole("button", { name: "Save", exact: true }).click();
      assert.equal(await page.getByRole("button").textContent(), "Saved");
      assert.equal(await page.evaluate(() => localStorage.getItem("completed")), id);
      await context.addCookies([{ name: "fixture", value: id, url: "http://fixture.test/" }]);
      assert.equal((await context.cookies())[0].value, id);
      const screenshot = `run-${ordinal}.png`;
      await page.screenshot({ path: join(output, screenshot) });
      const bytes = await readFile(join(output, screenshot));
      runs.push({ id, passed: true, freshStorage: true, screenshot,
        bytes: bytes.length, sha256: createHash("sha256").update(bytes).digest("hex"),
        ms: performance.now() - start });
    } finally {
      await context.close();
    }
  }
} finally {
  await browser.close();
}
const { version: playwright } = JSON.parse(await readFile("node_modules/playwright/package.json", "utf8"));
const result = { passed: true, platform: process.platform, arch: process.arch, node: process.version,
  playwright, chromium: browserVersion, browserTotalMs: performance.now() - started, runs };
await writeFile(join(output, "result.json"), JSON.stringify(result, null, 2) + "\n");
console.log(JSON.stringify(result));
