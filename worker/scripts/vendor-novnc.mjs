#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const packageRoot = path.join(root, "node_modules", "@novnc", "novnc");
const source = path.join(root, "node_modules", "@novnc", "novnc", "lib");
const targetRoot = path.join(root, "public", "portal", "assets", "novnc");
const target = path.join(targetRoot, "lib");

if (!fs.existsSync(source)) {
  throw new Error("missing @novnc/novnc; run npm ci --prefix worker first");
}

fs.rmSync(targetRoot, { recursive: true, force: true });
copyDirectory(source, target);
fs.copyFileSync(path.join(packageRoot, "LICENSE.txt"), path.join(targetRoot, "LICENSE.txt"));

function copyDirectory(from, to) {
  fs.mkdirSync(to, { recursive: true });
  for (const entry of fs.readdirSync(from, { withFileTypes: true })) {
    const sourcePath = path.join(from, entry.name);
    const targetPath = path.join(to, entry.name);
    if (entry.isDirectory()) {
      copyDirectory(sourcePath, targetPath);
    } else if (entry.isFile() && entry.name.endsWith(".js")) {
      fs.copyFileSync(sourcePath, targetPath);
    }
  }
}
