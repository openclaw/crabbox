#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const root = process.cwd();
const commandDocsDir = path.join(root, "docs", "commands");
const failures = [];

const commands = readHelpCommands();
const commandNames = commands.map((command) => command.name);
const commandSet = new Set(commandNames);
const indexEntries = readCommandIndex();

for (const command of commands) {
  const file = path.join(commandDocsDir, `${command.name}.md`);
  if (!fs.existsSync(file)) {
    failures.push(`docs/commands/README.md is missing ${command.name}.md for CLI command ${command.name}`);
    continue;
  }
  const firstLine = fs.readFileSync(file, "utf8").split(/\r?\n/, 1)[0]?.trim();
  if (firstLine !== `# ${command.name}`) {
    failures.push(`${rel(file)} should start with "# ${command.name}"`);
  }
}

const seenIndexEntries = new Set();
for (const entry of indexEntries) {
  if (seenIndexEntries.has(entry.name)) {
    failures.push(`docs/commands/README.md lists ${entry.name} more than once`);
  }
  seenIndexEntries.add(entry.name);

  if (!commandSet.has(entry.name)) {
    failures.push(`docs/commands/README.md lists ${entry.name}, but CLI help does not expose that command`);
  }
  if (entry.href !== `${entry.name}.md`) {
    failures.push(`docs/commands/README.md should link ${entry.name} to ${entry.name}.md, not ${entry.href}`);
  }
}

for (const command of commandNames) {
  if (!seenIndexEntries.has(command)) {
    failures.push(`docs/commands/README.md does not link CLI command ${command}`);
  }
}

const documentedFiles = fs
  .readdirSync(commandDocsDir, { withFileTypes: true })
  .filter((entry) => entry.isFile() && entry.name.endsWith(".md") && entry.name !== "README.md")
  .map((entry) => entry.name.replace(/\.md$/, ""))
  .sort();

for (const fileCommand of documentedFiles) {
  if (!commandSet.has(fileCommand)) {
    failures.push(`docs/commands/${fileCommand}.md exists, but CLI help does not expose ${fileCommand}`);
  }
}

if (indexEntries.map((entry) => entry.name).join("\n") !== commandNames.join("\n")) {
  failures.push(
    `docs/commands/README.md order should match internal/cli/app.go help order:\n${commandNames
      .map((name) => `- [${name}](${name}.md)`)
      .join("\n")}`,
  );
}

if (failures.length) {
  console.error(failures.join("\n"));
  process.exit(1);
}

console.log(`checked ${commands.length} command docs: command surface ok`);

function readHelpCommands() {
  const appPath = path.join(root, "internal", "cli", "app.go");
  const app = fs.readFileSync(appPath, "utf8");
  const match = app.match(/Commands:\n([\s\S]*?)\n\nCommon Flows:/);
  if (!match) {
    fail(`could not find Commands block in ${rel(appPath)}`);
  }

  const commands = [];
  for (const line of match[1].split(/\r?\n/)) {
    const command = line.match(/^  ([a-z][a-z0-9-]*)\s{2,}\S/);
    if (command) {
      commands.push({ name: command[1] });
    }
  }

  if (commands.length === 0) {
    fail(`could not parse any commands from ${rel(appPath)}`);
  }
  return commands;
}

function readCommandIndex() {
  const readmePath = path.join(commandDocsDir, "README.md");
  const readme = fs.readFileSync(readmePath, "utf8");
  const entries = [];
  for (const match of readme.matchAll(/^- \[([a-z][a-z0-9-]*)\]\(([^)]+)\)$/gm)) {
    entries.push({ name: match[1], href: match[2] });
  }
  if (entries.length === 0) {
    fail(`could not parse command links from ${rel(readmePath)}`);
  }
  return entries;
}

function fail(message) {
  console.error(message);
  process.exit(1);
}

function rel(file) {
  return path.relative(root, file).replaceAll(path.sep, "/");
}
