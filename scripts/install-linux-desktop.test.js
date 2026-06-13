import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const scriptPath = "scripts/install-linux-desktop.sh";

test("public Linux desktop bootstrap is valid and keeps VNC on loopback", () => {
	const syntax = spawnSync("bash", ["-n", scriptPath], { encoding: "utf8" });
	assert.equal(syntax.status, 0, syntax.stderr || syntax.stdout);

	const script = fs.readFileSync(scriptPath, "utf8");
	assert.match(script, /-localhost -rfbport 5900/);
	assert.match(script, /\/var\/lib\/crabbox\/vnc\.password/);
	assert.match(script, /write_managed_file \/var\/lib\/crabbox\/vnc\.password 0600 "\$desktop_user" "\$desktop_group"/);
	assert.doesNotMatch(script, /vnc\.password 0640/);
	assert.match(script, /-passwdfile \/var\/lib\/crabbox\/vnc\.password/);
	assert.doesNotMatch(script, /x11vnc -storepasswd/);
	assert.match(script, /crabbox-xvfb\.service/);
	assert.match(script, /crabbox-desktop\.service/);
	assert.match(script, /crabbox-x11vnc\.service/);
	assert.match(script, /write_managed_file \/usr\/local\/bin\/crabbox-start-desktop 0755 root root/);
	assert.match(script, /#!\/bin\/bash/);
	assert.match(script, /PATH=\/usr\/sbin:\/usr\/bin:\/sbin:\/bin/);
	assert.match(script, /\/usr\/bin\/systemctl restart crabbox-desktop\.service crabbox-x11vnc\.service/);
	assert.match(script, /\/usr\/bin\/sleep 1/);
	assert.match(script, /^\s+novnc \\$/m);
	assert.match(script, /^\s+sudo \\$/m);
	assert.match(script, /^\s+util-linux \\$/m);
	assert.match(script, /^\s+websockify$/m);
	assert.match(script, /\/etc\/sudoers\.d\/crabbox-desktop-reset 0440 root root/);
	assert.match(script, /NOPASSWD: \/bin\/bash \/usr\/local\/bin\/crabbox-start-desktop/);
	assert.match(script, /visudo -cf \/etc\/sudoers\.d\/crabbox-desktop-reset/);
	assert.doesNotMatch(script, /NOPASSWD:\s+ALL/);
	assert.doesNotMatch(script, /google-chrome|microsoft-edge|brave-browser/i);
});

test("public Linux desktop bootstrap refuses managed symlinks", () => {
	const dir = fs.mkdtempSync(path.join(process.env.TMPDIR || "/tmp", "crabbox-desktop-test-"));
	const target = path.join(dir, "target");
	const link = path.join(dir, "managed");
	fs.writeFileSync(target, "do-not-overwrite");
	fs.symlinkSync(target, link);
	const result = spawnSync(
		"bash",
		["-c", 'source "$1"; require_safe_managed_file "$2"', "_", scriptPath, link],
		{ encoding: "utf8" },
	);
	assert.equal(result.status, 2, result.stderr || result.stdout);
	assert.equal(fs.readFileSync(target, "utf8"), "do-not-overwrite");
});

test("public Linux desktop bootstrap rejects unit-file injection inputs", () => {
	for (const assignment of [
		"desktop_user=$'bad\\nUser'",
		"display=$':99\\nEnvironment=BAD=1'",
		"geometry=$'1920x1080x24\\nExecStart=/bin/false'",
	]) {
		const result = spawnSync(
			"bash",
			["-c", `source ${JSON.stringify(scriptPath)}; ${assignment}; validate_config`],
			{ encoding: "utf8" },
		);
		assert.equal(result.status, 2, `${assignment}: ${result.stderr || result.stdout}`);
	}
});
