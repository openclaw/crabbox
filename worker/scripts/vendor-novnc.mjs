#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const packageRoot = path.join(root, "node_modules", "@novnc", "novnc");
const targetRoot = path.join(root, "public", "portal", "assets", "novnc");
const target = path.join(targetRoot, "rfb.js");
const bundleURL = "https://cdn.jsdelivr.net/npm/@novnc/novnc@1.6.0/+esm";

if (!fs.existsSync(packageRoot)) {
  throw new Error("missing @novnc/novnc; run npm ci --prefix worker first");
}

fs.rmSync(targetRoot, { recursive: true, force: true });
fs.mkdirSync(targetRoot, { recursive: true });
const response = await fetch(bundleURL);
if (!response.ok) {
  throw new Error(
    `failed to download noVNC browser bundle: ${response.status} ${response.statusText}`,
  );
}
const bundle = (await response.text()).replace(/\n\/\/# sourceMappingURL=.*$/s, "\n");
if (!bundle.includes("export{") || bundle.includes("Object.defineProperty(exports")) {
  throw new Error("downloaded noVNC bundle is not browser-compatible ESM");
}
fs.writeFileSync(target, bundle);
fs.copyFileSync(path.join(packageRoot, "LICENSE.txt"), path.join(targetRoot, "LICENSE.txt"));
