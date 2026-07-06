#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const metadataPath = path.join(root, "docs", "providers", "provider-metadata.json");
const readmePath = path.join(root, "docs", "providers", "README.md");
const lifecycleGuidePath = path.join(root, "docs", "features", "provider-live-smoke.md");
const benchmarkCategoriesPath = path.join(root, "internal", "cli", "provider_categories_generated.go");
const beginMarker = "<!-- BEGIN GENERATED PROVIDER MATRIX -->";
const endMarker = "<!-- END GENERATED PROVIDER MATRIX -->";
const lifecycleBeginMarker = "<!-- BEGIN GENERATED PROVIDER LIFECYCLE COVERAGE -->";
const lifecycleEndMarker = "<!-- END GENERATED PROVIDER LIFECYCLE COVERAGE -->";
const check = process.argv.includes("--check");
const allowed = {
  status: new Set(["built-in", "specialized"]),
  category: new Set([
    "brokerable-cloud",
    "direct-cloud",
    "self-hosted-virtualization",
    "local-runtime",
    "local-vm",
    "local-sandbox",
    "delegated-sandbox",
    "ci-proof-runner",
    "gpu-cloud",
    "service-control",
    "byo-ssh",
    "external-provider"
  ]),
  location: new Set(["local", "self-hosted", "cloud", "provider-managed", "byo"]),
  ssh: new Set(["crabbox-managed", "provider-specific", "no", "not-applicable"]),
  sync: new Set(["crabbox-sync", "archive-sync", "provider-owned", "none"]),
  gpu: new Set(["yes", "optional", "no", "unknown"])
};

try {
  main();
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
}

function main() {
  const metadata = JSON.parse(fs.readFileSync(metadataPath, "utf8"));
  const benchmarkCategories = formatGoSource(renderBenchmarkCategories(metadata));
  if (check) {
    if (!fs.existsSync(benchmarkCategoriesPath) || fs.readFileSync(benchmarkCategoriesPath, "utf8") !== benchmarkCategories) {
      fail("benchmark provider categories are stale; run node scripts/generate-provider-matrix.mjs");
    }
  } else {
    fs.writeFileSync(benchmarkCategoriesPath, benchmarkCategories, "utf8");
  }

  const providers = readProviderMatrix();
  validate(providers, metadata);
  const generated = render(providers, metadata);
  const readme = fs.readFileSync(readmePath, "utf8");
  const next = replaceGenerated(readme, generated, beginMarker, endMarker, readmePath);
  const lifecycleGuide = fs.readFileSync(lifecycleGuidePath, "utf8");
  const coverage = lifecycleCoverage(providers, metadata);
  const nextLifecycleGuide = replaceGenerated(
    lifecycleGuide,
    renderLifecycleCoverage(coverage),
    lifecycleBeginMarker,
    lifecycleEndMarker,
    lifecycleGuidePath
  );

  if (check) {
    if (next !== readme) {
      fail("provider decision matrix is stale; run node scripts/generate-provider-matrix.mjs");
    }
    if (nextLifecycleGuide !== lifecycleGuide) {
      fail("provider lifecycle coverage is stale; run node scripts/generate-provider-matrix.mjs");
    }
    console.log(`checked provider matrix: ${providers.length} providers`);
  } else {
    fs.writeFileSync(readmePath, next, "utf8");
    fs.writeFileSync(lifecycleGuidePath, nextLifecycleGuide, "utf8");
    console.log(`generated provider matrix: ${providers.length} providers`);
  }
}

