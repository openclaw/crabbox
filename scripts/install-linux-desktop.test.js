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
	assert.match(script, /-localhost yes -rfbport 5900/);
	assert.match(script, /\/var\/lib\/crabbox\/vnc\.password/);
	assert.match(script, /write_managed_file \/var\/lib\/crabbox\/vnc\.password 0600 "\$desktop_user" "\$desktop_group"/);
	assert.doesNotMatch(script, /vnc\.password 0640/);
	assert.match(script, /-SecurityTypes VncAuth -PasswordFile=\/var\/lib\/crabbox\/vnc\.pass/);
	assert.match(script, /tigervncpasswd -f/);
	assert.match(script, /write_managed_file \/var\/lib\/crabbox\/vnc\.pass 0600 "\$desktop_user" "\$desktop_group"/);
	assert.match(script, /-AlwaysShared -AcceptSetDesktopSize/);
	assert.doesNotMatch(script, /x11vnc -storepasswd/);
	assert.match(script, /crabbox-xvfb\.service/);
	assert.match(script, /crabbox-desktop\.service/);
	assert.match(script, /crabbox-x11vnc\.service/);
	assert.match(script, /write_managed_file \/usr\/local\/bin\/crabbox-start-desktop 0755 root root/);
	assert.match(script, /#!\/bin\/bash/);
	assert.match(script, /PATH=\/usr\/sbin:\/usr\/bin:\/sbin:\/bin/);
	assert.match(script, /\/usr\/bin\/systemctl restart "\$\{services\[@\]\}"/);
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

test("public Linux desktop selects the matching server for every supported geometry depth", () => {
	for (const depth of [8, 16, 24, 32]) {
		const result = spawnSync("bash", ["-c", `
			source "$1"
			geometry="1280x720x$2"
			validate_config
			write_managed_file() { printf '\\nUNIT %s\\n' "$1"; cat; }
			systemctl() { if [[ "$1" == cat ]]; then return 1; fi; printf 'SYSTEMCTL %s\\n' "$*"; }
			install_services
		`, "_", scriptPath, String(depth)], { encoding: "utf8" });
		assert.equal(result.status, 0, result.stderr);
		assert.match(result.stdout, /After=crabbox-xvfb.service\nRequires=crabbox-xvfb.service/);
		if (depth === 8) {
			assert.match(result.stdout, /Xvfb :99 -screen 0 1280x720x8 -nolisten tcp -ac/);
			assert.match(result.stdout, /x11vnc -display :99 -localhost -rfbport 5900 -forever -shared -passwdfile \/var\/lib\/crabbox\/vnc.password/);
			assert.doesNotMatch(result.stdout, /Xtigervnc/);
			assert.match(result.stdout, /SYSTEMCTL restart crabbox-xvfb.service crabbox-desktop.service crabbox-x11vnc.service/);
		} else {
			assert.match(result.stdout, new RegExp(`Xtigervnc :99 -geometry 1280x720 -depth ${depth} `));
			assert.doesNotMatch(result.stdout, /ExecStart=\/usr\/bin\/x11vnc|ExecStart=\/usr\/bin\/Xvfb/);
			assert.match(result.stdout, /SYSTEMCTL restart crabbox-xvfb.service crabbox-desktop.service\n/);
		}
	}
});

test("public Linux desktop keeps package selection and root reset units aligned", () => {
	for (const depth of [8, 16, 24, 32]) {
		const result = spawnSync("bash", ["-c", `
			source "$1"
			geometry="1280x720x$2"
			apt-get() { printf 'APT %s\\n' "$*"; }
			install_packages
			install() { :; }
			require_safe_managed_directory() { :; }
			visudo() { :; }
			write_managed_file() { if [[ "$1" == /usr/local/bin/crabbox-start-desktop ]]; then cat; else cat >/dev/null; fi; }
			install_reset_helper
		`, "_", scriptPath, String(depth)], { encoding: "utf8" });
		assert.equal(result.status, 0, result.stderr);
		const reset = result.stdout.slice(result.stdout.indexOf("#!/bin/bash"));
		const syntax = spawnSync("bash", ["-n"], { input: reset, encoding: "utf8" });
		assert.equal(syntax.status, 0, syntax.stderr);
		if (depth === 8) {
			assert.match(result.stdout, /xvfb x11vnc/);
			assert.doesNotMatch(result.stdout, /tigervnc-standalone-server/);
			assert.match(reset, /services=\(crabbox-xvfb.service crabbox-desktop.service crabbox-x11vnc.service\)/);
		} else {
			assert.match(result.stdout, /tigervnc-standalone-server tigervnc-tools/);
			assert.doesNotMatch(result.stdout, /xvfb x11vnc/);
			assert.match(reset, /services=\(crabbox-xvfb.service crabbox-desktop.service\)/);
		}
		assert.match(reset, /for service in "\$\{services\[@\]\}"/);
	}
});

test("public Linux desktop readiness requires every selected service", () => {
	for (const [depth, broken] of [[8, "crabbox-x11vnc.service"], [24, "crabbox-xvfb.service"], [8, ""], [24, ""]]) {
		const result = spawnSync("bash", ["-c", `
			source "$1"
			geometry="1280x720x$2"
			broken="$3"
			systemctl() { [[ "$*" != "is-active --quiet $broken" ]]; }
			sleep() { :; }
			ss() { printf 'LISTEN 0 128 127.0.0.1:5900 0.0.0.0:*\\n'; }
			verify_desktop
		`, "_", scriptPath, String(depth), broken], { encoding: "utf8" });
		assert.equal(result.status, broken ? 5 : 0, result.stderr || result.stdout);
		assert.match(result.stderr, broken ? /desktop services did not become ready/ : /ready user=crabbox/);
	}
});

test("public Linux desktop fails if an existing fixed-size VNC service cannot stop", () => {
	const result = spawnSync("bash", ["-c", `
		source "$1"
		write_managed_file() { cat >/dev/null; }
		systemctl() { if [[ "$1" == cat ]]; then return 0; fi; return 7; }
		install_services
	`, "_", scriptPath], { encoding: "utf8" });
	assert.equal(result.status, 7, result.stderr);
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
