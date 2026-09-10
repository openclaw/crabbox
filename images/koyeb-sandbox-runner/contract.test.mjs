import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const imageRoot = dirname(fileURLToPath(import.meta.url));

async function source(name) {
  return readFile(join(imageRoot, name), "utf8");
}

test("image pins the public amd64 Koyeb Sandbox release and verified artifacts", async () => {
  const dockerfile = await source("Dockerfile");

  assert.match(
    dockerfile,
    /^FROM --platform=linux\/amd64 docker\.io\/koyeb\/sandbox@sha256:64d478144162b02c919ba58de54a1942274ebca71fe43c046a18616015f4591e$/m,
  );
  assert.match(dockerfile, /^ARG TAILSCALE_VERSION=1\.102\.3$/m);
  assert.match(
    dockerfile,
    /^ARG TAILSCALE_SHA256=36ddd9b51be57ffc2990cf76323cfa13643bfbb1b8a969f6183fa164741cdef5$/m,
  );
  assert.match(dockerfile, /^ARG CODE_SERVER_VERSION=4\.136\.2$/m);
  assert.match(
    dockerfile,
    /^ARG CODE_SERVER_SHA256=83cf05cf4013da071bffade5fe207865c80966231e4f3cf5946aff3e15a966ec$/m,
  );
  assert.match(dockerfile, /^ARG NODE_VERSION=24\.20\.0$/m);
  assert.match(
    dockerfile,
    /^ARG NODE_AMD64_SHA256=855d581f8a4eb1a8117e3426de25fe02770592febcfb31369aee1ffbfee9e8ec$/m,
  );
  assert.match(dockerfile, /^ARG GOOGLE_CHROME_VERSION=152\.0\.7977\.82-1$/m);
  assert.match(
    dockerfile,
    /^ARG GOOGLE_CHROME_SHA256=4d25e4a028c78a7ae910683551c2f234792cc5595e7e3e34939f599342ada446$/m,
  );

  const tailscaleVerify = dockerfile.indexOf('printf \'%s  %s\\n\' "${TAILSCALE_SHA256}"');
  const tailscaleExtract = dockerfile.indexOf("tar -xzf /tmp/tailscale.tgz");
  const codeServerVerify = dockerfile.indexOf('printf \'%s  %s\\n\' "${CODE_SERVER_SHA256}"');
  const codeServerExtract = dockerfile.indexOf("tar -xzf /tmp/code-server.tgz");
  const nodeVerify = dockerfile.indexOf('printf \'%s  %s\\n\' "${NODE_AMD64_SHA256}"');
  const nodeExtract = dockerfile.indexOf("tar -xzf /tmp/node.tar.gz");
  const chromeVerify = dockerfile.indexOf('printf \'%s  %s\\n\' "${GOOGLE_CHROME_SHA256}"');
  const chromeInstall = dockerfile.indexOf("apt-get install -y --no-install-recommends /tmp/google-chrome.deb");
  assert.ok(tailscaleVerify >= 0 && tailscaleExtract > tailscaleVerify);
  assert.ok(codeServerVerify >= 0 && codeServerExtract > codeServerVerify);
  assert.ok(nodeVerify >= 0 && nodeExtract > nodeVerify);
  assert.ok(chromeVerify >= 0 && chromeInstall > chromeVerify);
  assert.match(
    dockerfile,
    /https:\/\/github\.com\/coder\/code-server\/releases\/download\/v\$\{CODE_SERVER_VERSION\}\/code-server-\$\{CODE_SERVER_VERSION\}-linux-amd64\.tar\.gz/,
  );
  assert.match(dockerfile, /ln -s \/usr\/local\/lib\/code-server\/bin\/code-server \/usr\/local\/bin\/code-server/);
  assert.match(
    dockerfile,
    /XDG_CONFIG_HOME=\/tmp\/code-server-config \/usr\/local\/bin\/code-server --version/,
  );
  assert.match(dockerfile, /rm -rf \/tmp\/code-server \/tmp\/code-server-config/);
  assert.doesNotMatch(dockerfile, /code-server\.dev\/install\.sh/);
  assert.match(dockerfile, /test "\$\(\/usr\/local\/bin\/node --version\)" = "v\$\{NODE_VERSION\}"/);
});

test("image leaves PID 1 and public port publication to Koyeb", async () => {
  const dockerfile = await source("Dockerfile");

  assert.doesNotMatch(dockerfile, /^(?:ENTRYPOINT|CMD)\b/m);
  assert.doesNotMatch(dockerfile, /^EXPOSE\s+(?:22|5900)\b/m);
  assert.doesNotMatch(dockerfile, /(?:bind_port|3031)/);
  assert.match(dockerfile, /Koyeb injects and starts sandbox-executor as PID 1/);
});

