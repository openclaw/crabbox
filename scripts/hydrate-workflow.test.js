import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");
const workflow = path.join(repoRoot, ".github", "workflows", "hydrate.yml");
const text = await readFile(workflow, "utf8");
const actionHash = "67a5bacd381cf1b90fd8490e32aa40092094176db43e75493164941a6a072446";
const matcherHash = "03d59f953e08fdbe12c43f98bce08e9daa6d0ff5b5920111c9b23dd4e1991d06";

function qualificationPython() {
  const opener = "          /usr/bin/python3 -I - <<'PY'\n";
  const start = text.indexOf(opener);
  assert.ok(start >= 0, "qualification Python opener missing");
  const end = text.indexOf("\n          PY\n", start + opener.length);
  assert.ok(end >= 0, "qualification Python terminator missing");
  return text.slice(start + opener.length, end).split("\n").map((line) => {
    assert.ok(line === "" || line.startsWith("          "));
    return line.slice(10);
  }).join("\n");
}

test("hydrate workflow does not mutate shared runner tool cache", async () => {
  assert.match(text, /uses:\s+actions\/setup-go@[0-9a-f]{40}/);
  assert.doesNotMatch(text, /rm\s+-rf\s+["']?\$RUNNER_TOOL_CACHE\/go/);
  assert.doesNotMatch(text, /tar\s+-C\s+["']?\$RUNNER_TOOL_CACHE/);
});

test("image qualification preserves default hydration and custom runner labels", () => {
  const [normal, qualification] = text.split("\n  image-toolcache:\n");
  assert.ok(qualification);
  assert.match(normal, /default: hydrate/);
  assert.match(normal, /if: inputs\.crabbox_job != 'image-toolcache'/);
  assert.match(qualification, /if: inputs\.crabbox_job == 'image-toolcache'/);
  for (const job of [normal, qualification]) {
    assert.match(job, /runs-on: \[self-hosted, "\$\{\{ inputs\.crabbox_runner_label \}\}"\]/);
  }
  assert.match(normal, /go-version-file: go\.mod/);
  assert.match(normal, /run: npm ci --prefix worker/);
  assert.match(normal, /mv "\$\{state\}\.tmp" "\$state"/);
  assert.doesNotMatch(qualification, /npm ci|go-version-file|--reclaim|actions\/setup-go@/);
});

test("qualification authenticates upstream action bytes and verifies Runner-applied state", () => {
  const source = qualificationPython();
  assert.match(text, /repository: actions\/setup-go\n\s+ref: 924ae3a1cded613372ab5595356fb5720e22ba16/);
  assert.match(text, /path: \.crabbox-setup-go\n\s+persist-credentials: false/);
  assert.ok(source.includes(actionHash));
  assert.ok(source.includes(matcherHash));
  assert.match(source, /"INPUT_GO-VERSION": "1\.27\.1", "INPUT_CHECK-LATEST": "false"/);
  assert.match(source, /"INPUT_CACHE": "false", "INPUT_ARCHITECTURE": "x64", "INPUT_TOKEN": ""/);
  assert.doesNotMatch(source, /\/sys\/class\/net|\/proc\/net\/route/);
  assert.match(source, /resource\.setrlimit\(resource\.RLIMIT_FSIZE, \(65536, 65536\)\)/);
  assert.match(text, /"\$GOTOOLCHAIN" == local/);
  assert.match(text, /"\$\(command -v go\)" == "\$RUNNER_TOOL_CACHE\/go\/1\.27\.1\/x64\/bin\/go"/);
  assert.match(text, /"\$\(go env GOROOT\)" == "\$RUNNER_TOOL_CACHE\/go\/1\.27\.1\/x64"/);
});

const fixture = String.raw`
import contextlib, json, os, pathlib, pwd, resource, shutil, subprocess, sys, tempfile, types
from unittest import mock

source = sys.stdin.read()
mode = sys.argv[1]
with tempfile.TemporaryDirectory(prefix="crabbox-hydrate-test-") as temporary:
    root = pathlib.Path(temporary).resolve()
    home = root / "home"
    runner = home / "actions-runner"
    cache = runner / "_work/_tool"
    slot = cache / "go/1.27.1/x64"
    slot.mkdir(parents=True)
    marker = slot.with_name("x64.complete")
    marker.touch()
    (runner / ".runner").write_text(json.dumps({"workFolder": "_work" if mode != "work-folder" else "other"}))
    workspace = root / "workspace"
    action = workspace / ".crabbox-setup-go"
    (action / "dist/setup").mkdir(parents=True)
    (action / "dist/setup/index.js").write_text("fixture action\n")
    (action / "matchers.json").write_text("fixture matchers\n")
    output = root / "outputs"
    output.mkdir()
    scratch = root / "scratch"
    scratch.mkdir()
    for name in ("env", "path", "output", "state"):
        (output / name).touch()
    if mode == "missing-slot":
        marker.unlink()
    if mode == "writable":
        slot.chmod(0o777)
    if mode == "symlink":
        marker.unlink()
        marker.symlink_to(runner / ".runner")
    if mode == "action-hash":
        (action / "dist/setup/index.js").write_text("changed action\n")
    if mode == "matcher-hash":
        (action / "matchers.json").write_text("changed matchers\n")
    environment = {
        "HOME": str(home), "USER": "fixture-user", "LOGNAME": "fixture-user", "PATH": "/fixture/bin:/usr/bin:/bin",
        "RUNNER_TOOL_CACHE": str(cache), "RUNNER_TOOLSDIRECTORY": "/different-secondary",
        "AGENT_TOOLSDIRECTORY": "", "agent.ToolsDirectory": "/another-secondary",
        "RUNNER_TEMP": str(scratch), "RUNNER_OS": "Linux", "RUNNER_ARCH": "X64",
        "GITHUB_WORKSPACE": str(workspace), "GITHUB_ACTIONS": "true", "CI": "true",
        "AWS_SECRET_ACCESS_KEY": "fixture-secret", "GH_TOKEN": "fixture-secret",
        "GITHUB_TOKEN": "fixture-secret", "ACTIONS_RUNTIME_TOKEN": "fixture-secret",
    }
    environment.update({"GITHUB_" + name.upper(): str(output / name) for name in ("env", "path", "output", "state")})
    if mode == "custom-cache":
        environment["RUNNER_TOOL_CACHE"] = str(root / "custom-cache")
    if mode in ("custom-url", "custom-url-input"):
        environment["GO_DOWNLOAD_BASE_URL" if mode == "custom-url" else "INPUT_GO-DOWNLOAD-BASE-URL"] = "https://example.invalid"
    if mode == "missing-env":
        del environment["GITHUB_ENV"]
    original_lstat = pathlib.Path.lstat
    original_read = pathlib.Path.read_text
    original_iterdir = pathlib.Path.iterdir
    calls = []
    ip_calls = []
    mount_reads = []
    def metadata(path, *args, **kwargs):
        value = original_lstat(path, *args, **kwargs)
        return types.SimpleNamespace(st_mode=value.st_mode, st_uid=1002 if mode == "foreign" and path == marker else 1001)
    def read(path, *args, **kwargs):
        if str(path) == "/proc/222/stat":
            return "222 (python3) S " + ("1" if mode == "ancestry" else "333") + " 0"
        if str(path) == "/sys/class/net/lo/flags":
            mount_reads.append(str(path))
            return "0x1"
        if str(path) == "/proc/net/route":
            mount_reads.append(str(path))
            return "header\nhost-route\n"
        return original_read(path, *args, **kwargs)
    def entries(path):
        if str(path) == "/sys/class/net":
            mount_reads.append(str(path))
            return iter([pathlib.Path("/sys/class/net/lo"), pathlib.Path("/sys/class/net/eth0")])
        return original_iterdir(path)
    def run(arguments, **options):
        if arguments[0] == "/usr/bin/systemctl":
            return types.SimpleNamespace(stdout="MainPID=333\nActiveState=" + ("inactive" if mode == "service" else "active") +
                                         "\nUser=fixture-user\nWorkingDirectory=" + str(runner) + "\n")
        if arguments[0] == "/usr/sbin/ip":
            ip_calls.append(arguments)
            assert options == {"check": True, "capture_output": True, "text": True, "timeout": 5}
            kind = arguments[2]
            assert arguments == (["/usr/sbin/ip", "-j", "link", "show"] if kind == "link" else
                                 ["/usr/sbin/ip", "-j", "route", "show", "table", "all"])
            if mode == "ip-" + kind + "-missing":
                raise FileNotFoundError("fixture-network-private")
            if mode == "ip-" + kind + "-nonzero":
                raise subprocess.CalledProcessError(7, arguments, stderr="fixture-network-private")
            if mode == "ip-" + kind + "-timeout":
                raise subprocess.TimeoutExpired(arguments, 5, stderr="fixture-network-private")
            if mode == "ip-" + kind + "-json":
                return types.SimpleNamespace(stdout="invalid fixture-network-private")
            flags = ["LOOPBACK", "UP"] if mode == "network-up" else ["LOOPBACK"]
            rows = [{"ifname": "lo", "flags": flags}]
            if mode == "network-links":
                rows.append({"ifname": "eth0", "flags": ["UP"]})
            if mode == "network-empty":
                rows = []
            if mode == "network-shape":
                rows = {}
            if mode == "network-name":
                rows[0]["ifname"] = "eth0"
            if mode == "flags-missing":
                del rows[0]["flags"]
            if mode == "flags-string":
                rows[0]["flags"] = "LOOPBACK"
            if mode == "flags-invalid":
                rows[0]["flags"] = ["LOOPBACK", 1]
            if mode == "flags-empty":
                rows[0]["flags"] = []
            if kind == "route":
                rows = [{"dst": "default"}] if mode == "network-route" else []
            return types.SimpleNamespace(stdout=json.dumps(rows))
        calls.append(arguments)
        assert arguments[:8] == ["sudo", "-n", "/usr/bin/timeout", "--kill-after=5", "90", "/usr/bin/unshare", "--net", "--"]
        start = arguments.index("env") + 2
        end = arguments.index("/usr/bin/python3")
        forwarded = dict(value.split("=", 1) for value in arguments[start:end])
        for key in ("HOME", "PATH", "RUNNER_TOOL_CACHE", "RUNNER_TOOLSDIRECTORY", "AGENT_TOOLSDIRECTORY",
                    "agent.ToolsDirectory", "GITHUB_ENV", "GITHUB_PATH", "GITHUB_OUTPUT", "GITHUB_STATE"):
            assert forwarded[key] == environment[key], key
        assert not any("fixture-secret" in value for value in arguments)
        assert forwarded["INPUT_GO-VERSION"] == "1.27.1" and forwarded["INPUT_CACHE"] == "false"
        assert forwarded["INPUT_CHECK-LATEST"] == "false" and forwarded["INPUT_ARCHITECTURE"] == "x64"
        child = arguments[arguments.index("-c") + 1]
        child_argv = ["child", *arguments[arguments.index("-c") + 2:]]
        with mock.patch.dict(os.environ, forwarded, clear=True), mock.patch.object(sys, "argv", child_argv), \
             mock.patch.object(os, "readlink", side_effect=lambda value: {"net": "parent-net" if mode == "same-network" else "child-net",
                               "pid": "parent-pid", "mnt": "parent-mnt"}[value.rsplit("/", 1)[1]]), \
             mock.patch.object(resource, "setrlimit") as limit, mock.patch.object(os, "execvpe") as execute:
            try:
                exec(compile(child, "<namespace-child>", "exec"), {})
            except (AssertionError, OSError, subprocess.SubprocessError, ValueError, KeyError, TypeError):
                execute.assert_not_called()
                limit.assert_not_called()
                return types.SimpleNamespace(returncode=1)
            assert len(ip_calls) == 2
            limit.assert_called_once_with(resource.RLIMIT_FSIZE, (65536, 65536))
            execute.assert_called_once_with("node", ["node", str(action / "dist/setup/index.js")], os.environ)
        message = "Found in cache @ " + str(slot) + "\ngo version go1.27.1 linux/amd64\n"
        if mode == "cache-miss":
            message = "no cache\n"
        if mode == "download":
            message += "Attempting to download 1.27.1...\n"
        if mode == "version":
            message = message.replace("go1.27.1", "go1.27.0")
        if mode == "oversize":
            message += "x" * 65536
        options["stdout"].write(message.encode())
        return types.SimpleNamespace(returncode=37 if mode in ("action-exit", "exit-and-cleanup") else 0)
    with contextlib.ExitStack() as stack:
        stack.enter_context(mock.patch.dict(os.environ, environment, clear=True))
        stack.enter_context(mock.patch.object(sys, "platform", "linux"))
        stack.enter_context(mock.patch.object(os, "uname", return_value=types.SimpleNamespace(machine="x86_64")))
        stack.enter_context(mock.patch.object(os, "getuid", return_value=0 if mode == "root" else 1001))
        stack.enter_context(mock.patch.object(os, "getpid", return_value=222))
        stack.enter_context(mock.patch.object(pwd, "getpwuid", return_value=types.SimpleNamespace(pw_name="fixture-user")))
        stack.enter_context(mock.patch.object(pathlib.Path, "lstat", metadata))
        stack.enter_context(mock.patch.object(pathlib.Path, "read_text", read))
        stack.enter_context(mock.patch.object(pathlib.Path, "iterdir", entries))
        stack.enter_context(mock.patch.object(os, "readlink", side_effect=lambda value: "parent-" + value.rsplit("/", 1)[1]))
        stack.enter_context(mock.patch.object(subprocess, "run", run))
        if mode in ("cleanup", "exit-and-cleanup"):
            stack.enter_context(mock.patch.object(tempfile.TemporaryDirectory, "_rmtree", side_effect=OSError("cleanup")))
        code = 0
        try:
            exec(compile(source, "<workflow>", "exec"), {})
        except SystemExit as error:
            code = error.code
    leftovers = len(list(scratch.iterdir()))
    assert leftovers == (1 if mode in ("cleanup", "exit-and-cleanup") else 0), leftovers
    assert mount_reads == [], mount_reads
    print("FIXTURE_RESULT " + json.dumps({"code": code, "calls": len(calls), "leftovers": leftovers}))
`;

for (const [mode, code, calls] of [
  ["success", 0, 1],
  ...["root", "custom-cache", "work-folder", "service", "ancestry", "missing-slot", "writable", "foreign",
    "symlink", "action-hash", "matcher-hash", "custom-url", "custom-url-input", "missing-env"].map((mode) => [mode, 1, 0]),
  ...["same-network", "network-up", "network-route", "cache-miss", "download", "version", "oversize", "cleanup"].map((mode) => [mode, 1, 1]),
  ...["network-links", "network-empty", "network-shape", "network-name", "flags-missing", "flags-string",
    "flags-invalid", "flags-empty", ...["link", "route"].flatMap((kind) =>
      ["missing", "nonzero", "timeout", "json"].map((failure) => `ip-${kind}-${failure}`))].map((mode) => [mode, 1, 1]),
  ["action-exit", 37, 1], ["exit-and-cleanup", 37, 1],
]) {
  test(`registered Runner qualification: ${mode}`, () => {
    const hash = (value) => createHash("sha256").update(value).digest("hex");
    const source = qualificationPython()
      .replace(actionHash, hash("fixture action\n"))
      .replace(matcherHash, hash("fixture matchers\n"));
    const result = spawnSync("python3", ["-I", "-B", "-c", fixture, mode], {
      input: source, encoding: "utf8", timeout: 15000, maxBuffer: 65536,
    });
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stderr || result.stdout);
    const line = result.stdout.split("\n").find((line) => line.startsWith("FIXTURE_RESULT "));
    assert.ok(line, result.stdout);
    const proof = JSON.parse(line.slice("FIXTURE_RESULT ".length));
    assert.equal(proof.code, code, result.stderr || result.stdout);
    assert.equal(proof.calls, calls);
    assert.doesNotMatch(result.stdout + result.stderr, /fixture-secret|fixture-network-private|Found in cache @|go version go/);
  });
}
