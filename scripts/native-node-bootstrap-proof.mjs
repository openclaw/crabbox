// Finite, opt-in artifact qualification. Never run on an operator machine.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import https from "node:https";
import path from "node:path";
import { fileURLToPath } from "node:url";

assert.equal(process.env.GITHUB_ACTIONS, "true", "requires a disposable Actions runner");
assert.ok(["darwin", "win32", "linux"].includes(process.platform));
const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const root = path.join(process.env.RUNNER_TEMP, "crabbox-native-node-proof");
fs.mkdirSync(root, { recursive: true });
const records = {
  platform: process.platform,
  arch: process.arch,
  source: process.env.GITHUB_SHA,
  scenarios: [],
  tls: [],
};
const stateFile = path.join(root, "consumer-state.json");
fs.writeFileSync(stateFile, JSON.stringify({ existing: "usable", value: 42 }));
const sha = (data) => createHash("sha256").update(data).digest("hex");
const run = (cmd, args, options = {}) => {
  const result = spawnSync(cmd, args, { encoding: "utf8", timeout: 360_000, ...options });
  process.stdout.write(result.stdout || "");
  process.stderr.write(result.stderr || "");
  assert.equal(result.status, 0, result.error?.message || `${cmd} failed: ${result.status}`);
  return result.stdout.trim();
};
async function download(url, destination, expected) {
  const response = await fetch(url, { signal: AbortSignal.timeout(120_000) });
  assert.equal(response.status, 200, url);
  const bytes = Buffer.from(await response.arrayBuffer());
  assert.equal(sha(bytes), expected, `unreviewed archive bytes: ${url}`);
  fs.writeFileSync(destination, bytes);
  records.scenarios.push({ download: url, sha256: expected });
}
const oldSHA = {
  "darwin-arm64": "8294b7aa9b03997481c06babf1e8b270c859358f27da57a11509afe537ac381d",
  "darwin-x64": "d1b5e999db158c62fe8f7267a4476b035d8bd93b1a605bac24a3f0dd166e3316",
  "win32-x64": "57f71ab3652e797d84acddc79c81cc9ff1c6ddb2a1974cdb83f00fee9bff4c73",
  "win32-arm64": "8502f4a50b458d4cc38ed8f2001556c2cd239d464920f74017926ccb1e1c157f",
}; // Released v0.79.1 bootstrap identities, source74fe694e6783664afbb718985a5337d10fb440c1.
let selected;
let prefix = [];
let command;
const cleanup = [];
try {
  if (process.platform === "linux") {
    assert.equal(process.arch, "x64", "Linux image qualification requires native amd64");
    const context = path.join(root, "image");
    fs.mkdirSync(context);
    await download(
      "https://raw.githubusercontent.com/openclaw/crabbox/74fe694e6783664afbb718985a5337d10fb440c1/scripts/install-linux-developer-tools.sh",
      path.join(context, "released.sh"),
      "453cb391ca6f87249ec8e848d08cfe8e5e78797b111876862e1af95d46d10f06",
    );
    fs.copyFileSync(
      path.join(repo, "scripts/install-linux-developer-tools.sh"),
      path.join(context, "candidate.sh"),
    );
    fs.writeFileSync(
      path.join(context, "Dockerfile"),
      `FROM ubuntu:24.04@sha256:008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3 AS released
# Developer images include full Python; python3-minimal alone lacks shutil.
RUN apt-get update && apt-get install -y --no-install-recommends python3 && python3 -c "import shutil, tempfile; t=tempfile.TemporaryDirectory(); t.cleanup()"
COPY released.sh /proof/released.sh
RUN bash /proof/released.sh --node-only && test "$(node --version)" = v24.19.0
RUN mkdir -p /var/lib/crabbox && echo '{"existing":"usable","value":42}' >/var/lib/crabbox/retained.json && sha256sum /var/lib/crabbox/retained.json >/proof/state.sha256 && bash /proof/released.sh --node-only
FROM released AS upgraded
COPY candidate.sh /proof/candidate.sh
RUN bash /proof/candidate.sh --node-only && test "$(node --version)" = v24.22.1 && sha256sum -c /proof/state.sha256 && bash /proof/candidate.sh --node-only && sha256sum -c /proof/state.sha256 && for tool in node npm npx corepack pnpm pnpx; do test "$(readlink /usr/local/bin/$tool)" = "/opt/hostedtoolcache/node/24.22.1/x64/bin/$tool"; done
FROM ubuntu:24.04@sha256:008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3 AS fresh
RUN apt-get update && apt-get install -y --no-install-recommends python3 && python3 -c "import shutil, tempfile; t=tempfile.TemporaryDirectory(); t.cleanup()"
COPY candidate.sh /proof/candidate.sh
RUN bash /proof/candidate.sh --node-only && test "$(node --version)" = v24.22.1
`,
    );
    for (const target of ["upgraded", "fresh"]) {
      const tag = `crabbox-native-node-${target}:${process.env.GITHUB_RUN_ID}`;
      run("docker", ["build", "--target", target, "--tag", tag, context]);
      cleanup.push(() => run("docker", ["image", "rm", "-f", tag]));
      records.scenarios.push({
        target,
        image: run("docker", ["image", "inspect", "--format", "{{.Id}}", tag]),
        executable: run("docker", [
          "run",
          "--rm",
          tag,
          "sh",
          "-c",
          "command -v node; readlink -f /usr/local/bin/node; node --version; sha256sum $(command -v node)",
        ]),
      });
      if (target === "upgraded") {
        command = "docker";
        prefix = ["run", "--rm", "--network=host", "-v", `${root}:/proof/fixture:ro`, tag, "node"];
      }
    }
    selected = "/proof/fixture/";
  } else {
    const platform = process.platform === "darwin" ? "darwin" : "win";
    const name = `node-v24.19.0-${platform}-${process.arch}`;
    const extension = process.platform === "darwin" ? "tar.gz" : "zip";
    const archive = path.join(root, `old.${extension}`);
    await download(
      `https://nodejs.org/dist/v24.19.0/${name}.${extension}`,
      archive,
      oldSHA[`${process.platform}-${process.arch}`],
    );
    if (process.platform === "darwin") run("tar", ["-xzf", archive, "-C", root]);
    else
      run(path.join(process.env.SystemRoot, "System32/WindowsPowerShell/v1.0/powershell.exe"), [
        "-NoProfile",
        "-NonInteractive",
        "-Command",
        `Expand-Archive -LiteralPath '${archive.replaceAll("'", "''")}' -DestinationPath '${root.replaceAll("'", "''")}'`,
      ]);
    const old = path.join(root, name, ...(process.platform === "darwin" ? ["bin"] : []));
    if (process.platform === "win32") {
      run(path.join(process.env.SystemRoot, "System32/WindowsPowerShell/v1.0/powershell.exe"), [
        "-NoProfile",
        "-NonInteractive",
        "-ExecutionPolicy",
        "Bypass",
        "-File",
        path.join(repo, "scripts/native-node-bootstrap-proof.ps1"),
        "-RepositoryRoot",
        repo,
        "-WorkRoot",
        root,
        "-OldPrefix",
        old,
      ]);
      records.component = JSON.parse(
        fs.readFileSync(path.join(root, "component.json"), "utf8").replace(/^﻿/, ""),
      );
      command = records.component.executable;
    } else {
      assert.equal(
        fs.existsSync("/usr/local/lib/crabbox"),
        false,
        "refusing an existing managed prefix",
      );
      const backup = path.join(root, "backup");
      fs.mkdirSync(backup);
      for (const tool of ["node", "npm", "npx"]) {
        const target = `/usr/local/bin/${tool}`;
        try {
          fs.lstatSync(target);
          run("sudo", ["-n", "mv", target, path.join(backup, tool)]);
          cleanup.push(() => run("sudo", ["-n", "mv", path.join(backup, tool), target]));
        } catch (error) {
          if (error.code !== "ENOENT") throw error;
        }
      }
      cleanup.push(() =>
        run("sudo", [
          "-n",
          "rm",
          "-rf",
          "/usr/local/lib/crabbox",
          "/usr/local/bin/node",
          "/usr/local/bin/npm",
          "/usr/local/bin/npx",
        ]),
      );
      const installer = path.join(repo, "recipes/bootstrap/v1/macOSNodeInstall.sh");
      for (const tool of ["node", "npm", "npx"])
        run("sudo", ["-n", "ln", "-s", path.join(old, tool), `/usr/local/bin/${tool}`]);
      run("sudo", ["-n", "bash", installer]);
      assert.equal(run("/usr/local/bin/node", ["--version"]), "v24.19.0");
      for (const tool of ["node", "npm", "npx"])
        run("sudo", ["-n", "rm", `/usr/local/bin/${tool}`]);
      run("sudo", ["-n", "bash", installer]);
      command = fs.realpathSync("/usr/local/bin/node");
      assert.equal(run(command, ["--version"]), "v24.22.1");
      const initial = sha(fs.readFileSync(command));
      run("sudo", ["-n", "bash", installer]);
      assert.equal(sha(fs.readFileSync(command)), initial);
      run("sudo", ["-n", "rm", "/usr/local/bin/node"]);
      run("sudo", ["-n", "ln", "-s", path.join(root, "missing-node"), "/usr/local/bin/node"]);
      run("sudo", ["-n", "bash", installer]);
      assert.equal(fs.realpathSync("/usr/local/bin/node"), command);
      assert.equal(sha(fs.readFileSync(command)), initial);
      for (let restart = 0; restart < 2; restart++)
        run(command, [
          "-e",
          "const fs=require('fs');const d=JSON.parse(fs.readFileSync(process.argv[1],'utf8'));if(d.existing!=='usable'||d.value!==42)process.exit(47)",
          stateFile,
        ]);
      records.component = {
        executable: command,
        sha256: initial,
        healthyOld: "retained-24.19.0",
        fresh: true,
        repeat: true,
        brokenRecovery: true,
        consumerRestarts: 2,
        stateSHA256: sha(fs.readFileSync(stateFile)),
      };
    }
    records.scenarios.push(records.component);
    selected = root + path.sep;
  }
  const oldCA = fs.readFileSync(path.join(repo, "testdata/emudhra/old-published-roots.pem"));
  assert.equal(sha(oldCA), "714d457d580922dbf1d0be8bd35ba236a842b50b0072ae791582a19adef772a5");
  fs.writeFileSync(path.join(root, "old-ca.pem"), oldCA);
  fs.mkdirSync(path.join(root, "empty-ca"));
  const hosts = ["dv", "ev", "ov"].flatMap((type) =>
    ["g3b", "g3c"].flatMap((group) =>
      ["", "-aaa"].map((aaa) => `${type}-${group}${aaa}-emudhra.we-have-a-problem.com`),
    ),
  );
  const probe = `const https=require('https');const hosts=JSON.parse(process.argv[1]);const get=host=>new Promise(r=>{const q=https.get('https://'+host+'/',{agent:false,timeout:12000},s=>{const o={host,accepted:true,status:s.statusCode,authorized:s.socket.authorized};s.resume();s.on('end',()=>r(o))});q.on('timeout',()=>q.destroy(new Error('timeout')));q.on('error',e=>r({host,accepted:false,code:e.code,message:e.message}))});(async()=>{const rows=[];for(let n=0;n<hosts.length;n+=4)rows.push(...await Promise.all(hosts.slice(n,n+4).map(get)));console.log(JSON.stringify({node:process.version,executable:process.execPath,rows,control:await get('github.com')}))})()`;
  for (const [mode, flags, variables] of [
    ["default", [], {}],
    ["extra-old", [], { NODE_EXTRA_CA_CERTS: selected + "old-ca.pem" }],
    [
      "openssl-old",
      ["--use-openssl-ca"],
      { SSL_CERT_FILE: selected + "old-ca.pem", SSL_CERT_DIR: selected + "empty-ca" },
    ],
    ["system-extra-old", ["--use-system-ca"], { NODE_EXTRA_CA_CERTS: selected + "old-ca.pem" }],
  ]) {
    let args = [...prefix, ...flags, "-e", probe, JSON.stringify(hosts)];
    if (process.platform === "linux") {
      args = [...prefix];
      const at = args.indexOf("node");
      args.splice(at - 1, 0, ...Object.entries(variables).flatMap(([k, v]) => ["-e", `${k}=${v}`]));
      args.push(...flags, "-e", probe, JSON.stringify(hosts));
    }
    const output = run(command, args, { env: { ...process.env, ...variables, NODE_OPTIONS: "" } });
    const result = JSON.parse(output.split(/\r?\n/).at(-1));
    assert.equal(result.node, "v24.22.1");
    assert.equal(result.control.accepted, true, JSON.stringify(result.control));
    assert.ok(
      result.rows.every((row) => !row.accepted && row.code === "CERT_REVOKED"),
      JSON.stringify(result),
    );
    records.tls.push({ mode, ...result });
  }
  const openssl =
    process.platform === "win32"
      ? path.join(process.env.ProgramFiles, "Git/usr/bin/openssl.exe")
      : "openssl";
  const caKey = path.join(root, "ca-key.pem"),
    caCert = path.join(root, "ca.pem");
  run(openssl, [
    "req",
    "-x509",
    "-newkey",
    "rsa:2048",
    "-nodes",
    "-keyout",
    caKey,
    "-out",
    caCert,
    "-days",
    "2",
    "-subj",
    "/CN=Crabbox native artifact proof",
  ]);
  fs.writeFileSync(path.join(root, "server.ext"), "subjectAltName=DNS:localhost\n");
  run(openssl, [
    "req",
    "-newkey",
    "rsa:2048",
    "-nodes",
    "-keyout",
    path.join(root, "server-key.pem"),
    "-out",
    path.join(root, "server.csr"),
    "-subj",
    "/CN=localhost",
  ]);
  run(openssl, [
    "x509",
    "-req",
    "-in",
    path.join(root, "server.csr"),
    "-CA",
    caCert,
    "-CAkey",
    caKey,
    "-CAcreateserial",
    "-out",
    path.join(root, "server.pem"),
    "-days",
    "2",
    "-extfile",
    path.join(root, "server.ext"),
  ]);
  const server = https.createServer(
    {
      key: fs.readFileSync(path.join(root, "server-key.pem")),
      cert: fs.readFileSync(path.join(root, "server.pem")),
    },
    (_q, response) => response.end("private-ca-ok"),
  );
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const url = `https://localhost:${server.address().port}/`;
    const code = `require('https').get(${JSON.stringify(url)},r=>{const authorized=r.socket.authorized;let b='';r.on('data',x=>b+=x);r.on('end',()=>{if(b!=='private-ca-ok'||!authorized)process.exit(48);console.log('private-ca-ok')})}).on('error',e=>{console.error(e.code);process.exit(49)})`;
    // An asynchronous child lets this process continue serving its real HTTPS response.
    const childArgs =
      process.platform === "linux"
        ? [
            ...prefix.slice(0, -2),
            "-e",
            "NODE_EXTRA_CA_CERTS=/proof/fixture/ca.pem",
            ...prefix.slice(-2),
            "-e",
            code,
          ]
        : ["-e", code];
    const { spawn } = await import("node:child_process");
    await new Promise((resolve, reject) => {
      const child = spawn(command, childArgs, {
        env: { ...process.env, NODE_OPTIONS: "", NODE_EXTRA_CA_CERTS: caCert },
        stdio: "inherit",
      });
      const timer = setTimeout(() => {
        child.kill();
        reject(new Error("private CA control timed out"));
      }, 20000);
      child.on("error", reject);
      child.on("exit", (status) => {
        clearTimeout(timer);
        status === 0 ? resolve() : reject(new Error(`private CA child failed: ${status}`));
      });
    });
    records.privateCA = { accepted: true, caSHA256: sha(fs.readFileSync(caCert)) };
  } finally {
    await new Promise((resolve) => server.close(resolve));
  }
  records.complete = true;
} finally {
  fs.writeFileSync(path.join(root, "result.json"), JSON.stringify(records, null, 2) + "\n");
  console.log("CRABBOX_NATIVE_NODE_PROOF " + JSON.stringify(records));
  for (const fn of cleanup.reverse()) fn();
}
