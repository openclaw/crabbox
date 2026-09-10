set -euo pipefail
: "${expected_node_major?Mint smoke renderer must set expected_node_major}"
[[ "$(id -u)" -ne 0 ]] || { echo 'developer image smoke requires a nonroot user' >&2; exit 1; }
uname -a
command -v git
command -v gh
command -v jq
command -v rg
command -v fd
command -v python3
command -v node
command -v npm
command -v corepack
command -v pnpm
command -v trufflehog
trufflehog --no-update --version
command -v docker
node --version
node -e 'const major = process.versions.node.split(".")[0]; const expected = process.argv[1]; if (expected ? major !== expected : Number(major) < 24) throw new Error("Node.js " + (expected ? "major " + expected : "24 or newer") + " is required, found " + process.version)' -- "$expected_node_major"
corepack --version
if [[ -n "${expected_pnpm_version:-}" ]]; then
  (
    cd /
    version="$(COREPACK_ENABLE_NETWORK=0 pnpm --version)"
    [[ "$version" == "$expected_pnpm_version" ]] || {
      printf 'pnpm default mismatch: expected %s, found %s\n' "$expected_pnpm_version" "$version" >&2
      exit 1
    }
    corepack_version="$(COREPACK_ENABLE_NETWORK=0 corepack pnpm --version)"
    [[ "$version" == "$corepack_version" ]] || { echo 'ordinary pnpm does not match Corepack' >&2; exit 1; }
    printf '%s\n' "$version"
  )
else
  pnpm --version
fi

smoke_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-smoke.XXXXXXXX")"
cache_probe=""
browser_pid=""
cleanup_smoke() {
  local status=$? line
  if [[ -n "$browser_pid" ]]; then
    kill "$browser_pid" 2>/dev/null || true
    wait "$browser_pid" 2>/dev/null || true
  fi
  if [[ "$status" != 0 && -f "$smoke_dir/visible-browser.log" ]]; then
    while IFS= read -r line; do
      case "$line" in
        browser-smoke-evidence\ *) printf '%s\n' "$line" >&2; break ;;
      esac
    done <"$smoke_dir/visible-browser.log"
  fi
  rm -f -- "${cache_probe:-}"
  rm -rf -- "$smoke_dir"
}
trap cleanup_smoke EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Direct Python invocation makes $! the cleanup owner, not a shell intermediary.
# Keep one browser invocation and the existing 30s limit with bounded evidence.
browser_probe="$(cat <<'PY'
import json
import hashlib
import itertools
import os
import pathlib
import re
import selectors
import shutil
import signal
import subprocess
import sys
import time

started = time.monotonic()
interrupted = 0
child = None
status = 1
streams = {"stdout": bytearray(), "stderr": bytearray()}
sizes = {"stdout": 0, "stderr": 0}
process_samples = []
sampler = None
sample_timeout = 0.25
sample_bytes = 16384
identity = {}
def interrupt(signum, _frame):
    global interrupted
    interrupted = 128 + signum
for signum in (signal.SIGINT, signal.SIGTERM):
    signal.signal(signum, interrupt)
def signal_group(signum):
    try:
        os.killpg(child.pid, signum)
    except ProcessLookupError:
        pass
def read_proc(entry, name, limit=4096):
    with (entry / name).open("rb") as source:
        data = source.read(limit + 1)
    if len(data) > limit:
        raise ValueError("proc field exceeds limit")
    return data.decode("ascii")

def proc_stat(entry):
    prefix, fields = read_proc(entry, "stat").rsplit(") ", 1)
    if prefix.split(" ", 1)[0] != entry.name:
        raise ValueError("proc pid changed")
    fields = fields.split()
    if not re.fullmatch(r"[A-Za-z]", fields[0]):
        raise ValueError("invalid proc state")
    row = {"pid": int(entry.name), "uid": entry.stat().st_uid,
           "ppid": int(fields[1]), "pgid": int(fields[2]), "sid": int(fields[3]),
           "state": fields[0], "minorFaults": int(fields[7]), "majorFaults": int(fields[9]),
           "startTicks": int(fields[19]), "userTicks": int(fields[11]),
           "systemTicks": int(fields[12]), "rssBytes": int(fields[21]) * os.sysconf("SC_PAGE_SIZE")}
    if any(not 0 <= value < 2 ** 64 for value in row.values() if isinstance(value, int)):
        raise ValueError("invalid proc counter")
    return row

