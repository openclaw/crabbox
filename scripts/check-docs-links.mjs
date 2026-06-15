#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const root = process.cwd();
const files = [path.join(root, "README.md"), ...walk(path.join(root, "docs"))];
const failures = [];

for (const file of files) {
  const markdown = fs.readFileSync(file, "utf8");
  const headings = headingAnchors(markdown);
  const links = markdown.matchAll(/\[[^\]]+\]\(([^)]+)\)/g);
  for (const match of links) {
    const href = splitMarkdownTarget(match[1].trim());
    if (!href || href.startsWith("http://") || href.startsWith("https://") || href.startsWith("mailto:")) {
      continue;
    }
    const [rawPath, rawAnchor] = href.split("#", 2);
    const linkPath = stripAngleBrackets(rawPath);
    const target = linkPath ? path.resolve(path.dirname(file), linkPath) : file;
    if (!fs.existsSync(target)) {
      failures.push(`${rel(file)} links to missing ${href}`);
      continue;
    }
    if (rawAnchor && target.endsWith(".md")) {
      const targetHeadings = target === file ? headings : headingAnchors(fs.readFileSync(target, "utf8"));
      if (!targetHeadings.has(rawAnchor)) {
        failures.push(`${rel(file)} links to missing heading ${href}`);
      }
    }
  }
}

if (failures.length) {
  console.error(failures.join("\n"));
  process.exit(1);
}

console.log(`checked ${files.length} markdown files: internal links ok`);

function walk(dir) {
  return fs
    .readdirSync(dir, { withFileTypes: true })
    .flatMap((entry) => {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) return walk(full);
      return entry.name.endsWith(".md") ? [full] : [];
    })
    .sort();
}

function headingAnchors(markdown) {
  const anchors = new Set();
  for (const rawLine of markdown.split("\n")) {
    const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
    let hashes = 0;
    while (hashes < line.length && line[hashes] === "#") {
      hashes += 1;
    }
    if (hashes < 1 || hashes > 6 || line[hashes] !== " ") {
      continue;
    }
    const base = slugify(line.slice(hashes + 1));
    if (!base) {
      continue;
    }
    let anchor = base;
    let suffix = 0;
    while (anchors.has(anchor)) {
      suffix += 1;
      anchor = `${base}-${suffix}`;
    }
    anchors.add(anchor);
  }
  return anchors;
}

function splitMarkdownTarget(href) {
  const trimmed = href.replace(/\s+(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')\s*$/, "");
  return stripAngleBrackets(trimmed);
}

function slugify(text) {
  let out = "";
  let inTag = false;
  let lastDash = false;
  for (const char of text.toLowerCase()) {
    if (char === "`") {
      continue;
    }
    if (char === "<") {
      inTag = true;
      continue;
    }
    if (char === ">") {
      inTag = false;
      continue;
    }
    if (inTag) {
      continue;
    }
    const code = char.charCodeAt(0);
    const ok = (code >= 97 && code <= 122) || (code >= 48 && code <= 57);
    if (ok) {
      out += char;
      lastDash = false;
    } else if (!lastDash) {
      out += "-";
      lastDash = true;
    }
  }
  return trimDashes(out);
}

function stripAngleBrackets(text) {
  if (text.startsWith("<") && text.endsWith(">")) return text.slice(1, -1);
  return text;
}

function rel(file) {
  return path.relative(root, file).replaceAll(path.sep, "/");
}

function trimDashes(value) {
  let start = 0;
  let end = value.length;
  while (start < end && value[start] === "-") start += 1;
  while (end > start && value[end - 1] === "-") end -= 1;
  return value.slice(start, end);
}
