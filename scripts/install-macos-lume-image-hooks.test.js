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

test("first boot launch daemon retries failures and a late VirtioFS challenge", async () => {
  const text = await readFile(daemon, "utf8");

  assert.match(text, /<string>dev\.crabbox\.lume-firstboot<\/string>/);
  assert.match(text, /<string>\/usr\/local\/libexec\/crabbox-lume-firstboot<\/string>/);
  assert.match(text, /<key>RunAtLoad<\/key>\s*<true\/>/);
  assert.match(text, /<key>SuccessfulExit<\/key>\s*<false\/>/);
  assert.match(text, /<key>PathState<\/key>[\s\S]*\/Volumes\/My Shared Files\/challenge/);
});

test("first boot returns the rotated host key through the challenged VirtioFS share", async () => {
  const text = await readFile(firstboot, "utf8");

  assert.match(text, /trust_mount="\/Volumes\/My Shared Files"/);
  assert.match(text, /ssh-ed25519 %s\\n'[\s\S]*challenge.*platform_uuid.*expected_host_key/);
  assert.match(text, /\/bin\/rm -f "\$challenge_path"/);
  assert.match(text, /dscl \. -read "\/Users\/\$ssh_user" NFSHomeDirectory/);
  assert.match(text, /authorized_key_path" "\$ssh_home\/\.ssh\/authorized_keys"/);
  assert.match(text, /AuthenticationMethods publickey/);
  assert.match(text, /PasswordAuthentication no/);
  assert.match(text, /AuthorizedKeysFile none/);
  assert.match(text, /AuthorizedKeysCommand none/);
  assert.match(text, /TrustedUserCAKeys none/);
  assert.match(text, /AllowUsers \$ssh_user/);
  assert.match(text, /challenge_processed=true/);
  assert.match(text, /if \[\[ "\$challenge_processed" == true \]\]; then/);
  assert.match(text, /elif \[\[ "\$identity_changed" == true \]\] \|\| \[\[ ! -f "\$sshd_config_path" \]\]; then/);
  assert.match(text, /if \[\[ "\$sshd_config_changed" == true \]\]; then/);
  assert.match(text, /sshd -T -C "user=\$verify_user,host=localhost,addr=127\.0\.0\.1"/);
  assert.match(text, /require_effective_sshd 'authorizedkeysfile none'/);
  assert.match(text, /preserved_lease_user=.*lease_user_marker/);
  assert.match(text, /effective_allowusers=.*grep '\^allowusers '/);
  assert.match(text, /effective_allowusers" != "allowusers \$verify_user"/);
  assert.ok(text.indexOf("AuthorizedKeysFile none") < text.indexOf("launchctl kickstart -k system/com.openssh.sshd"));
});