def sample(proc_root, group_id, uid):
    result = []
    # Only the sampler reads proc: even a small io read can wait on a kernel lock.
    for entry in itertools.islice(proc_root.iterdir(), 8192):
        if not entry.name.isdecimal():
            continue
        try:
            row = proc_stat(entry)
            if (row["pgid"], row["sid"], row["uid"]) != (group_id, group_id, uid):
                continue
            try:
                io = dict(line.split(": ", 1) for line in read_proc(entry, "io").splitlines())
                values = {key: int(io[key]) for key in ("read_bytes", "write_bytes", "rchar", "wchar")}
                if all(0 <= value < 2 ** 64 for value in values.values()):
                    row["io"] = values
            except (OSError, ValueError, KeyError):
                pass
            row["wchan"] = None
            try:
                wchan = read_proc(entry, "wchan", 65).strip()
                if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_.]{0,63}", wchan):
                    row["wchan"] = wchan
            except (OSError, ValueError):
                pass
            after = proc_stat(entry)
            if any(after[key] != row[key] for key in ("pid", "uid", "pgid", "sid", "startTicks")):
                continue
            result.append(row)
            if len(result) == 8:
                break
        except (OSError, ValueError, IndexError, UnicodeError):
            continue
    return result

def start_sample(now):
    global sampler
    record = {"attemptElapsedMs": round((now - started) * 1000), "attemptDurationMs": 0,
              "outcome": "error", "processes": [], "truncatedRows": 0, "samplerReaped": False}
    process_samples.append(record)
    if sampler is not None:
        record["reason"] = "previousSamplerUnreaped"
        return
    reader = writer = None
    try:
        reader, writer = os.pipe()
        os.set_blocking(reader, False)
        os.set_blocking(writer, False)
        pid = os.fork()
    except OSError:
        for fd in (reader, writer):
            if fd is not None:
                os.close(fd)
        record["reason"] = "start"
        return
    if pid == 0:
        try:
            for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT, signal.SIGPIPE):
                signal.signal(signum, signal.SIG_DFL)
            signal.pthread_sigmask(signal.SIG_SETMASK, [])
            os.close(reader)
            # A stuck sampler must not retain the outer command's output pipes.
            with open(os.devnull, "rb+") as null:
                for fd in (0, 1, 2):
                    os.dup2(null.fileno(), fd)
            for stream in (child.stdout, child.stderr):
                stream.close()
            data = json.dumps(sample(pathlib.Path("/proc"), child.pid, os.getuid())).encode("utf-8")
            if len(data) > sample_bytes or os.write(writer, data) != len(data):
                os._exit(1)
            os._exit(0)
        except BaseException:
            os._exit(1)
    os.close(writer)
    record["samplerPID"] = pid
    sampler = {"pid": pid, "fd": reader, "record": record, "started": now,
               "data": bytearray(), "finished": False}

def poll_sample(now, cancel=None):
    global sampler
    if sampler is None:
        return
    current = sampler
    record = current["record"]
    try:
        pid, result = os.waitpid(current["pid"], os.WNOHANG)
    except ChildProcessError:
        record["outcome"], record["reason"] = "error", "notChild"
        if not current["finished"]:
            os.close(current["fd"])
        sampler = None
        return
    if pid:
        record["samplerReaped"] = True
    if not current["finished"]:
        # Preserve an already completed worker before cancelling a live collector.
        reason = None if pid else cancel or ("deadline" if now - current["started"] >= sample_timeout else None)
        if reason is None:
            try:
                current["data"].extend(os.read(current["fd"], sample_bytes + 1 - len(current["data"])))
                if len(current["data"]) > sample_bytes:
                    reason = "ipcLimit"
            except BlockingIOError:
                pass
            except OSError:
                reason = "ipcRead"
        if reason is not None or pid:
            record["attemptDurationMs"] = round((now - current["started"]) * 1000)
            record["outcome"] = "timeout" if reason == "deadline" else "error"
            if reason is None and result == 0:
                try:
                    rows = json.loads(current["data"])
                    if not isinstance(rows, list) or len(rows) > 8 or not all(isinstance(row, dict) for row in rows):
                        raise ValueError("invalid sample")
                    record["processes"] = rows
                    record["outcome"] = "ok" if rows else "empty"
                except (ValueError, UnicodeError):
                    reason = "ipcData"
            else:
                reason = reason or "collector"
            if reason:
                record["reason"] = reason
            if not pid:
                # This unreaped direct child cannot have had its PID reused.
                try:
                    os.kill(current["pid"], signal.SIGKILL)
                except ProcessLookupError:
                    pass
            os.close(current["fd"])
            current["finished"] = True
    if pid:
        sampler = None

