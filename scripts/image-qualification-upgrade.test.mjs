import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";

import { readQualificationPackage, packageFile } from "./image-qualification-package.mjs";
import { canonical } from "./image-qualification-isolation.mjs";

const root = path.resolve(import.meta.dirname, "..");
// Accepted package immediately before the creation-only contract. This test
// executes its actual private authority, rather than editing new persisted data.
const previous = "592a4982f5e3d376ad9dfda08218d429eb02dd1b";
const pkg = "qualification/first-walk/v1";
const hash = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");

test(
  "previous private package state upgrades through original-owner recovery",
  { timeout: 180_000 },
  (t) => {
    const directory = fs.mkdtempSync(path.join(os.tmpdir(), "qualification-upgrade-"));
    t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
    // Neither tested implementation nor its child processes inherit credentials,
    // user Git configuration, or a provider CLI configuration directory.
    const env = {
      PATH: process.env.PATH,
      HOME: directory,
      TMPDIR: directory,
      GIT_CONFIG_NOSYSTEM: "1",
      GIT_CONFIG_GLOBAL: os.devNull,
      CI: "true",
      NO_COLOR: "1",
    };
    const run = (command, args, cwd = root, input) =>
      execFileSync(command, args, {
        cwd,
        env,
        input,
        timeout: 90_000,
        maxBuffer: 16 * 1024 * 1024,
      });
    const oldPackage = path.join(directory, "previous-package");
    fs.mkdirSync(path.join(oldPackage, pkg), { recursive: true });
    for (const name of [
      "manifest.json",
      "runtime.patch",
      "linuxOwnershipRepair.sh",
      "wrangler.private.json",
    ])
      fs.writeFileSync(
        path.join(oldPackage, pkg, name),
        run("git", ["show", `${previous}:${pkg}/${name}`]),
      );

    function materialize(name, packageRoot) {
      const { contract, identity } = readQualificationPackage(packageRoot);
      const checkout = path.join(directory, name);
      run("git", ["clone", "--shared", "--no-checkout", "--", root, checkout]);
      run("git", ["checkout", "--detach", contract.baselineCommit], checkout);
      assert.equal(
        run("git", ["rev-parse", "HEAD^{tree}"], checkout).toString().trim(),
        contract.baselineTree,
      );
      for (const file of contract.files) {
        if (file.beforeSha256 === null)
          assert.equal(fs.existsSync(path.join(checkout, file.path)), false);
        else assert.equal(hash(packageFile(checkout, file.path)), file.beforeSha256);
      }
      const overlay = packageFile(packageRoot, `${pkg}/runtime.patch`);
      run("git", ["apply", "--check", "-"], checkout, overlay);
      run("git", ["apply", "-"], checkout, overlay);
      for (const file of contract.files)
        assert.equal(hash(packageFile(checkout, file.path)), file.afterSha256);
      const records = fs
        .readdirSync(path.join(checkout, "worker/src"), { recursive: true })
        .filter((name) => fs.statSync(path.join(checkout, "worker/src", name)).isFile())
        .map((name) => {
          const bytes = packageFile(checkout, `worker/src/${name}`);
          return { path: name.split(path.sep).join("/"), bytes: bytes.length, sha256: hash(bytes) };
        })
        .sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
      assert.equal(hash(canonical(records)), identity.workerSourceSha256);
      fs.symlinkSync(
        path.join(root, "worker/node_modules"),
        path.join(checkout, "worker/node_modules"),
        "junction",
      );
      return { checkout, identity };
    }

    const old = materialize("previous", oldPackage);
    const current = materialize("current", root);
    const target = path.join(current.checkout, "worker/test/aws-qualification-authority.test.ts");
    const url = (file) => JSON.stringify(pathToFileURL(file).href);
    const bindings = `\nconst previousAuthorityURL = ${url(path.join(old.checkout, "worker/src/aws-qualification-authority.ts"))};
const previousRecoveryURL = ${url(path.join(old.checkout, "scripts/image-qualification-recovery.mjs"))};
const currentRecoveryURL = ${url(path.join(root, "scripts/image-qualification-recovery.mjs"))};
const previousSessionURL = ${url(path.join(old.checkout, "scripts/image-qualification-run-session.mjs"))};
const currentSessionURL = ${url(path.join(root, "scripts/image-qualification-run-session.mjs"))};\n`;
    fs.appendFileSync(
      target,
      bindings +
        fs.readFileSync(path.join(root, "scripts/image-qualification-upgrade.fixture.ts"), "utf8"),
    );
    const output = run(
      process.execPath,
      [
        path.join(root, "worker/node_modules/vitest/vitest.mjs"),
        "run",
        "test/aws-qualification-authority.test.ts",
        "--reporter=default",
      ],
      path.join(current.checkout, "worker"),
    ).toString();
    assert.match(output, /UPGRADE_SETTLED/);
    assert.match(output, /lost-response/);
    assert.match(output, /cancelled/);
    // Synthetic output only; never a receipt for live AWS/Cloudflare behavior.
    console.log(
      JSON.stringify({
        previous,
        oldPackage: old.identity,
        currentPackage: current.identity,
        proof: "credential-free cross-package upgrade and settlement",
      }),
    );
    console.log(output);
  },
);
