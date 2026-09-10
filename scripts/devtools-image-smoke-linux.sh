set -euo pipefail
: "${expected_node_major?Mint smoke renderer must set expected_node_major}"
: "${expected_pnpm_version?Mint smoke renderer must set expected_pnpm_version}"
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
processes = []
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
def sample():
    result = []
    # Read numeric kernel metadata only: never cmdline, environ, or user paths.
    for entry in itertools.islice(pathlib.Path("/proc").iterdir(), 8192):
        if not entry.name.isdecimal():
            continue
        try:
            fields = (entry / "stat").read_text().rsplit(") ", 1)[1].split()
            if int(fields[2]) == child.pid and int(fields[3]) == child.pid and entry.stat().st_uid == os.getuid():
                row = {"pid": int(entry.name), "ppid": int(fields[1]), "state": fields[0],
                       "startTicks": int(fields[19]), "userTicks": int(fields[11]),
                       "systemTicks": int(fields[12]), "rssBytes": int(fields[21]) * os.sysconf("SC_PAGE_SIZE")}
                try:
                    io = dict(line.split(": ", 1) for line in (entry / "io").read_text().splitlines())
                    row["io"] = {key: int(io[key]) for key in ("read_bytes", "write_bytes", "rchar", "wchar")}
                except (OSError, ValueError, KeyError):
                    pass
                result.append(row)
                if len(result) == 8:
                    break
        except (OSError, ValueError, IndexError):
            continue
    return result
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
        while child.poll() is None or ready.get_map():
            now = time.monotonic()
            if now >= next_sample:
                try:
                    observed = sample()
                    if observed:
                        processes = observed
                except OSError:
                    pass
                next_sample = next(samples, float("inf"))
            if interrupted or now - started >= 36:
                status = interrupted or (child.returncode if child.returncode is not None else 137)
                if status < 0:
                    status = 128 - status
                break
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
                if not ready.get_map() or now - child_done >= 2:
                    status = child.returncode if child.returncode >= 0 else 128 - child.returncode
                    break
        if not interrupted and child.returncode is not None:
            status = child.returncode if child.returncode >= 0 else 128 - child.returncode
finally:
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
        print("browser-smoke-evidence " + json.dumps({
            "commandExit": status, "elapsedMs": round((time.monotonic() - started) * 1000),
            "stdoutBytes": sizes["stdout"], "stderrBytes": sizes["stderr"],
            "dom": dom, "stderr": text[:4096], "processes": processes, "identity": identity,
        }, sort_keys=True), file=sys.stderr)
    except Exception:
        pass
    if not status:
        sys.stdout.buffer.write(streams["stdout"])
sys.exit(status)
PY
)"

# Check the ordinary runtime-user default without activating a manager, fetching
# packages, or allowing an enclosing project's pin to substitute for the image.
actual_pnpm_version="$(
  cd "$smoke_dir"
  parent="$PWD"
  while :; do
    [[ ! -e "$parent/package.json" && ! -L "$parent/package.json" ]] || {
      echo 'pnpm default verification requires a manifest-free temporary directory' >&2
      exit 1
    }
    [[ "$parent" != / ]] || break
    parent="$(dirname "$parent")"
  done
  COREPACK_ENABLE_NETWORK=0 COREPACK_DEFAULT_TO_LATEST=0 COREPACK_ENABLE_AUTO_PIN=0 \
    COREPACK_ENV_FILE=0 pnpm --version
)" || exit $?
printf '%s\n' "$actual_pnpm_version"
[[ -z "$expected_pnpm_version" || "$actual_pnpm_version" == "$expected_pnpm_version" ]] || {
  echo "runtime pnpm version differs from requested $expected_pnpm_version" >&2
  exit 1
}
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