test("image account and runtime layout permit public-key SSH and an unprivileged desktop", async () => {
  const [dockerfile, bootstrap, desktop, health, teardown] = await Promise.all([
    source("Dockerfile"),
    source("bootstrap.sh"),
    source("desktop-session.sh"),
    source("healthcheck.sh"),
    source("teardown.sh"),
  ]);

  assert.match(dockerfile, /usermod --password 'x' crabbox/);
  assert.doesNotMatch(dockerfile, /passwd --lock crabbox/);
  for (const developmentTool of [
    "build-essential",
    "git",
    "python3",
    "python3-pip",
    "python3-venv",
    "unzip",
    "zip",
  ]) {
    const escaped = developmentTool.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    assert.match(dockerfile, new RegExp(`^      ${escaped}(?: |;)`, "m"));
  }
  for (const executable of ["/usr/bin/git", "/usr/bin/make", "/usr/bin/pip3"]) {
    assert.ok(bootstrap.includes(executable), `bootstrap dependency check missing ${executable}`);
  }
  assert.doesNotMatch(dockerfile, /\bsudo\b/);
  assert.match(dockerfile, /rm -f \/etc\/ssh\/ssh_host_\*/);
  assert.match(bootstrap, /install -d -m 0755 -o root -g root \/run\/sshd/);
  assert.match(bootstrap, /install -d -m 0711 -o root -g root "\$runtime_root"/);
  assert.match(bootstrap, /authorized_keys_file=\/var\/lib\/crabbox\/authorized_keys/);
  assert.match(
    bootstrap,
    /install -m 0640 -o root -g "\$ssh_user" "\$public_key_file" "\$authorized_keys_file"/,
  );
  assert.match(bootstrap, /CRABBOX_KOYEB_SESSION_LOG_ROOT="\$desktop_runtime"/);
  assert.match(desktop, /session_log_root="\$\{CRABBOX_KOYEB_SESSION_LOG_ROOT:-\$\{XDG_RUNTIME_DIR\}\}"/);
  assert.doesNotMatch(desktop, /CRABBOX_KOYEB_STATE_ROOT/);
  assert.match(teardown, /install -d -m 0711 -o root -g root "\$runtime_root"/);
  assert.match(teardown, /\/var\/lib\/crabbox\/authorized_keys/);
  assert.match(teardown, /\/var\/lib\/crabbox\/lease-id/);

  const nonRootExit = health.indexOf('if [[ "$(id -u)" -ne 0 ]]');
  const rootReadyCheck = health.indexOf('test -r "$ready_file"');
  assert.ok(nonRootExit >= 0 && rootReadyCheck > nonRootExit);
});

test("worker desktop endpoint launchers are preinstalled with the pinned contract", async () => {
  const [dockerfile, bootstrap, browser, terminal] = await Promise.all([
    source("Dockerfile"),
    source("bootstrap.sh"),
    source("crabbox-worker-browser"),
    source("crabbox-worker-terminal"),
  ]);

  assert.match(dockerfile, /COPY --chmod=0755 crabbox-worker-browser \/usr\/local\/bin\/crabbox-worker-browser/);
  assert.match(dockerfile, /COPY --chmod=0755 crabbox-worker-terminal \/usr\/local\/bin\/crabbox-worker-terminal/);
  assert.match(bootstrap, /printf '%s\\n' "\$lease_id" >\/var\/lib\/crabbox\/lease-id/);
  for (const required of [
    "crabbox-worker-browser does not accept arguments",
    "CRABBOX_DESKTOP_ENV=xfce",
    "DISPLAY=:99",
    "/usr/local/bin/crabbox-browser",
    "/var/lib/crabbox/lease-id",
    ".cache/crabbox/worker-browser",
    ".crabbox-launch.lock",
    "http://127.0.0.1:9222/json/version",
    "--remote-debugging-address=127.0.0.1",
    "--remote-debugging-port=9222",
  ]) {
    assert.ok(browser.includes(required), `browser launcher missing ${required}`);
  }
  for (const required of [
    "crabbox-worker-terminal does not accept arguments",
    "CRABBOX_DESKTOP_ENV=xfce",
    "DISPLAY=:99",
    "nohup /usr/bin/xfce4-terminal",
  ]) {
    assert.ok(terminal.includes(required), `terminal launcher missing ${required}`);
  }
});

test("tailscale commands default to the runner-owned userspace socket", async () => {
  const [dockerfile, wrapper] = await Promise.all([source("Dockerfile"), source("tailscale-client")]);

  assert.match(
    dockerfile,
    /install -D -m 0755 \/tmp\/tailscale\/tailscale \/usr\/local\/libexec\/crabbox-koyeb-sandbox\/tailscale/,
  );
  assert.match(dockerfile, /COPY --chmod=0755 tailscale-client \/usr\/local\/bin\/tailscale/);
  assert.doesNotMatch(dockerfile, /install -m 0755 \/tmp\/tailscale\/tailscale \/usr\/local\/bin\/tailscale/);
  assert.match(wrapper, /CRABBOX_KOYEB_TAILSCALE_SOCKET:-\/run\/crabbox-koyeb\/tailscaled\.sock/);
  assert.match(wrapper, /--socket \| --socket=\*/);
  assert.match(wrapper, /--socket="\$socket" "\$@"/);
});

