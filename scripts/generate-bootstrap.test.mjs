import assert from "node:assert/strict";
import { mkdtemp, readFile, writeFile, rm, mkdir, cp } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import test from "node:test";
import {
  repoRoot, loadSources, generateSources, validateArtifacts, validateCatalog, main,
  fragmentTokens, psQuote, shellQuote, imageFields,
} from "./generate-bootstrap.mjs";

const sources = await loadSources();
const fixtures = JSON.parse(await readFile(resolve(repoRoot, "testdata/bootstrap/fixtures.json"), "utf8"));
const artifactDocument = JSON.parse(await readFile(resolve(repoRoot, "recipes/bootstrap/v1/artifacts.json"), "utf8"));
// The CLI flag supports the repository's Node 22.12 floor; the programmatic
// stripTypeScriptTypes API was added later. Cache synthetic calls to keep proof cheap.
const typeScriptResults = new Map();
function evaluateTypeScript(path, name, args = []) {
  const request = JSON.stringify({ name, args });
  const key = path + request;
  if (!typeScriptResults.has(key)) {
    const program = `const module = await import(process.argv[1]);
const {name,args} = JSON.parse(process.argv[2]);
const value = name ? module[name] : module;
console.log(JSON.stringify(typeof value === "function" ? value(...args) : value));`;
    typeScriptResults.set(key, JSON.parse(run(process.execPath, ["--experimental-strip-types", "--input-type=module", "-e", program, pathToFileURL(resolve(repoRoot, path)).href, request])));
  }
  return typeScriptResults.get(key);
}
const sharedFunctions = new Set(sources.fragments.map((fragment) => "shared" + fragment.name[0].toUpperCase() + fragment.name.slice(1)));
const shared = new Proxy({}, {
  get(_target, name) {
    return sharedFunctions.has(name)
      ? (...args) => evaluateTypeScript("worker/src/bootstrap.generated.ts", name, args)
      : evaluateTypeScript("worker/src/bootstrap.generated.ts", name);
  },
});
const hasPowerShell = spawnSync("pwsh", ["-NoProfile", "-Command", "$PSVersionTable.PSVersion.ToString()"], { encoding: "utf8" }).status === 0;
const nameFor = (name) => "shared" + name[0].toUpperCase() + name.slice(1);
function parameters(fixture) {
  return { ...fixture, version: sources.constants.defaultTailscaleVersion, amd64SHA: sources.constants.defaultTailscaleAMD64SHA256, arm64SHA: sources.constants.defaultTailscaleARM64SHA256 };
}
function render(fragment, fixture) {
  const values = parameters(fixture);
  return shared[nameFor(fragment.name)](...Object.keys(fragment.parameters).map((name) => values[name]));
}
async function temporary(t) {
  const directory = await mkdtemp(join(tmpdir(), "crabbox-bootstrap-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return directory;
}
function run(command, args, options = {}) {
  const result = spawnSync(command, args, { encoding: "utf8", timeout: 60000, ...options });
  assert.equal(result.status, 0, result.stderr || result.stdout || String(result.error));
  return result.stdout;
}

test("generation is reproducible and every checked-in output is current", async () => {
  const first = generateSources(sources);
  assert.deepEqual(generateSources(await loadSources()), first);
  for (const [path, source] of first) assert.equal(await readFile(resolve(repoRoot, path), "utf8"), source, path);
});

test("artifact and catalog schemas reject malformed or ambiguous data", () => {
  for (const mutate of [
    (d) => { d.artifacts.extra = {}; },
    (d) => { d.artifacts.openSSHWin64Zip.SHA256 = "not-a-digest"; },
    (d) => { d.artifacts.openSSHWin64Zip.URL = "http://example.com/file"; },
    (d) => { d.artifacts.openSSHWin64Zip.URL = "https://user:secret@example.com/file"; },
    (d) => { d.artifacts.wslTruffleHog.Version = "1.2.3'; run"; },
    (d) => { d.artifacts.defaultCodeServer.ARM64SHA256 = 42; },
  ]) {
    const document = structuredClone(artifactDocument); mutate(document);
    assert.throws(() => validateArtifacts(document));
  }
  for (const mutate of [
    (d) => { d.default = "missing"; },
    (d) => { d.images.push(d.images[0]); },
    (d) => { d.images[0].typo = "bad"; },
    (d) => { d.images[0].AppleVMSHA256 = "0"; },
    (d) => { d.images[0].ContainerName = "ubuntu:26.04"; },
    (d) => { d.images[0].ContainerName = "docker.io/library/ubuntu@sha256:missing"; },
    (d) => { d.images[0].ContainerName = `docker.io/library/ubuntu:26.04@sha256:${"a".repeat(64)}`; },
    (d) => { d.images[0].ContainerName = `ubuntu@sha256:${"a".repeat(64)}`; },
    (d) => { d.supported = [d.default, d.default]; },
    (d) => { d.aliases.test = "missing"; },
    (d) => { d.aliases[d.default] = d.default; },
  ]) {
    const document = structuredClone(sources.catalog); mutate(document);
    assert.throws(() => validateCatalog(document));
  }
  for (const source of ["{{unknown}}", "{{raw:openSSHWin64ZipURL}}", "{{sh:unknown}}", "{{broken", "broken}}"])
    assert.throws(() => fragmentTokens({ name: "test", source, parameters: {} }, sources.constants));
  assert.throws(() => fragmentTokens({ name: "test", source: "", parameters: { user: "sh-string" } }, sources.constants));
});

test("compiled Go and TypeScript agree exactly for every shared fragment and fixture", async (t) => {
  const directory = await temporary(t);
  const expected = {};
  const entries = [];
  for (const fixture of fixtures) {
    const values = parameters(fixture);
    for (const fragment of sources.fragments) {
      const key = `${fixture.name}/${fragment.name}`;
      expected[key] = render(fragment, fixture);
      const args = Object.entries(fragment.parameters).map(([name, type]) => type.endsWith("strings") ? `[]string{${values[name].map(JSON.stringify).join(",")}}` : JSON.stringify(values[name]));
      entries.push(`${JSON.stringify(key)}: ${nameFor(fragment.name)}(${args.join(", ")}),`);
    }
  }
  const constants = Object.keys(sources.constants).map((name) => `${JSON.stringify(name)}:${name},`);
  await writeFile(join(directory, "bootstrap.go"), (await readFile(resolve(repoRoot, "internal/cli/bootstrap_generated.go"), "utf8")).replace("package cli", "package main"));
  await writeFile(join(directory, "catalog.go"), (await readFile(resolve(repoRoot, "internal/cli/os_image.go"), "utf8")).replace("package cli", "package main"));
  await writeFile(join(directory, "main.go"), `package main\nimport("encoding/json";"os")\nfunc main(){json.NewEncoder(os.Stdout).Encode(map[string]any{"fragments":map[string]string{${entries.join("\n")}},"constants":map[string]string{${constants.join("\n")}},"images":osImageSpecs,"aliases":osImageAliases,"default":defaultOSImage})}\n`);
  const actual = JSON.parse(run("go", ["run", join(directory, "main.go"), join(directory, "bootstrap.go"), join(directory, "catalog.go")]));
  for (const [key, value] of Object.entries(expected)) assert.ok(actual.fragments[key] === value, `fragment differs: ${key}`);
  assert.deepEqual(actual.constants, sources.constants);
  for (const name of Object.keys(sources.constants)) assert.equal(shared[name], sources.constants[name]);
  const catalog = evaluateTypeScript("worker/src/os-image.generated.ts", "");
  assert.equal(actual.default, catalog.defaultOSImage);
  assert.deepEqual(actual.aliases, catalog.osImageAliases);
  assert.deepEqual(Object.fromEntries(Object.entries(actual.images).map(([name, spec]) => [name, Object.fromEntries(Object.entries(spec).map(([field, value]) => [imageFields[field], value]))])), catalog.osImageSpecs);
});

test("shell and PowerShell literals preserve quoting-sensitive data without evaluation", async (t) => {
  const directory = await temporary(t);
  for (const value of ["alice's $HOME `id` \\\" 🦀", "line\nREADY\nprintf injected\r", "", "{{user}} ${value}"]) {
    assert.equal(run("bash", ["-c", `printf '%s' ${shellQuote(value)}`]), value);
    const header = shared.sharedWindowsHeader(value, value, value, [value]);
    const assignments = header.slice(header.indexOf("$user ="), header.indexOf('$base ='));
    if (hasPowerShell) {
      const output = run("pwsh", ["-NoProfile", "-NonInteractive", "-Command", `${assignments}\nConvertTo-Json -Compress @($user, $publicKey, $workRoot, $sshPorts[0])`]);
      assert.deepEqual(JSON.parse(output), [value, value, value, value]);
    }
  }
  for (const fixture of fixtures) {
    for (const fragment of sources.fragments.filter((f) => f.file.endsWith(".sh"))) {
      const file = join(directory, "syntax.sh");
      await writeFile(file, render(fragment, fixture));
      run("bash", ["-n", file]);
    }
  }
});

test("OpenSSH resolver prefers installed, then system, then PATH and fails when missing", async (t) => {
  if (!hasPowerShell) {
    t.skip("PowerShell unavailable; static and cross-runtime checks still run"); return;
  }
  const directory = await temporary(t);
  const installed = join(directory, "Program Files", "OpenSSH");
  const system = join(directory, "System32", "OpenSSH");
  await mkdir(installed, { recursive: true }); await mkdir(system, { recursive: true });
  const header = shared.sharedWindowsHeader("fixture", "fixture", "fixture", []);
  const resolver = header.slice(header.indexOf("function Resolve-CrabboxOpenSSHCommand"), header.indexOf("$user ="));
  const executable = "crabbox-fixture-keygen";
  const installPath = join(installed, executable), systemPath = join(system, executable);
  const prelude = `$ErrorActionPreference='Stop'\n${resolver}\n$openSSHInstallRoot=${psQuote(installed)}\n$openSSHSystemRoot=${psQuote(system)}\n`;
  await writeFile(installPath, ""); await writeFile(systemPath, "");
  const resolveCommand = () => run("pwsh", ["-NoProfile", "-NonInteractive", "-Command", prelude + `Resolve-CrabboxOpenSSHCommand ${psQuote(executable)}`]).trim();
  assert.equal(resolveCommand(), installPath);
  await rm(installPath); assert.equal(resolveCommand(), systemPath);
  await rm(systemPath);
  // The last fallback uses actual Get-Command resolution, without installing a service.
  const pathCommand = process.platform === "win32" ? "cmd.exe" : "sh";
  const fallback = run("pwsh", ["-NoProfile", "-NonInteractive", "-Command", prelude + `$expected=(Get-Command ${psQuote(pathCommand)}).Source; $actual=Resolve-CrabboxOpenSSHCommand ${psQuote(pathCommand)}; if ($actual -ne $expected) {throw 'PATH mismatch'}; $actual`]);
  assert.ok(fallback.trim());
  const missing = spawnSync("pwsh", ["-NoProfile", "-NonInteractive", "-Command", prelude + `Resolve-CrabboxOpenSSHCommand ${psQuote(executable)}`], { encoding: "utf8" });
  assert.notEqual(missing.status, 0); assert.match(missing.stderr, /was not found/u);
  const body = shared.sharedWindowsCore();
  assert.match(body, /\$sshKeygen = Resolve-CrabboxOpenSSHCommand "ssh-keygen.exe"/u);
  assert.match(body, /& \$sshKeygen -A/u);
  assert.match(body, /Start-Process -FilePath \$sshKeygen/u);
  assert.doesNotMatch(body, /Program Files\\OpenSSH\\ssh-keygen.exe/u);
});

test("pinned Linux installers choose the architecture digest and reject unsupported or missing pins", () => {
  for (const [label, source, prefix, amd64, arm64] of [
    ["code-server", shared.sharedCodeServerInstall(), "CS", sources.constants.defaultCodeServerAMD64SHA256, sources.constants.defaultCodeServerARM64SHA256],
    ["Tailscale", shared.sharedTailscalePinnedInstall("1.98.4", "amd-pin", "arm-pin"), "TS", "amd-pin", "arm-pin"],
  ]) {
    const selection = source.slice(0, source.indexOf(`${prefix}_INSTALL_DIR=`));
    for (const [architecture, expectedArch, expectedDigest] of [["x86_64", "amd64", amd64], ["aarch64", "arm64", arm64], ["arm64", "arm64", arm64]]) {
      assert.equal(run("bash", ["-c", `set -eu\nuname() { printf '%s' ${shellQuote(architecture)}; }\n${selection}\nprintf '%s %s' "$${prefix}_ARCH" "$${prefix}_SHA256"`]), `${expectedArch} ${expectedDigest}`, label);
    }
    const unsupported = spawnSync("bash", ["-c", `set -eu\nuname() { printf ppc64; }\n${selection}`], { encoding: "utf8" });
    assert.equal(unsupported.status, 3); assert.match(unsupported.stderr, /unsupported .* architecture/u);
  }
  const emptyPin = shared.sharedTailscalePinnedInstall("1.98.4", "", "");
  const missing = spawnSync("bash", ["-c", `set -eu\nuname() { printf x86_64; }\n${emptyPin}`], { encoding: "utf8" });
  assert.equal(missing.status, 3); assert.match(missing.stderr, /missing Tailscale checksum/u);
});

test("pinned download arguments are not evaluated as another shell program", () => {
  const version = '1.2.3"; printf INJECTED; # $(printf INJECTED)';
  const source = shared.sharedTailscalePinnedInstall(version, "checksum", "checksum");
  const end = source.indexOf("    printf '%s  %s");
  const command = `set -eu\nuname() { printf x86_64; }\nretry() { printf '%s\\n' "$@"; exit; }\n` + source.slice(0, end);
  const args = run("bash", ["-c", command]).trimEnd().split("\n");
  assert.equal(args[0], "curl");
  assert.equal(args.at(-1), `https://pkgs.tailscale.com/stable/tailscale_${version}_amd64.tgz`);
});

test("PowerShell fragments parse and download verification fails closed before extraction", async (t) => {
  if (!hasPowerShell) {
    t.skip("PowerShell unavailable"); return;
  }
  const directory = await temporary(t);
  const files = [];
  for (const fragment of sources.fragments.filter((f) => f.file.endsWith(".ps1"))) {
    const file = join(directory, fragment.file);
    await writeFile(file, render(fragment, fixtures.find((f) => f.name === "windows-quotes")));
    files.push(file);
  }
  const program = `$ErrorActionPreference='Stop'\nforeach ($file in @(${files.map(psQuote).join(",")})) { $tokens=$null; $errors=$null; [void][System.Management.Automation.Language.Parser]::ParseFile($file,[ref]$tokens,[ref]$errors); if ($errors.Count) { throw ($errors | Out-String) } }`;
  run("pwsh", ["-NoProfile", "-NonInteractive", "-Command", program]);
  const artifact = join(directory, "download.zip"); await writeFile(artifact, "fixture");
  const header = shared.sharedWindowsHeader("fixture", "fixture", "fixture", []);
  const assertion = header.slice(header.indexOf("function Assert-CrabboxFileSHA256"), header.indexOf("function New-CrabboxPassword"));
  run("pwsh", ["-NoProfile", "-NonInteractive", "-Command", `$ErrorActionPreference='Stop'\n${assertion}\n$path=${psQuote(artifact)}\nAssert-CrabboxFileSHA256 $path (Get-FileHash $path).Hash\ntry { Assert-CrabboxFileSHA256 $path ${psQuote("0".repeat(64))}; throw 'accepted wrong checksum' } catch { if ($_ -notmatch 'SHA-256 mismatch') {throw} }; if (Test-Path $path) {throw 'corrupt artifact retained'}`]);
  await assert.rejects(readFile(artifact), { code: "ENOENT" });
});

test("check detects missing and stale outputs without rewriting, and regeneration repairs them", async (t) => {
  const directory = await temporary(t);
  await cp(resolve(repoRoot, "recipes"), join(directory, "recipes"), { recursive: true });
  await mkdir(join(directory, "internal/cli"), { recursive: true });
  await mkdir(join(directory, "worker/src"), { recursive: true });
  await assert.rejects(main(["--check"], directory), /is stale/u);
  await main([], directory);
  await main(["--check"], directory);
  const stale = join(directory, "worker/src/bootstrap.generated.ts");
  await writeFile(stale, "stale fixture\n");
  await assert.rejects(main(["--check"], directory), /is stale/u);
  assert.equal(await readFile(stale, "utf8"), "stale fixture\n");
  await main([], directory);
  await main(["--check"], directory);
  await main([], directory);
  await main(["--check"], directory);
});

test("generated catalog preserves the shipped Apple VM source-verifier contract", () => {
  run("go", ["test", "./scripts/apple-vm-image-source", "-run", "^TestExtractCurrentRealSource$", "-count=1"], { cwd: repoRoot });
});