def evidence_line(evidence):
    evidence["truncatedStderrBytes"] = 0
    while True:
        line = ("browser-smoke-evidence " + json.dumps(evidence, ensure_ascii=False, sort_keys=True) + "\n").encode("utf-8")
        if len(line) < 8192:
            return line
        if evidence["stderr"]:
            before = evidence["stderr"].encode("utf-8")
            evidence["stderr"] = before[:max(0, len(before) - (len(line) - 8191))].decode("utf-8", errors="ignore")
            evidence["truncatedStderrBytes"] += len(before) - len(evidence["stderr"].encode("utf-8"))
            continue
        available = [record for record in evidence["processSamples"] if record["processes"]]
        if not available:
            raise ValueError("evidence metadata exceeds limit")
        record = max(available, key=lambda item: len(item["processes"]))
        record["processes"].pop()
        record["truncatedRows"] += 1
try:
    try:
        wrapper = pathlib.Path(shutil.which("crabbox-browser"))
        if wrapper.stat().st_size <= 1048576:
            identity["wrapperSHA256"] = hashlib.sha256(wrapper.read_bytes()).hexdigest()
        packages = subprocess.run(["dpkg-query", "-W", "-f=${Package}=${Version}\n",
                                   "google-chrome-stable", "chromium"],
                                  capture_output=True, text=True, timeout=2)
        identity["packages"] = [line for line in packages.stdout.splitlines()[:2]
                                if len(line) <= 160 and all(c.isalnum() or c in "=-_.+:~" for c in line)]
    except (OSError, TypeError, subprocess.TimeoutExpired):
        pass
    started = time.monotonic()
    child = subprocess.Popen(["timeout", "--kill-after=5", "30", "crabbox-browser", *sys.argv[1:]],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    with selectors.DefaultSelector() as ready:
        for name in streams:
            pipe = getattr(child, name)
            os.set_blocking(pipe.fileno(), False)
            ready.register(pipe, selectors.EVENT_READ, name)
        samples = iter((started + 1, started + 25))
        next_sample = next(samples)
        child_done = None
        while child.poll() is None or ready.get_map() or sampler is not None:
            now = time.monotonic()
            poll_sample(now, "browserComplete" if child.returncode is not None else None)
            if interrupted or now - started >= 36:
                status = interrupted or (child.returncode if child.returncode is not None else 137)
                if status < 0:
                    status = 128 - status
                break
            if child.returncode is None and now >= next_sample:
                start_sample(now)
                next_sample = next(samples, float("inf"))
                while next_sample <= now:
                    next_sample = next(samples, float("inf"))
            for key, _ in ready.select(0.1):
                data = os.read(key.fd, 65536)
                if not data:
                    ready.unregister(key.fileobj)
                    continue
                name = key.data
                sizes[name] += len(data)
                streams[name].extend(data[:max(0, 4096 - len(streams[name]))])
            if child.returncode is not None:
                child_done = child_done or now
                if (not ready.get_map() and sampler is None) or now - child_done >= 2:
                    status = child.returncode if child.returncode >= 0 else 128 - child.returncode
                    break
        if not interrupted and child.returncode is not None:
            status = child.returncode if child.returncode >= 0 else 128 - child.returncode
finally:
    poll_sample(time.monotonic(), "interrupted" if interrupted else "browserComplete")
    if child is not None:
        # timeout owns its browser; this outer owner also reaps on interruption
        # or inherited pipes held open by surviving descendants.
        signal_group(signal.SIGTERM)
        try:
            child.wait(timeout=2)
        except subprocess.TimeoutExpired:
            signal_group(signal.SIGKILL)
            child.wait(timeout=2)
        signal_group(signal.SIGKILL)
        for name in streams:
            getattr(child, name).close()
    # Reap without a blocking wait, using only the sampler's original deadline.
    while sampler is not None and time.monotonic() - sampler["started"] < sample_timeout:
        poll_sample(time.monotonic(), "browserComplete")
        if sampler is not None:
            time.sleep(0.005)
    poll_sample(time.monotonic(), "browserComplete")
    status = interrupted or status
    try:
        text = streams["stderr"].decode("utf-8", errors="replace")
        # Keep unknown Chromium errors useful without exposing private
        # paths, endpoints, credential-shaped values, or command dumps.
        text = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]|[\x00-\x08\x0b-\x1f\x7f]", "", text)
        text = re.sub(r"(?im)^.*(?:command line|environment):.*$", "[command/environment redacted]", text)
        text = re.sub(r"(?im)\b(token|password|secret|authorization|cookie|credential|api[_-]?key)\b\s*[:=][^\n]*", r"\1=[redacted]", text)
        text = re.sub(r"\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s<>]+|(?:/[^/\s]+)+", "[path/url redacted]", text)
        text = re.sub(r"\b[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}\b|\b(?:\d{1,3}\.){3}\d{1,3}\b|(?:[0-9a-fA-F]{0,4}:){2,}[0-9a-fA-F:]+", "[address redacted]", text)
        text = re.sub(r"\b(?:gh[pousr]_[A-Za-z0-9_]+|sk-[A-Za-z0-9_-]+|[A-Za-z0-9_-]{64,})\b", "[opaque value redacted]", text)
        fixture = b"<p>crabbox-browser-smoke</p>"
        dom = next((fixture[:length].decode() for length in range(len(fixture), 0, -1)
                    if fixture[:length] in streams["stdout"]), "")
        # A successful process can still fail the calling DOM/render assertion.
        line = evidence_line({
            "commandExit": status, "elapsedMs": round((time.monotonic() - started) * 1000),
            "stdoutBytes": sizes["stdout"], "stderrBytes": sizes["stderr"],
            "dom": dom, "stderr": text[:4096], "processSamples": process_samples,
            "clockTicksPerSecond": os.sysconf("SC_CLK_TCK"), "identity": identity,
        })
        sys.stderr.buffer.write(line)
    except Exception:
        pass
    if not status:
        sys.stdout.buffer.write(streams["stdout"])
