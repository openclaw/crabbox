import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { copySmokeRepo, writeExecutable } from "./test-support/smoke-fixtures.mjs";

const common = path.join(import.meta.dirname, "lib", "live-smoke-common.sh");

function runCommon(body, args = [], options = {}) {
  return spawnSync("bash", ["-c", 'source "$1"\nshift\n' + body, "smoke", common, ...args], {
    encoding: "utf8",
    ...options,
  });
}

test("sourcing common smoke helpers only defines functions", () => {
  for (const options of ["set +e", "set -euo pipefail"]) {
    const result = spawnSync(
      "bash",
      [
        "-c",
        `${options}
trap ':' USR1
export SMOKE_SENTINEL='unchanged value'
before="$(pwd; set +o; trap -p; export -p)"
source "$1"
after="$(pwd; set +o; trap -p; export -p)"
[[ "$before" == "$after" ]]
`,
        "smoke",
        common,
      ],
      { encoding: "utf8" },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, "");
    assert.equal(result.stderr, "");
  }
});

test("slug validation accepts exact supported fields anywhere in dictionaries and lists", () => {
  const slug = "smoke-123";
  const records = [
    { labels: { slug } },
    ...["slug", "name", "id", "leaseId"].map((key) => ({ [key]: slug })),
  ];
  for (const record of records) {
    for (const payload of [record, [record], { arbitrary: [null, { child: record }] }]) {
      const result = runCommon('slug="$1"\nvalidate_list_json_contains_slug "list --json" "$2"', [
        slug,
        JSON.stringify(payload),
      ]);
      assert.equal(result.status, 0, JSON.stringify(payload) + result.stderr);
      assert.equal(result.stdout + result.stderr, "");
    }
  }
});

test("slug validation rejects unrelated values, aliases, scalar values, and partial matches", () => {
  const slug = "smoke-123";
  const payloads = [
    [],
    {},
    null,
    false,
    123,
    slug,
    [slug],
    { other: slug },
    { lease_id: slug },
    { labels: { other: slug } },
    { slug: slug.toUpperCase() },
    { name: `prefix-${slug}` },
    { id: `${slug}-suffix` },
    { slug: 123 },
  ];
  for (const payload of payloads) {
    const result = runCommon(
      'slug="$1"\nvalidate_list_json_contains_slug "list --json" "$2"\nprintf unreachable',
      [slug, JSON.stringify(payload)],
    );
    assert.equal(result.status, 1, JSON.stringify(payload) + result.stderr);
    assert.equal(result.stdout, "");
    assert.equal(
      result.stderr,
      "classification=validation_failed command=list\\ --json exit=1\n" +
        `list JSON did not include slug ${slug}\n`,
    );
  }
  const numeric = runCommon("slug=123\nvalidate_list_json_contains_slug list '{\"id\":123}'");
  assert.equal(numeric.status, 1, numeric.stderr);
});

test("empty inventory validation accepts only an empty JSON list and preserves display labels", () => {
  for (const payload of ["[]", " \n [ ] \t"]) {
    const result = runCommon('validate_list_json_empty list "$1" Example', [payload]);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout + result.stderr, "");
  }
  for (const label of ["DigitalOcean", "Linode", "OVH", "Scaleway", "Tencent Cloud", "Vultr"]) {
    for (const payload of ["{}", "null", "false", "0", '""', "[{}]", "[[]]"]) {
      const result = runCommon(
        'validate_list_json_empty "list --json" "$1" "$2"\nprintf unreachable',
        [payload, label],
      );
      assert.equal(result.status, 1, result.stderr);
      assert.equal(result.stdout, "");
      assert.equal(
        result.stderr,
        "classification=validation_failed command=list\\ --json exit=1\n" +
          `${label} Crabbox inventory is not empty\n`,
      );
    }
  }
});

test("both JSON validators classify malformed input without losing parser diagnostics", () => {
  for (const validator of ["validate_list_json_contains_slug", "validate_list_json_empty"]) {
    const result = runCommon(`slug=smoke\n${validator} list '{"broken":' Example`);
    assert.equal(result.status, 1, result.stderr);
    assert.equal(result.stdout, "");
    assert.match(
      result.stderr,
      /^classification=validation_failed command=list exit=1\ninvalid JSON: /,
    );
  }
});

test("capture preserves argument boundaries, merges streams, and normalizes trailing newlines", () => {
  const result = runCommon(`
run_capture ignored bash -c 'printf "<%s><%s>" "$1" "$2"; printf stderr >&2; printf "\\n\\n"' command 'a b' '*'
`);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, "<a b><*>stderr\n");
  assert.equal(result.stderr, "");
});

