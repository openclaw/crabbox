import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";

const files = ["install-macos-lume-image-hooks.sh", "macos-lume-firstboot.sh", "macos-lume-firstboot-launchdaemon.plist"].map((name) => path.join(import.meta.dirname, name));
const contains = (text, needles) => needles.forEach((needle) => assert.ok(text.includes(needle), needle));

test("Lume image hooks preserve the secure bootstrap contract", async () => {
  const syntax = spawnSync("bash", ["-n", files[0], files[1]], { encoding: "utf8" });
  assert.equal(syntax.status, 0, syntax.stderr || syntax.stdout);
  const [install, boot, daemon] = await Promise.all(files.map((file) => readFile(file, "utf8")));
  const ordered = ["/bin/rm -f /etc/ssh/ssh_host_*", "/usr/bin/ssh-keygen -A", "AuthorizedKeysFile none",
    "Include /etc/ssh/sshd_config.d/00-crabbox-lease.conf", "launchctl kickstart -k system/com.openssh.sshd",
    "/usr/bin/ssh-keyscan", '/bin/mv -f "$tmp" "$marker"'].map((needle) => boot.indexOf(needle));
  assert.ok(ordered.every((position) => position >= 0));
  assert.deepEqual(ordered, [...ordered].sort((a, b) => a - b));
  assert.doesNotMatch(boot, /kickstart -k system\/com\.openssh\.sshd[^\n]*\|\| true/);
  assert.doesNotMatch(boot, /printf '%s\\n' \+/);
  contains(install, ['for file in "$firstboot_script" "$firstboot_plist"',
	'sudo rm -f "$marker"',
    "if [[ -x /Applications/CuaDriver.app/Contents/MacOS/cua-driver ]]; then", 'if launchctl bootstrap "gui/$(id -u)"',
    "it will start at the next GUI login", "Cua Driver is not installed; skipped its optional LaunchAgent",
    "Lume first-boot identity hook did not become ready"]);
  for (const pattern of [/<string>dev\.crabbox\.lume-firstboot<\/string>/,
    /<string>\/usr\/local\/libexec\/crabbox-lume-firstboot<\/string>/, /<key>RunAtLoad<\/key>\s*<true\/>/,
    /<key>SuccessfulExit<\/key>\s*<false\/>/, /<key>PathState<\/key>[\s\S]*\/Volumes\/My Shared Files\/challenge/])
    assert.match(daemon, pattern);
  contains(boot, ['trust_mount="/Volumes/My Shared Files"', '/bin/rm -f "$challenge_path"',
    'dscl . -read "/Users/$ssh_user" NFSHomeDirectory', 'authorized_key_path" "$ssh_home/.ssh/authorized_keys',
    "ssh-rsa|ecdsa-sha2-nistp", "AuthenticationMethods publickey", "PasswordAuthentication no", "AuthorizedKeysFile none",
    "AuthorizedKeysCommand none", "TrustedUserCAKeys none", "AllowUsers $ssh_user", "challenge_processed=true",
    'if [[ "$challenge_processed" == true ]]; then', 'elif [[ "$identity_changed" == true ]] || [[ ! -f "$sshd_config_path" ]]; then',
    'if [[ "$sshd_config_changed" == true ]]; then', 'sshd_main_config_path="/etc/ssh/sshd_config"',
    "Subsystem sftp /usr/libexec/sftp-server", "Include /etc/ssh/crypto.conf",
    'sshd -T -C "user=$verify_user,host=localhost,addr=127.0.0.1"', "require_effective_sshd 'authorizedkeysfile none'"]);
  for (const pattern of [/ssh-ed25519 %s\\n'[\s\S]*challenge.*platform_uuid.*expected_host_key/,
    /preserved_lease_user=.*lease_user_marker/, /effective_allowusers=.*grep '\^allowusers '/,
    /effective_allowusers" != "allowusers \$verify_user"/])
    assert.match(boot, pattern);
});