sys.exit(status)
PY
)"

printf 'int main(void) { return 0; }\n' >"$smoke_dir/main.c"
cc "$smoke_dir/main.c" -o "$smoke_dir/compiler"
"$smoke_dir/compiler"
mkdir "$smoke_dir/native" "$smoke_dir/build-home" "$smoke_dir/build-tmp"
cat >"$smoke_dir/native/CMakeLists.txt" <<'CMAKE'
cmake_minimum_required(VERSION 3.16)
project(native_smoke LANGUAGES CXX)
find_package(PkgConfig REQUIRED)
pkg_check_modules(NATIVE REQUIRED IMPORTED_TARGET
  gtk+-3.0 webkit2gtk-4.1 ayatana-appindicator3-0.1 librsvg-2.0 openssl)
find_library(XDO_LIBRARY NAMES xdo REQUIRED)
add_executable(native-smoke main.cpp)
target_compile_features(native-smoke PRIVATE cxx_std_17)
target_link_libraries(native-smoke PRIVATE PkgConfig::NATIVE ${XDO_LIBRARY})
CMAKE
cat >"$smoke_dir/native/main.cpp" <<'CPP'
#include <iostream>
#include <gtk/gtk.h>
#include <webkit2/webkit2.h>
#include <libayatana-appindicator/app-indicator.h>
#include <librsvg/rsvg.h>
#include <openssl/ssl.h>
// Older distro libxdo headers omit their C++ linkage guard.
extern "C" {
#include <xdo.h>
}
int main() {
  if (gtk_get_major_version() < 3 || webkit_get_major_version() < 2 ||
      app_indicator_get_type() == 0 || rsvg_handle_get_type() == 0 ||
      OPENSSL_init_ssl(0, nullptr) != 1 || xdo_get_symbol_map() == nullptr) return 1;
  std::cout << "native-build-ok\n";
}
CPP
# Use only task-owned build/cache state; no project dependencies or ambient credentials.
build_env=(env -i "PATH=$PATH" "HOME=$smoke_dir/build-home" "TMPDIR=$smoke_dir/build-tmp"
  "XDG_CACHE_HOME=$smoke_dir/build-home/cache")
