import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const pluginRoot = path.join(repoRoot, "plugins", "herdr");

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

test("Herdr plugin manifest exposes the supported Crabbox actions and panes", () => {
  const manifest = fs.readFileSync(path.join(pluginRoot, "herdr-plugin.toml"), "utf8");
  assert.match(manifest, /^id = "crabbox"$/m);
  assert.match(manifest, /^min_herdr_version = "0\.7\.0"$/m);
  assert.match(manifest, /^platforms = \["linux", "macos"\]$/m);
  assert.match(manifest, /\[\[build\]\]\ncommand = \["sh", "build\.sh"\]/);

  const actionIds = [...manifest.matchAll(/\[\[actions\]\]\nid = "([^"]+)"/g)].map(
    (match) => match[1],
  );
  assert.deepEqual(actionIds, ["boxes", "warmup", "prewarm", "connect", "run-job", "doctor"]);

  const paneIds = [...manifest.matchAll(/\[\[panes\]\]\nid = "([^"]+)"/g)].map(
    (match) => match[1],
  );
  assert.deepEqual(paneIds, ["boxes", "warmup", "prewarm", "connect", "run-job", "doctor"]);
  assert.doesNotMatch(manifest, /\[\[events\]\]/, "the plugin must not stop leases from events");
  assert.doesNotMatch(manifest, /\[\[keys\./, "keybindings belong in the user's Herdr config");
});

test("Herdr plugin shell entrypoints pass syntax checks", () => {
  for (const file of ["build.sh", "bin/open-pane.sh", "bin/pane.sh"]) {
    const result = spawnSync("sh", ["-n", path.join(pluginRoot, file)], {
      cwd: repoRoot,
      encoding: "utf8",
    });
    assert.equal(result.status, 0, `${file}: ${result.stdout}${result.stderr}`);
  }
});

test("Herdr result panes and failed connections wait and preserve exit status", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-herdr-pane-result-"));
  const installedPlugin = path.join(dir, "plugin");
  fs.cpSync(pluginRoot, installedPlugin, { recursive: true });
  writeExecutable(
    path.join(installedPlugin, "crabbox-shim.sh"),
    `#!/bin/sh
printf '<%s>\\n' "$@"
exit 7
`,
  );

  for (const command of ["doctor", "connect"]) {
    const result = spawnSync("sh", [path.join(installedPlugin, "bin", "pane.sh"), command], {
      cwd: installedPlugin,
      input: "\n",
      encoding: "utf8",
    });
    assert.equal(result.status, 7, result.stdout + result.stderr);
    assert.match(result.stdout, new RegExp(`<__herdr-plugin>\\n<${command}>`));
    assert.match(result.stdout, /Command exited with status 7\. Press Enter to close\./);
  }
});