test("failed capture calls the provider classifier and exits with the original status", () => {
  const result = runCommon(`
classify_blocker() {
  printf 'provider-blocker command=%q exit=%s\\n' "$1" "$2" >&2
  printf '%s\\n' "$3" >&2
}
trap 'printf "exit=%s\\n" "$?" >&2' EXIT
set +e
run_capture 'doctor --name a b' bash -c 'printf out; printf "err\\n\\n" >&2; exit 37'
printf unreachable
`);
  assert.equal(result.status, 37, result.stderr);
  assert.equal(result.stdout, "");
  assert.equal(
    result.stderr,
    "provider-blocker command=doctor\\ --name\\ a\\ b exit=37\nouterr\nexit=37\n",
  );
});

test("capture and successful validators leave errexit enabled", () => {
  for (const command of [
    "run_capture command true",
    'slug=smoke\nvalidate_list_json_contains_slug list \'{"slug":"smoke"}\'',
    "validate_list_json_empty list '[]' Example",
  ]) {
    const result = runCommon(`set +e\n${command}\nfalse\nprintf unreachable`);
    assert.equal(result.status, 1, result.stderr);
    assert.doesNotMatch(result.stdout, /unreachable/);
  }
});

test("local key snapshots retain both roots, regular files only, and sorted unique paths", (t) => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-smoke-keys-"));
  t.after(() => fs.rmSync(home, { recursive: true, force: true }));
  const macRoot = path.join(home, "Library", "Application Support", "crabbox", "testboxes");
  const xdg = path.join(home, "config");
  const xdgRoot = path.join(xdg, "crabbox", "testboxes");
  const files = [path.join(macRoot, "b"), path.join(xdgRoot, "nested", "a")];
  for (const file of files) {
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, "fixture");
  }
  fs.symlinkSync(files[0], path.join(xdgRoot, "linked-key"));
  const env = { ...process.env, HOME: home, XDG_CONFIG_HOME: xdg, LC_ALL: "C" };
  const result = runCommon("local_testbox_key_snapshot", [], { env });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, files.sort().join("\n") + "\n");

  env.XDG_CONFIG_HOME = path.join(home, "Library", "Application Support");
  const duplicate = runCommon("local_testbox_key_snapshot", [], { env });
  assert.equal(duplicate.stdout, path.join(macRoot, "b") + "\n");
  fs.renameSync(xdg, path.join(home, ".config"));
  delete env.XDG_CONFIG_HOME;
  const fallback = runCommon("local_testbox_key_snapshot", [], { env });
  assert.equal(fallback.status, 0, fallback.stderr);
  assert.equal(
    fallback.stdout,
    files
      .map((file) => file.replace("/config/", "/.config/"))
      .sort()
      .join("\n") + "\n",
  );
});

test("smoke fixtures copy only explicit dependencies and work outside a root with spaces", (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox smoke fixture-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const source = path.join(dir, "source");
  fs.mkdirSync(path.join(source, "lib"), { recursive: true });
  const script = path.join(source, "smoke.sh");
  writeExecutable(
    script,
    '#!/usr/bin/env bash\nsource "$(dirname "${BASH_SOURCE[0]}")/lib/live-smoke-common.sh"\nvalidate_list_json_empty list "[]" Example\n',
  );
  fs.copyFileSync(common, path.join(source, "lib", "live-smoke-common.sh"));
  fs.writeFileSync(path.join(source, "unrelated"), "not a dependency");
  const plain = copySmokeRepo(path.join(dir, "plain"), script);
  assert.deepEqual(fs.readdirSync(path.join(plain.tempRoot, "scripts")), ["smoke.sh"]);
  assert.equal(fs.statSync(plain.smokeScript).mode & 0o777, 0o755);

  const copied = copySmokeRepo(path.join(dir, "copied"), script, ["lib/live-smoke-common.sh"]);
  assert.equal(
    fs
      .lstatSync(path.join(copied.tempRoot, "scripts", "lib", "live-smoke-common.sh"))
      .isSymbolicLink(),
    false,
  );
  fs.rmSync(source, { recursive: true });
  const result = spawnSync("bash", [copied.smokeScript], { cwd: dir, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout + result.stderr, "");
  assert.throws(
    () => copySmokeRepo(path.join(dir, "missing"), copied.smokeScript, ["missing.sh"]),
    { code: "ENOENT" },
  );
});