function readProviderMatrix() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-provider-matrix-"));
  const binary = path.join(dir, process.platform === "win32" ? "crabbox.exe" : "crabbox");
  try {
    const build = spawnSync("go", ["build", "-trimpath", "-o", binary, "./cmd/crabbox"], {
      cwd: root,
      encoding: "utf8"
    });
    if (build.status !== 0) {
      fail(`go build ./cmd/crabbox failed:\n${output(build)}`);
    }
    const run = spawnSync(binary, ["providers", "--json"], {
      cwd: root,
      encoding: "utf8"
    });
    if (run.status !== 0) {
      fail(`crabbox providers --json failed:\n${output(run)}`);
    }
    return JSON.parse(run.stdout);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

function validate(providers, metadata) {
  const names = new Set(providers.map((provider) => provider.provider));
  const metadataNames = new Set(Object.keys(metadata));
  const missing = [...names].filter((name) => !metadataNames.has(name));
  const extra = [...metadataNames].filter((name) => !names.has(name));
  if (missing.length) fail(`provider metadata missing: ${missing.join(", ")}`);
  if (extra.length) fail(`provider metadata has unregistered entries: ${extra.join(", ")}`);

  for (const provider of providers) {
    const profile = metadata[provider.provider];
    for (const field of [
      "status",
      "category",
      "substrate",
      "location",
      "ssh",
      "sync",
      "gpu",
      "lifecycle",
      "cleanup",
      "bestFit",
      "caveat",
      "docs"
    ]) {
      if (typeof profile[field] !== "string" || !profile[field].trim()) {
        fail(`${provider.provider}.${field} must be a non-empty string`);
      }
    }
    for (const [field, values] of Object.entries(allowed)) {
      if (!values.has(profile[field])) {
        fail(`${provider.provider}.${field} has invalid value ${JSON.stringify(profile[field])}`);
      }
    }
    const docsPath = path.join(root, "docs", "providers", profile.docs);
    if (!profile.docs.endsWith(".md") || !fs.existsSync(docsPath)) {
      fail(`${provider.provider}.docs does not exist: ${profile.docs}`);
    }
  }
}

function render(providers, metadata) {
  const counts = new Map();
  for (const provider of providers) {
    counts.set(provider.kind, (counts.get(provider.kind) ?? 0) + 1);
  }
  const lines = [
    beginMarker,
    "",
    "## Provider decision matrix",
    "",
    "This table combines the live provider spec compiled into the CLI with curated",
    "selection metadata. Regenerate it with `node scripts/generate-provider-matrix.mjs`.",
    "`scripts/check-docs.sh` fails when provider registration, metadata, docs paths, or",
    "this generated table drift.",
    "",
    `Current built-in surface: ${providers.length} providers (${counts.get("ssh-lease") ?? 0} SSH lease, ${counts.get("delegated-run") ?? 0} delegated run, ${counts.get("service-control") ?? 0} service control).`,
    "",
    "Access terms:",
    "",
    "- **Crabbox-managed SSH**: SSH uses Crabbox's normal client; the sync column shows whether run and sync use that data plane.",
    "- **Provider-specific SSH**: an adapter-specific login helper, not the normal Crabbox data plane.",
    "- **No SSH**: the provider owns command execution end to end.",
    "",
    "| Provider | Status / category | Execution / access | Targets / substrate | Location / GPU | Lifecycle / cleanup | Best fit | Main caveat |",
    "| --- | --- | --- | --- | --- | --- | --- | --- |"
  ];

  for (const provider of providers) {
    const profile = metadata[provider.provider];
    const aliases = provider.aliases?.length ? ` (${provider.aliases.map(code).join(", ")})` : "";
    const features = provider.features?.length ? provider.features.map(code).join(", ") : "none";
    const coordinator = provider.coordinator === "supported" ? "coordinator optional" : "direct only";
    lines.push(
      `| [${escapeCell(provider.provider)}](${escapeLink(profile.docs)})${aliases} | ${escapeCell(profile.status)}; ${code(provider.kind)} · ${escapeCell(profile.category)} | ${sshLabel(profile.ssh)}; ${code(profile.sync)} · ${escapeCell(coordinator)}; features: ${features} | ${provider.targets.map(code).join(", ")}; ${escapeCell(profile.substrate)} | ${code(profile.location)}; GPU: ${escapeCell(profile.gpu)} | ${escapeCell(profile.lifecycle)}; ${escapeCell(profile.cleanup)} | ${escapeCell(profile.bestFit)} | ${escapeCell(profile.caveat)} |`
    );
  }

  lines.push("", endMarker);
  return lines.join("\n");
}

function replaceGenerated(input, generated, startMarker, endMarker, targetPath) {
  const start = input.indexOf(startMarker);
  const end = input.indexOf(endMarker);
  if (start < 0 || end < 0 || end < start) {
    fail(`missing generated markers in ${path.relative(root, targetPath)}`);
  }
  return `${input.slice(0, start)}${generated}${input.slice(end + endMarker.length)}`;
}

function lifecycleCoverage(providers, metadata) {
  const liveMatrixSource = fs.readFileSync(path.join(root, "scripts", "live-smoke.sh"), "utf8");
  const matrixProviders = new Set(
    [...liveMatrixSource.matchAll(/\bhas_provider\s+([a-z0-9-]+)/g)].map((match) => match[1])
  );
  return providers.map((provider) => {
    const name = provider.provider;
    const packageName = name === "blacksmith-testbox" ? "blacksmith" : name.replaceAll("-", "");
    const packageDir = path.join(root, "internal", "providers", packageName);
    const files = fs.existsSync(packageDir) ? fs.readdirSync(packageDir) : [];
    const hermeticLifecycle = files.some((file) => /lifecycle.*_test\.go$/.test(file) && !hasSmokeBuildTag(path.join(packageDir, file)));
    const goSmoke = files.some((file) => /(?:_live|smoke)_test\.go$/.test(file) && hasSmokeBuildTag(path.join(packageDir, file)));
    const dedicatedSmoke = dedicatedLiveSmoke(name);
    const matrixSmoke = matrixProviders.has(name);
    return {
      name,
      docs: metadata[name].docs,
      packageName,
      hermeticLifecycle,
      goSmoke,
      dedicatedSmoke,
      matrixSmoke
    };
  });
}

function hasSmokeBuildTag(file) {
  return fs.readFileSync(file, "utf8").split("\n", 8).some((line) => line.trim() === "//go:build smoke");
}

function dedicatedLiveSmoke(provider) {
  const candidates = [path.join(root, "scripts", `live-${provider}-smoke.sh`)];
  if (provider === "cloudflare") candidates.push(path.join(root, "scripts", "deploy-cloudflare-smoke.sh"));
  if (provider === "cloudflare-dynamic-workers") candidates.push(path.join(root, "scripts", "deploy-cloudflare-dynamic-workers-smoke.sh"));
  if (provider === "codesandbox") candidates.push(path.join(root, "scripts", "live-codesandbox-smoke.test.js"));
  if (provider === "proxmox") candidates.push(path.join(root, "scripts", "proxmox-live-smoke.sh"));
  if (provider === "xcp-ng") candidates.push(path.join(root, "scripts", "xcpng-live-smoke.sh"));
  return candidates.some((file) => fs.existsSync(file));
}

function renderLifecycleCoverage(coverage) {
  const hermeticCount = coverage.filter((provider) => provider.hermeticLifecycle).length;
  const liveCount = coverage.filter((provider) => provider.dedicatedSmoke || provider.matrixSmoke).length;
  const goSmokeCount = coverage.filter((provider) => provider.goSmoke).length;
  const uncoveredCount = coverage.filter(
    (provider) => !provider.hermeticLifecycle && !provider.dedicatedSmoke && !provider.matrixSmoke && !provider.goSmoke
  ).length;
  const lines = [
    lifecycleBeginMarker,
    "",
    "## Source-derived coverage matrix",
    "",
    "This matrix is generated from the registered provider list, convention-named",
    "hermetic lifecycle tests, `scripts/live-smoke.sh`, dedicated live runners, and",
    "`//go:build smoke` tests. Regenerate it with",
    "`node scripts/generate-provider-matrix.mjs`; docs CI rejects drift.",
    "",
    `Current coverage: ${coverage.length} providers; ${hermeticCount} with convention-named hermetic lifecycle tests, ${liveCount} with a live runner, ${goSmokeCount} with tagged Go smoke tests, and ${uncoveredCount} with none of those lifecycle surfaces.`,
    "",
    "| Provider | Hermetic lifecycle | Live runner | Tagged Go smoke |",
    "| --- | --- | --- | --- |"
  ];
  for (const provider of coverage) {
    const hermetic = provider.hermeticLifecycle ? `yes (\`${provider.packageName}\`)` : "—";
    const live = provider.dedicatedSmoke && provider.matrixSmoke ? "dedicated + matrix" : provider.dedicatedSmoke ? "dedicated" : provider.matrixSmoke ? "matrix" : "—";
    lines.push(`| [${provider.name}](../providers/${provider.docs}) | ${hermetic} | ${live} | ${provider.goSmoke ? "yes" : "—"} |`);
  }
  lines.push("", lifecycleEndMarker);
  return lines.join("\n");
}

function renderBenchmarkCategories(metadata) {
  const lines = [
    "// Code generated by scripts/generate-provider-matrix.mjs; DO NOT EDIT.",
    "",
    "package cli",
    "",
    "var benchmarkProviderCategories = map[string]string{"
  ];
  for (const provider of Object.keys(metadata).sort()) {
    lines.push(`\t${JSON.stringify(provider)}: ${JSON.stringify(metadata[provider].category)},`);
  }
  lines.push("}", "");
  return lines.join("\n");
}

function formatGoSource(source) {
  const result = spawnSync("gofmt", { input: source, encoding: "utf8" });
  if (result.status !== 0) {
    fail(`gofmt benchmark provider categories failed:\n${output(result)}`);
  }
  return result.stdout;
}

function sshLabel(value) {
  return {
    "crabbox-managed": "Crabbox-managed SSH",
    "provider-specific": "Provider-specific SSH",
    no: "No SSH",
    "not-applicable": "SSH not applicable"
  }[value];
}

function code(value) {
  return `\`${escapeCell(value)}\``;
}

function escapeCell(value) {
  return String(value).replaceAll("|", "\\|").replaceAll("\n", " ");
}

function escapeLink(value) {
  return encodeURI(value).replaceAll("(", "%28").replaceAll(")", "%29");
}

function output(result) {
  return `${result.stdout ?? ""}${result.stderr ?? ""}`.trim();
}

function fail(message) {
  throw new Error(message);
}