test("bootstrap consumes one-call auth safely and creates only tailnet SSH ingress", async () => {
  const bootstrap = await source("bootstrap.sh");

  for (const required of [
    ': "${CRABBOX_KOYEB_LEASE_ID:?missing CRABBOX_KOYEB_LEASE_ID}"',
    ': "${CRABBOX_KOYEB_SSH_PUBLIC_KEY_FILE:?missing CRABBOX_KOYEB_SSH_PUBLIC_KEY_FILE}"',
    ': "${CRABBOX_KOYEB_TAILSCALE_AUTH_KEY:?missing CRABBOX_KOYEB_TAILSCALE_AUTH_KEY}"',
    ': "${CRABBOX_KOYEB_TAILSCALE_HOSTNAME:?missing CRABBOX_KOYEB_TAILSCALE_HOSTNAME}"',
    ': "${CRABBOX_KOYEB_TAILSCALE_TAGS:?missing CRABBOX_KOYEB_TAILSCALE_TAGS}"',
    "--tun=userspace-networking",
    "--state=mem:",
    '--auth-key="file:/dev/stdin"',
    "--advertise-tags=",
    "--tcp=22",
    "tcp://127.0.0.1:22",
    "ListenAddress 127.0.0.1",
    "PasswordAuthentication no",
    "KbdInteractiveAuthentication no",
    "AllowTcpForwarding local",
    "-localhost",
    "-rfbport 5900",
    "-noshm",
    "-nolisten tcp",
  ]) {
    assert.ok(bootstrap.includes(required), `bootstrap missing ${required}`);
  }

  assert.ok(
    bootstrap.indexOf("unset CRABBOX_KOYEB_TAILSCALE_AUTH_KEY") <
      bootstrap.indexOf('setsid /usr/local/sbin/tailscaled'),
    "the service-level child processes must not inherit the auth key",
  );
  assert.match(
    bootstrap,
    /printf '%s' "\$tailscale_auth_key" \| tailscale --socket="\$tailscale_socket" up "\$\{tailscale_up_args\[@\]\}"/,
  );
  assert.doesNotMatch(bootstrap, /--auth-key="\$\{?CRABBOX_KOYEB_TAILSCALE_AUTH_KEY/);
  assert.doesNotMatch(bootstrap, /(?:auth-key|auth_key)[^\n]*(?:>|tee|install|cp)[^\n]*\/var\//i);
  assert.doesNotMatch(bootstrap, /\b(?:systemctl|service)\b/);
  assert.doesNotMatch(bootstrap, /\b(?:bind_port|EXPOSE|0\.0\.0\.0:22|0\.0\.0\.0:5900)\b/);
});

test("bootstrap supports key-only SSH on the native Koyeb private mesh", async () => {
  const [bootstrap, health] = await Promise.all([
    source("bootstrap.sh"),
    source("healthcheck.sh"),
  ]);

  for (const required of [
    'CRABBOX_KOYEB_NETWORK',
    'koyeb-mesh',
    'CRABBOX_KOYEB_PRIVATE_HOST',
    'ListenAddress 127.0.0.1',
    '"crabbox-koyeb-sandbox-runner/v2"',
    '"koyeb-mesh"',
  ]) {
    assert.ok(bootstrap.includes(required), `private-mesh bootstrap missing ${required}`);
  }
  assert.match(bootstrap, /PasswordAuthentication no/);
  assert.match(bootstrap, /AllowUsers \$\{ssh_user\}/);
  assert.match(health, /network\.transport/);
  assert.match(health, /koyeb-mesh/);
});

test("health and teardown fail closed around the owned process set", async () => {
  const [health, teardown] = await Promise.all([
    source("healthcheck.sh"),
    source("teardown.sh"),
  ]);

  for (const required of [
    "BackendState",
    "Running",
    "nc -z 127.0.0.1 22",
    "nc -z 127.0.0.1 5900",
    "serve status --json",
    "crabbox-ready.json",
  ]) {
    assert.ok(health.includes(required), `health check missing ${required}`);
  }
  assert.match(health, /127\\\\\.0\\\\\.0\\\\\.1\|\\\\\[::1\\\\\]/);
  for (const required of [
    "serve --tcp=22 off",
    "logout",
    "/proc/${pid}/cmdline",
    "kill -- \"-${pid}\"",
    "rm -f -- \"${state_root}/crabbox-ready.json\"",
  ]) {
    assert.ok(teardown.includes(required), `teardown missing ${required}`);
  }
  assert.doesNotMatch(teardown, /\bpkill\b/);
  assert.doesNotMatch(teardown, /killall/);
});

test("all runner shell entrypoints parse with bash", async () => {
  for (const name of [
    "bootstrap.sh",
    "desktop-session.sh",
    "healthcheck.sh",
    "teardown.sh",
    "crabbox-browser",
    "tailscale-client",
    "crabbox-worker-browser",
    "crabbox-worker-terminal",
  ]) {
    const result = spawnSync("bash", ["-n", join(imageRoot, name)], { encoding: "utf8" });
    assert.equal(result.status, 0, `${name}: ${result.stderr}`);
  }
});
