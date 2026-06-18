import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const script = path.join(scriptDir, "proxmox-build-template.sh");

async function writeExecutable(file, body) {
  await writeFile(file, body);
  await chmod(file, 0o755);
}

async function setupFakeProxmox() {
  const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-proxmox-template-test-"));
  const log = path.join(dir, "commands.log");

  await writeExecutable(
    path.join(dir, "qm"),
    `#!/usr/bin/env bash
set -euo pipefail
printf 'qm %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  status)
    if [[ "\${CRABBOX_FAKE_TEMPLATE_EXISTS:-0}" == "1" ]]; then exit 0; fi
    exit 1
    ;;
  create)
    exit 0
    ;;
  importdisk)
    if [[ "\${CRABBOX_FAKE_IMPORTDISK_FAIL:-0}" == "1" ]]; then exit 23; fi
    exit 0
    ;;
  template)
    exit 0
    ;;
esac
`,
  );

  await writeExecutable(
    path.join(dir, "pvesm"),
    `#!/usr/bin/env bash
set -euo pipefail
printf 'pvesm %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
cat <<'EOF'
Volid Format Type Size VMID
local-lvm:vm-9400-disk-0 raw images 1G 9400
EOF
`,
  );

  await writeExecutable(
    path.join(dir, "curl"),
    `#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
dest=""
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == "--output" ]]; then
    dest="$2"
    break
  fi
  shift
done
printf 'fake image\\n' >"$dest"
`,
  );

  await writeExecutable(
    path.join(dir, "sha256sum"),
    `#!/usr/bin/env bash
set -euo pipefail
input="$(cat)"
printf 'sha256sum %s input=%s\\n' "$*" "$input" >>"\${CRABBOX_FAKE_LOG:?}"
if [[ "\${CRABBOX_FAKE_SHA_FAIL:-0}" == "1" ]]; then exit 1; fi
exit 0
`,
  );

  await writeExecutable(
    path.join(dir, "qemu-img"),
    `#!/usr/bin/env bash
set -euo pipefail
printf 'qemu-img %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
cp "$4" "$5"
`,
  );

  await writeExecutable(
    path.join(dir, "virt-customize"),
    `#!/usr/bin/env bash
set -euo pipefail
printf 'virt-customize %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
`,
  );

  return { dir, log };
}

async function runTemplateScript(extraEnv = {}) {
  const fake = await setupFakeProxmox();
  const env = {
    ...process.env,
    PATH: `${fake.dir}:${process.env.PATH}`,
    CRABBOX_FAKE_LOG: fake.log,
    CRABBOX_PROXMOX_ALLOW_NONROOT_FOR_TEST: "1",
    CRABBOX_PROXMOX_IMAGE_SHA256: "a".repeat(64),
    ...extraEnv,
  };
  const result = await new Promise((resolve) => {
    const child = spawn("bash", [script], { env });
    let stderr = "";
    child.stderr.on("data", (chunk) => {
      stderr += chunk;
    });
    child.on("close", (code) => resolve({ code, stderr }));
  });
  let log = "";
  try {
    log = await readFile(fake.log, "utf8");
  } catch {
    // no commands logged
  }
  return { ...result, log };
}

test("default image uses the pinned release URL and digest", async () => {
  const result = await runTemplateScript({
    CRABBOX_PROXMOX_IMAGE_SHA256: "",
  });

  assert.equal(result.code, 0, result.stderr);
  assert.match(
    result.log,
    /curl .*https:\/\/cloud-images\.ubuntu\.com\/releases\/noble\/release-20260518\/ubuntu-24\.04-server-cloudimg-amd64\.img/,
  );
  assert.match(
    result.log,
    /sha256sum .*input=53fdde898feed8b027d94baa9cfe8229867f330a1d9c49dc7d84465ee7f229f7 /,
  );
});

test("custom image URL requires a sha256 before download or mutation", async () => {
  const result = await runTemplateScript({
    CRABBOX_PROXMOX_IMAGE_URL: "https://images.example.test/custom.img",
    CRABBOX_PROXMOX_IMAGE_SHA256: "",
  });

  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /SHA256.*required with CRABBOX_PROXMOX_IMAGE_URL/);
  assert.equal(result.log, "");
});

test("replace mode keeps existing template when image validation fails", async () => {
  const result = await runTemplateScript({
    CRABBOX_FAKE_TEMPLATE_EXISTS: "1",
    CRABBOX_FAKE_SHA_FAIL: "1",
    CRABBOX_PROXMOX_REPLACE_TEMPLATE: "1",
  });

  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /will be replaced after the new image is validated/);
  assert.doesNotMatch(result.log, /qemu-img|virt-customize|qm create|qm importdisk|qm template/);
  assert.doesNotMatch(result.log, /qm destroy 9400 --purge 1/);
});

test("custom image with a matching sha256 preserves template creation", async () => {
  const digest = "b".repeat(64);
  const result = await runTemplateScript({
    CRABBOX_PROXMOX_IMAGE_URL: "https://images.example.test/custom.img",
    CRABBOX_PROXMOX_IMAGE_SHA256: digest,
  });

  assert.equal(result.code, 0, result.stderr);
  assert.match(result.log, /curl .*https:\/\/images\.example\.test\/custom\.img/);
  assert.match(result.log, new RegExp(`sha256sum .*input=${digest} `));
  assert.match(result.log, /qm template 9400/);
});

test("post-create failure destroys the partial VM", async () => {
  const result = await runTemplateScript({
    CRABBOX_FAKE_IMPORTDISK_FAIL: "1",
  });

  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /cleaning up incomplete Proxmox template VMID=9400/);
  assert.match(result.log, /qm create 9400/);
  assert.match(result.log, /qm destroy 9400 --purge 1/);
});

test("successful template creation does not run rollback destroy", async () => {
  const result = await runTemplateScript();

  assert.equal(result.code, 0, result.stderr);
  assert.match(result.log, /qm template 9400/);
  assert.doesNotMatch(result.log, /qm destroy 9400 --purge 1/);
});
