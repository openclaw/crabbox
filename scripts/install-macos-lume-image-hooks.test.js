import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const installer = path.join(scriptDir, "install-macos-lume-image-hooks.sh");
const firstboot = path.join(scriptDir, "macos-lume-firstboot.sh");
const daemon = path.join(scriptDir, "macos-lume-firstboot-launchdaemon.plist");

test("Lume image hook shell scripts pass syntax checks", () => {
  const result = spawnSync("bash", ["-n", installer, firstboot], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr || result.stdout);
});

test("first boot publishes identity only after sshd serves the rotated key", async () => {
  const text = await readFile(firstboot, "utf8");
  const removeKeys = text.indexOf("/bin/rm -f /etc/ssh/ssh_host_*");
  const generateKeys = text.indexOf("/usr/bin/ssh-keygen -A");
  const restartSSHD = text.indexOf("launchctl kickstart -k system/com.openssh.sshd");
  const scanKey = text.indexOf("/usr/bin/ssh-keyscan");
  const publishMarker = text.indexOf('/bin/mv -f "$tmp" "$marker"');

  assert.ok(removeKeys >= 0);
  assert.ok(removeKeys < generateKeys);
  assert.ok(generateKeys < restartSSHD);
  assert.ok(restartSSHD < scanKey);
  assert.ok(scanKey < publishMarker);
  assert.doesNotMatch(text, /kickstart -k system\/com\.openssh\.sshd[^\n]*\|\| true/);
});

test("Cua Driver remains optional while the SSH identity hook is mandatory", async () => {
  const text = await readFile(installer, "utf8");

  assert.match(text, /for file in "\$firstboot_script" "\$firstboot_plist"/);
  assert.match(text, /if \[\[ -x \/Applications\/CuaDriver\.app\/Contents\/MacOS\/cua-driver \]\]; then/);
  assert.match(text, /Cua Driver is not installed; skipped its optional LaunchAgent/);
  assert.match(text, /Lume first-boot identity hook did not become ready/);
});

test("first boot launch daemon retries failures but not success", async () => {
  const text = await readFile(daemon, "utf8");

  assert.match(text, /<string>dev\.crabbox\.lume-firstboot<\/string>/);
  assert.match(text, /<string>\/usr\/local\/libexec\/crabbox-lume-firstboot<\/string>/);
  assert.match(text, /<key>RunAtLoad<\/key>\s*<true\/>/);
  assert.match(text, /<key>SuccessfulExit<\/key>\s*<false\/>/);
});