timeout 60 "${build_env[@]}" cmake -S "$smoke_dir/native" -B "$smoke_dir/native/build" -G Ninja
timeout 60 "${build_env[@]}" cmake --build "$smoke_dir/native/build" --parallel 2
native_result="$(timeout 10 "${build_env[@]}" "$smoke_dir/native/build/native-smoke")" || exit $?
[[ "$native_result" == native-build-ok ]] || { echo 'native build execution failed' >&2; exit 1; }
for tool in autoconf automake gawk nasm yasm batcat direnv zoxide sqlite3; do
  command -v "$tool"
done
python3 -I -c 'import sqlite3, ssl; assert sqlite3.connect(":memory:").execute("select 2 + 2").fetchone()[0] == 4; assert ssl.create_default_context().get_ca_certs()'
printf '%s\n' '{"private":true,"scripts":{"check":"node -e \"require('\''node:assert/strict'\'').equal(2 + 2, 4)\""}}' >"$smoke_dir/package.json"
(
  export COREPACK_ENABLE_NETWORK=0 npm_config_offline=true
  cd "$smoke_dir"
  npm --offline run check
  pnpm run check
)
for cache in /var/cache/crabbox/{pnpm,npm,corepack,docker}; do
  cache_probe="$(mktemp "$cache/.devtools-smoke.XXXXXXXX")"
  printf 'runtime-cache-ok\n' >"$cache_probe"
  grep -qx runtime-cache-ok "$cache_probe"
  rm -f -- "$cache_probe"
  cache_probe=""
done

docker_group_member() {
  if id -nG 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
    return 0
  fi
  local current_user docker_entry docker_members member
  current_user="$(whoami)"
  docker_entry="$(getent group docker 2>/dev/null || true)"
  [[ -n "$docker_entry" ]] || return 1
  docker_members="${docker_entry#*:*:*:}"
  local IFS=','
  local -a docker_member_list
  read -ra docker_member_list <<<"$docker_members"
  for member in "${docker_member_list[@]}"; do
    [[ "$member" == "$current_user" ]] && return 0
  done
  return 1
}
docker_probe="$(cat <<'DOCKER'
set -eu
directory="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-docker-smoke.XXXXXXXX")"
project="crabbox-smoke-$(basename "$directory" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9')"
tag="$project:local"
cleanup() {
  status=$?
  trap - EXIT
  if test -f "$directory/compose.yaml"; then
    docker compose -p "$project" -f "$directory/compose.yaml" down --remove-orphans >/dev/null 2>&1 || { test "$status" != 0 || status=1; }
  fi
  docker image rm "$tag" >/dev/null 2>&1 || { test "$status" != 0 || status=1; }
  rm -rf -- "$directory" || { test "$status" != 0 || status=1; }
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker version
docker compose version
docker buildx version
docker image inspect hello-world ubuntu:24.04 node:24-bookworm >/dev/null
docker run --rm --pull=never --network=none hello-world
docker run --rm --pull=never --network=none node:24-bookworm node -e 'require("node:assert/strict").equal(2 + 2, 4)'
printf 'FROM scratch\nCOPY payload /payload\n' >"$directory/Dockerfile"
printf 'offline-build-ok\n' >"$directory/payload"
docker buildx build --builder default --pull=false --network=none --load --tag "$tag" "$directory"
cat >"$directory/compose.yaml" <<'COMPOSE'
services:
  check:
    image: ubuntu:24.04
    pull_policy: never
    network_mode: none
    command: ["sh", "-c", "printf 'offline-compose-ok\\n'"]
COMPOSE
docker compose -p "$project" -f "$directory/compose.yaml" run --rm --no-deps --pull never check
DOCKER
)"
# A newly added group is not visible to the current SSH process; refresh only that membership.
if docker version >/dev/null 2>&1; then
  sh -c "$docker_probe"
