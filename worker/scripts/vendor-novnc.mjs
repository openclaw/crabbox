#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const packageRoot = path.join(root, "node_modules", "@novnc", "novnc");
const assetRoot = path.join(root, "public", "portal", "assets", "novnc");
const bundlePath = path.join(assetRoot, "rfb.js");
const licensePath = path.join(assetRoot, "LICENSE.txt");
const packagePath = path.join(packageRoot, "package.json");

if (!fs.existsSync(packagePath)) {
  throw new Error("missing @novnc/novnc; run npm ci --prefix worker first");
}

const packageJSON = JSON.parse(fs.readFileSync(packagePath, "utf8"));
if (packageJSON.version !== "1.6.0") {
  throw new Error(`unexpected @novnc/novnc version ${packageJSON.version}; update vendored assets`);
}

for (const file of [bundlePath, licensePath]) {
  if (!fs.existsSync(file)) {
    throw new Error(`missing checked-in noVNC asset ${path.relative(root, file)}`);
  }
}

const bundle = fs.readFileSync(bundlePath, "utf8");
if (!bundle.includes("export{") || bundle.includes("Object.defineProperty(exports")) {
  throw new Error("checked-in noVNC bundle is not browser-compatible ESM");
}
