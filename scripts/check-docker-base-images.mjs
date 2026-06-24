import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";

const runtimeDockerfiles = [
  "runtimes/aws-lambda-microvm/Dockerfile",
  "worker/cloudflare-container.Dockerfile",
  "worker/azure-dynamic-sessions.Dockerfile",
  "worker/Dockerfile.node",
];

export function unpinnedBaseImages(source, file = "Dockerfile") {
  const findings = [];
  for (const [index, line] of source.split(/\r?\n/).entries()) {
    const match = /^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)/i.exec(line);
    if (match && !/@sha256:[0-9a-f]{64}$/i.test(match[1])) {
      findings.push(`${file}:${index + 1}: base image lacks a valid SHA-256 digest: ${match[1]}`);
    }
  }
  return findings;
}

async function main(files = runtimeDockerfiles) {
  const findings = [];
  for (const file of files) {
    findings.push(...unpinnedBaseImages(await readFile(file, "utf8"), file));
  }
  if (findings.length > 0) {
    console.error(findings.join("\n"));
    process.exitCode = 1;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main(process.argv.slice(2).length > 0 ? process.argv.slice(2) : runtimeDockerfiles);
}