elif command -v sg >/dev/null 2>&1 && docker_group_member; then
  sg docker -c "$docker_probe"
else
  echo 'Docker is unavailable to the runtime user; verify the daemon and Docker group membership' >&2
  exit 1
fi

if [[ "${CRABBOX_LINUX_BROWSER:-1}" == 1 ]]; then
  python3 -c "$browser_probe" --headless --disable-gpu --disable-background-networking \
    --disable-component-update --disable-sync --user-data-dir="$smoke_dir/browser" \
    --dump-dom 'data:text/html,<p>crabbox-browser-smoke</p>' >"$smoke_dir/browser.html"
  grep -q '<p>crabbox-browser-smoke</p>' "$smoke_dir/browser.html"
fi
if [[ "${CRABBOX_LINUX_DESKTOP_TOOLS:-1}" == 1 ]]; then
  export DISPLAY="${DISPLAY:-$(sed -nE 's/^DISPLAY=(:[0-9]+)$/\1/p' /var/lib/crabbox/desktop.env)}"
  test -n "$DISPLAY"
  xset q >/dev/null
  xdotool getdisplaygeometry
  scrot "$smoke_dir/desktop.png"
  ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0 "$smoke_dir/desktop.png" |
    grep -Eq '^[1-9][0-9]*,[1-9][0-9]*$'
  if [[ "${CRABBOX_LINUX_BROWSER:-1}" == 1 ]]; then
    browser_title="crabbox-render-$(basename "$smoke_dir" | tr -cd 'a-zA-Z0-9')"
    printf '<title>%s</title><style>html,body{margin:0;height:100%%;background:rgb(25,163,91)}</style>\n' \
      "$browser_title" >"$smoke_dir/render.html"
    # The timeout owns only this isolated browser, including its child processes.
    python3 -c "$browser_probe" --disable-gpu --disable-background-networking \
      --disable-component-update --disable-sync --force-color-profile=srgb \
      --window-size=640,480 --window-position=0,0 --user-data-dir="$smoke_dir/visible-browser" \
      --app="file://$smoke_dir/render.html" \
      >"$smoke_dir/visible-browser.log" 2>&1 &
    browser_pid=$!
    rendered=0
    for attempt in {1..15}; do
      kill -0 "$browser_pid" 2>/dev/null || break
      browser_window="$(xdotool search --onlyvisible --limit 1 --name "$browser_title" 2>/dev/null || true)"
      if [[ "$browser_window" =~ ^[0-9]+$ ]] &&
        timeout 5 ffmpeg -nostdin -v error -y -f x11grab -draw_mouse 0 -window_id "$browser_window" \
          -i "$DISPLAY" -frames:v 1 -vf 'crop=16:16:(iw-16)/2:(ih-16)/2,format=rgb24' \
          -f rawvideo "$smoke_dir/browser.rgb" &&
        node -e 'const fs=require("node:fs"); if (!fs.readFileSync(process.argv[1]).equals(Buffer.alloc(768, Buffer.from([25,163,91])))) process.exit(1)' "$smoke_dir/browser.rgb"; then
        rendered=1
        break
      fi
      sleep 1
    done
    if [[ "$rendered" != 1 ]]; then
      browser_status=1
      if ! kill -0 "$browser_pid" 2>/dev/null; then
        wait "$browser_pid" || browser_status=$?
      fi
      echo 'browser did not render the local fixture on the selected display' >&2
      exit "$browser_status"
    fi
  fi
fi
developer_archive_probe
echo devtools-smoke-ok