test("Herdr long-lived panes close with their Crabbox process", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-herdr-pane-live-"));
  const installedPlugin = path.join(dir, "plugin");
  fs.cpSync(pluginRoot, installedPlugin, { recursive: true });
  writeExecutable(path.join(installedPlugin, "crabbox-shim.sh"), "#!/bin/sh\nexit 0\n");

  for (const command of ["boxes", "connect"]) {
    const result = spawnSync("sh", [path.join(installedPlugin, "bin", "pane.sh"), command], {
      cwd: installedPlugin,
      encoding: "utf8",
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.doesNotMatch(result.stdout, /Press Enter to close/);
  }
});

test("Herdr plugin build pins and preserves the Crabbox executable path", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-herdr-plugin-"));
  const installedPlugin = path.join(dir, "plugin");
  const fakeBinName = "bin with ' quote";
  const fakeBin = path.join(installedPlugin, fakeBinName);
  const fakeCrabbox = path.join(fakeBin, "crabbox");
  fs.cpSync(pluginRoot, installedPlugin, { recursive: true });
  fs.mkdirSync(fakeBin);
  writeExecutable(
    fakeCrabbox,
    `#!/bin/sh
if [ "\${1:-}" = __herdr-plugin ] && [ "\${2:-}" = context-cwd ]; then
  printf '/\\n'
  exit 0
fi
printf '<%s>\\n' "$@"
`,
  );

  const build = spawnSync("sh", ["build.sh"], {
    cwd: installedPlugin,
    env: {
      ...process.env,
      PATH: `${fakeBinName}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });
  assert.equal(build.status, 0, build.stdout + build.stderr);
  assert.match(build.stdout, /Crabbox Herdr plugin: using/);

  const shim = path.join(installedPlugin, "crabbox-shim.sh");
  assert.equal(fs.statSync(shim).mode & 0o111, 0o111);
  const invoke = spawnSync(shim, ["alpha", "two words"], {
    cwd: dir,
    encoding: "utf8",
  });
  assert.equal(invoke.status, 0, invoke.stdout + invoke.stderr);
  assert.equal(invoke.stdout, "<alpha>\n<two words>\n");
});

test("Herdr plugin build leaves an actionable shim for an old Crabbox CLI", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-herdr-plugin-old-cli-"));
  const installedPlugin = path.join(dir, "plugin");
  const fakeBin = path.join(dir, "bin");
  fs.cpSync(pluginRoot, installedPlugin, { recursive: true });
  fs.mkdirSync(fakeBin);
  writeExecutable(path.join(fakeBin, "crabbox"), "#!/bin/sh\nexit 2\n");

  const build = spawnSync("sh", ["build.sh"], {
    cwd: installedPlugin,
    env: {
      ...process.env,
      PATH: `${fakeBin}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });
  assert.equal(build.status, 1, build.stdout + build.stderr);
  assert.match(build.stderr, /requires a compatible crabbox executable/);

  const invoke = spawnSync(path.join(installedPlugin, "crabbox-shim.sh"), [], {
    encoding: "utf8",
  });
  assert.equal(invoke.status, 127);
  assert.match(invoke.stderr, /compatible Crabbox CLI was not found/);
});

test("Herdr actions preserve their invocation target", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-herdr-action-"));
  const installedPlugin = path.join(dir, "plugin");
  const fakeHerdr = path.join(dir, "herdr");
  fs.cpSync(pluginRoot, installedPlugin, { recursive: true });
  writeExecutable(
    path.join(installedPlugin, "crabbox-shim.sh"),
    `#!/bin/sh
if [ "\${1:-}" = __herdr-plugin ] && [ "\${2:-}" = context-cwd ]; then
  printf '/repo/with spaces\\n'
  exit 0
fi
exit 2
`,
  );
  writeExecutable(fakeHerdr, "#!/bin/sh\nprintf '<%s>\\n' \"$@\"\n");

  const invoke = (entrypoint, placement) => {
    const result = spawnSync("sh", [path.join(installedPlugin, "bin", "open-pane.sh"), entrypoint, placement], {
      cwd: installedPlugin,
      env: {
        ...process.env,
        HERDR_BIN_PATH: fakeHerdr,
        HERDR_PANE_ID: "workspace-1:pane-2",
        HERDR_PLUGIN_ROOT: installedPlugin,
        HERDR_WORKSPACE_ID: "workspace-1",
      },
      encoding: "utf8",
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    return result.stdout;
  };

  const split = invoke("doctor", "split");
  assert.match(split, /<--target-pane>\n<workspace-1:pane-2>/);
  assert.match(split, /<--direction>\n<right>/);
  assert.doesNotMatch(split, /<--cwd>/, "pane commands must start from the plugin root");

  const tab = invoke("connect", "tab");
  assert.match(tab, /<--workspace>\n<workspace-1>/);
  assert.doesNotMatch(tab, /<--target-pane>|<--direction>/);

  const overlay = invoke("boxes", "overlay");
  assert.doesNotMatch(overlay, /<--workspace>|<--target-pane>|<--direction>/);
});
