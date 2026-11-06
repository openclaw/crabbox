// Run from this example's directory. No provider or credential operations.
import { mkdir, writeFile } from "node:fs/promises";
import { performance } from "node:perf_hooks";
import { spawnSync } from "node:child_process";

const args = process.argv.slice(2);
if (args.length > 1 || (args.length === 1 && args[0] !== "--with-deps")) {
  throw new Error("usage: node prepare.mjs [--with-deps]");
}
const phases = [];
await mkdir("artifacts", { recursive: true });
for (const [phase, command, commandArgs] of [
  ["npm_ci", "npm", ["ci", "--ignore-scripts", "--no-audit", "--no-fund"]],
  ["browser_install", process.execPath, ["node_modules/playwright/cli.js", "install", ...args, "chromium"]],
]) {
  const start = performance.now();
  const child = spawnSync(command, commandArgs, { stdio: "inherit" });
  phases.push({ phase, ms: performance.now() - start, exitCode: child.status, signal: child.signal });
  await writeFile("artifacts/preparation.json", JSON.stringify({ node: process.version, phases }, null, 2) + "\n");
  if (child.error || child.status !== 0) {
    throw child.error ?? new Error(`${phase} failed: exit=${child.status} signal=${child.signal}`);
  }
}
console.log("Preparation timings: artifacts/preparation.json");
