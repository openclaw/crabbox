#!/usr/bin/env python3
"""Bounded, run-scoped unsigned UTM build and QEMU guest proof. Python 3.12+."""

import argparse
import getpass
import hashlib
import json
import os
import plistlib
import re
import selectors
import shlex
import shutil
import signal
import struct
import subprocess
import sys
import tarfile
import time
import uuid
from pathlib import Path

LIMIT = 1024 * 1024


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(LIMIT), b""):
            h.update(chunk)
    return h.hexdigest()


def save(path, value):
    Path(path).write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def extract_sysroot(archive, destination):
    # The data filter rejects devices and escaping links as well as ../ paths.
    with tarfile.open(archive, "r:gz") as tar:
        tar.extractall(destination, filter="data")


def signal_owned_group(pgid, signum, env):
    try:
        os.killpg(pgid, signum)
    except ProcessLookupError:
        pass
    except PermissionError:
        # macOS can return EPERM after the last member exits. Confirm absence
        # rather than hiding a real failure to stop a live owned process.
        observed = subprocess.run(
            ["/bin/ps", "-axo", "pgid=,stat="],
            env=env,
            capture_output=True,
            text=True,
            check=True,
            timeout=5,
        )
        live = [
            line
            for line in observed.stdout.splitlines()
            if len(line.split()) == 2
            and line.split()[0] == str(pgid)
            and not line.split()[1].startswith("Z")
        ]
        require(
            not live, "Cannot stop owned process group; inspect this run before cleanup"
        )


class Run:
    def __init__(self, output, lane):
        self.root = Path(output).absolute()
        require(
            not self.root.exists(),
            "Output already exists; choose a NEW run directory (stale proof rejected)",
        )
        self.root.mkdir(parents=True, mode=0o700)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir(mode=0o700)
        self.work = self.root / "work"
        self.work.mkdir(mode=0o700)
        self.env = {
            k: os.environ[k]
            for k in ("PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "DEVELOPER_DIR")
            if k in os.environ
        }
        self.account_name = getpass.getuser()
        self.log_bytes = 0
        self.result = {
            "schema": "crabbox-utm-workload/v1",
            "runId": str(uuid.uuid4()),
            "lane": lane,
            "status": "incomplete",
            "commands": [],
            "build": "untested",
            "package": "untested",
            "runtime": "untested",
            "graphics": "untested",
            "appleVirtualization": "untested",
        }
        self.flush()

    def flush(self):
        save(self.evidence / "result.json", self.result)

    def scrub(self, data):
        for original, replacement in (
            (str(self.root), "<run>"),
            (str(Path.home()), "<home>"),
            (self.account_name, "<user>"),
        ):
            if original:
                data = data.replace(original.encode(), replacement.encode())
        return data

    def command(self, name, argv, cwd=None, timeout=60, check=True):
        # Never dump the environment or unbounded native build output.
        command = [str(x) for x in argv]
        start = time.monotonic()
        proc = subprocess.Popen(
            command,
            cwd=cwd,
            env=self.env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        tail = b""
        kept = 0
        timed_out = False
        completed = False
        selector = selectors.DefaultSelector()
        selector.register(proc.stdout, selectors.EVENT_READ)
        try:
            with (self.evidence / (name + ".log")).open("wb") as log:
                while selector.get_map():
                    if time.monotonic() - start > timeout:
                        timed_out = True
                        raise TimeoutError(name + " exceeded its deadline")
                    for key, _ in selector.select(0.2):
                        chunk = os.read(key.fileobj.fileno(), 16384)
                        if not chunk:
                            selector.unregister(key.fileobj)
                            continue
                        tail = (tail + chunk)[-65536:]
                        # Logs are private, bounded, and scrub runner-specific paths.
                        chunk = self.scrub(chunk)
                        remaining = min(LIMIT - kept, 16 * LIMIT - self.log_bytes)
                        if remaining > 0:
                            log.write(chunk[:remaining])
                            kept += min(len(chunk), remaining)
                            self.log_bytes += min(len(chunk), remaining)
            try:
                proc.wait(timeout=max(0.01, timeout - (time.monotonic() - start)))
            except subprocess.TimeoutExpired as exc:
                timed_out = True
                raise TimeoutError(name + " exceeded its deadline") from exc
            completed = True
        finally:
            try:
                if not completed:
                    # The wrapper can exit while children retain its output pipe.
                    signal_owned_group(proc.pid, signal.SIGTERM, self.env)
                    try:
                        proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        pass
                    signal_owned_group(proc.pid, signal.SIGKILL, self.env)
                    proc.wait(timeout=5)
            finally:
                selector.close()
                proc.stdout.close()
                clean_tail = self.scrub(tail)
                (self.evidence / (name + ".tail.log")).write_bytes(clean_tail)
                self.result["commands"].append(
                    {
                        "name": name,
                        "exitCode": proc.returncode,
                        "timedOut": timed_out,
                        "logCapped": kept == LIMIT,
                    }
                )
                self.flush()
        output = tail.decode("utf-8", errors="replace")
        if check:
            require(
                proc.returncode == 0,
                name + " failed; inspect evidence/" + name + ".log",
            )
        return proc.returncode, output

    def platform(self):
        require(
            sys.platform == "darwin", "This workload requires a prepared macOS runner"
        )
        self.command("macos", ["/usr/bin/sw_vers"])
        _, arch = self.command("architecture", ["/usr/bin/uname", "-m"])
        self.result["hostArchitecture"] = arch.strip()
        self.flush()


def validate_submodules(output):
    # Git emits an empty result for a release with no submodules; otherwise its
    # prefix distinguishes matching (space), uninitialized (-), changed (+), conflict (U).
    require(
        all(line.startswith(" ") for line in output.splitlines()),
        "Initialize matching recursive submodules before building",
    )


def build(run, args):
    run.platform()
    require(
        not re.search(r"\s", str(run.root)),
        "Choose an output path without whitespace: upstream build script uses unquoted archive discovery",
    )
    source = Path(args.source).resolve(strict=True)
    developer = Path(args.developer_dir).resolve(strict=True)
    require(
        (developer / "usr/bin/xcodebuild").is_file(),
        "Select FULL Xcode Contents/Developer; Command Line Tools are insufficient",
    )
    run.env["DEVELOPER_DIR"] = str(developer)
    run.command("developer-selection", ["/usr/bin/xcode-select", "-p"])
    run.command("xcode", ["/usr/bin/xcodebuild", "-version"])
    run.command("sdk", ["/usr/bin/xcrun", "--sdk", "macosx", "--show-sdk-version"])
    # Recent Xcode releases may install the Metal compiler as a separate component.
    # Probe its execution, not just the wrapper path, before the expensive build.
    code, _ = run.command(
        "metal",
        ["/usr/bin/xcrun", "--sdk", "macosx", "metal", "--version"],
        check=False,
    )
    require(
        code == 0,
        "Metal compiler unavailable: prepare this Xcode with xcodebuild -downloadComponent MetalToolchain, then rerun in a new output directory",
    )
    require(
        re.fullmatch(r"[0-9a-f]{40}", args.revision),
        "Supply the full expected source commit SHA",
    )
    _, revision = run.command(
        "source-revision", ["git", "-C", source, "rev-parse", "HEAD"]
    )
    require(revision.strip() == args.revision, "Source HEAD does not match --revision")
    _, status = run.command(
        "source-status",
        ["git", "-C", source, "status", "--porcelain", "--untracked-files=all"],
    )
    require(
        not status.strip(),
        "Source must be clean, including untracked files; prepare dependencies separately",
    )
    _, modules = run.command(
        "source-submodules", ["git", "-C", source, "submodule", "status", "--recursive"]
    )
    validate_submodules(modules)
    manifest = json.loads(Path(args.dependencies).read_text())
    require(
        manifest["sourceRevision"] == args.revision,
        "Dependency manifest source revision mismatch",
    )
    require(
        manifest["artifactName"]
        in ("Sysroot-macos-" + args.arch, "Sysroot-macos-universal"),
        "Dependency artifact architecture mismatch",
    )
    require(
        re.fullmatch(
            r"https://github.com/utmapp/UTM/actions/runs/[0-9]+",
            manifest["actionsRunURL"],
        ),
        "Use a credential-free UTM Actions run URL for dependency provenance",
    )
    archive = Path(manifest["archive"]).resolve(strict=True)
    require(digest(archive) == manifest["sha256"], "Dependency archive SHA256 mismatch")
    run.result["sourceRevision"] = args.revision
    run.result["dependencies"] = {
        k: manifest[k]
        for k in ("sourceRevision", "artifactName", "actionsRunURL", "sha256")
    }
    run.result["architecture"] = args.arch
    run.result["developerDirectory"] = str(developer).replace(
        str(Path.home()), "<home>"
    )
    run.result["signing"] = "unsigned-development (ad-hoc)"
    checkout = run.work / "source"
    run.command(
        "source-copy",
        ["git", "clone", "--no-hardlinks", "--recursive", source, checkout],
        timeout=1800,
    )
    _, copied = run.command(
        "copied-revision", ["git", "-C", checkout, "rev-parse", "HEAD"]
    )
    require(
        copied.strip() == args.revision,
        "Source changed while cloning; retry with frozen inputs",
    )
    deps = run.work / "dependencies"
    deps.mkdir()
    extract_sysroot(archive, deps)
    # Xcode uses PLATFORM_DISPLAY_NAME (macOS), including this capitalization.
    roots = list(deps.glob("sysroot-*"))
    wanted = "sysroot-macOS-" + args.arch
    available = next((p for p in roots if p.name.lower() == wanted.lower()), None)
    if available is None:
        available = next(
            (p for p in roots if p.name.lower() == "sysroot-macos-arm64_x86_64"), None
        )
    require(
        available is not None,
        "Artifact lacks the selected macOS sysroot; extract sysroot.tgz from its GitHub artifact ZIP first",
    )
    require(
        (available / "lib").is_dir() and (available / "include").is_dir(),
        "Incomplete sysroot: lib/ and include/ are required",
    )
    require(
        any((available / "lib").iterdir()),
        "Empty sysroot lib/: download matching native dependencies",
    )
    require(
        (available / "Frameworks").is_dir()
        and any((available / "Frameworks").glob("*.framework")),
        "Sysroot lacks packaged native Frameworks/; download the complete matching artifact",
    )
    shutil.copytree(available, checkout / wanted, symlinks=True)
    run.result["sysroot"] = wanted
    run.flush()
    output = run.work / "UTM"
    run.command(
        "build",
        [
            "/bin/bash",
            "scripts/build_utm.sh",
            "-k",
            "macosx",
            "-s",
            "macOS",
            "-a",
            args.arch,
            "-o",
            output,
        ],
        cwd=checkout,
        timeout=args.timeout,
    )
    archive_app = Path(str(output) + ".xcarchive") / "Products/Applications/UTM.app"
    require(
        (archive_app / "Contents/MacOS/UTM").is_file(),
        "Build returned success without a fresh UTM.app archive",
    )
    run.result["build"] = "passed"
    run.flush()
    packages = run.work / "package"
    packages.mkdir()
    run.command(
        "package",
        [
            "/bin/bash",
            "scripts/package_mac.sh",
            "unsigned",
            str(output) + ".xcarchive",
            packages,
        ],
        cwd=checkout,
        timeout=1800,
    )
    dmg = packages / "UTM.dmg"
    require(
        dmg.is_file() and dmg.stat().st_size > 0,
        "Packaging returned success without fresh UTM.dmg",
    )
    mount = run.work / "mount"
    mount.mkdir()
    attached = False
    try:
        run.command(
            "dmg-attach",
            ["hdiutil", "attach", "-readonly", "-nobrowse", "-mountpoint", mount, dmg],
            timeout=120,
        )
        attached = True
        apps = list(mount.glob("*.app"))
        require(
            len(apps) == 1 and apps[0].name == "UTM.app",
            "Expected one UTM.app in packaged DMG",
        )
        app = run.work / "UTM.app"
        run.command("app-copy", ["ditto", apps[0], app], timeout=300)
    finally:
        if attached:
            run.command("dmg-detach", ["hdiutil", "detach", mount], timeout=120)
    with (app / "Contents/Info.plist").open("rb") as f:
        info = plistlib.load(f)
    run.result["app"] = {
        key: info.get(key)
        for key in (
            "CFBundleIdentifier",
            "CFBundleShortVersionString",
            "CFBundleVersion",
        )
    }
    run.result["appExecutableSHA256"] = digest(app / "Contents/MacOS/UTM")
    run.result["dmgSHA256"] = digest(dmg)
    run.command("codesign", ["codesign", "--verify", "--deep", "--strict", app])
    run.result["package"] = "passed"


def validate_vm(config, backend):
    require(
        config.get("Backend") == "QEMU",
        "Automated guest-agent proof supports QEMU only; Apple virtualization needs a separate guest SSH/serial proof",
    )
    qemu = config["QEMU"]
    require(
        qemu.get("Hypervisor") is (backend == "qemu-hvf"),
        "VM Hypervisor setting differs from declared backend",
    )
    require(
        not qemu.get("AdditionalArguments"),
        "Remove additional QEMU arguments: they can override backend or access external resources",
    )
    require(
        config.get("Sharing", {}).get("DirectoryShareMode", "None").lower() == "none",
        "Disable shared directories for task-owned proof",
    )
    for drive in config.get("Drive", []):
        image = drive.get("ImageName", "")
        require(
            image and Path(image).name == image and image not in (".", ".."),
            "All VM drives must be internal bundle images; detach external/removable drives",
        )
    require(config.get("Drive"), "Guest fixture has no internal disk")


def observed_backend(text):
    # UTMProcess.arguments renders a flat command line, quoting arguments that
    # contain spaces. Inspect one complete fresh launch record, not other log text.
    launches = [
        line
        for line in text.splitlines(keepends=True)
        if line.startswith("Launching: qemu-system-")
    ]
    require(
        len(launches) == 1 and launches[0].endswith("\n"),
        "Expected one complete fresh QEMU launch record; inspect evidence/backend.log",
    )
    try:
        arguments = shlex.split(launches[0][len("Launching: ") :])
    except ValueError as exc:
        raise RuntimeError(
            "Cannot parse fresh UTM launch arguments; inspect evidence/backend.log"
        ) from exc
    positions = [i for i, value in enumerate(arguments) if value == "-accel"]
    require(
        len(positions) == 1 and positions[0] + 1 < len(arguments),
        "Fresh launch must declare exactly one explicit accelerator; inspect evidence/backend.log",
    )
    accelerator = arguments[positions[0] + 1].split(",", 1)[0]
    require(
        accelerator in ("hvf", "tcg"),
        "Fresh launch uses an unsupported or ambiguous accelerator; inspect evidence/backend.log",
    )
    return "qemu-" + accelerator


def capture_backend(run, vm):
    logs = list(vm.rglob("debug.log"))
    if len(logs) != 1:
        return ""
    with logs[0].open("rb") as stream:
        text = stream.read(LIMIT).decode("utf-8", errors="replace")
    # UTM follows launch arguments with an environment dump. Do not collect it.
    launch = text.split("Launching: qemu-system-", 1)
    selected = ""
    if len(launch) == 2:
        selected = (
            "Launching: qemu-system-" + launch[1].split("Environment variables:", 1)[0]
        )
    errors = [
        line
        for line in text.splitlines()
        if re.search(r"(?i)error|failed|denied|unsupported", line) and "=" not in line
    ]
    selected += "\n" + "\n".join(errors[-30:])
    selected = run.scrub(selected.encode()).decode()
    selected = re.sub(r"(?i)(password=)[^,\s]+", r"\1<redacted>", selected)
    # Keep the first pre-validation snapshot even when cleanup adds later output.
    destination = run.evidence / (
        "backend-final.log"
        if (run.evidence / "backend.log").exists()
        else "backend.log"
    )
    destination.write_bytes(selected.encode()[:LIMIT])
    return text


def prepare_controller(run, built, prior):
    source = built / "work/source"
    protocol = source / "Scripting/UTMScripting.swift"
    bridge = Path(__file__).with_name("UTMProofController.swift").resolve(strict=True)
    require(
        protocol.is_file(),
        "Keep the successful build's source copy: its pinned UTM scripting protocol is required",
    )
    _, revision = run.command(
        "controller-source-revision", ["git", "-C", source, "rev-parse", "HEAD"]
    )
    require(
        revision.strip() == prior["sourceRevision"],
        "Controller protocol source differs from built source revision",
    )
    run.command(
        "controller-source-clean",
        [
            "git",
            "-C",
            source,
            "diff",
            "--exit-code",
            "HEAD",
            "--",
            "Scripting/UTMScripting.swift",
        ],
    )
    developer = prior.get("developerDirectory", "").replace("<home>", str(Path.home()))
    require(
        developer and Path(developer).is_dir(),
        "The build's full Xcode directory is required to compile the PID-bound controller",
    )
    run.env["DEVELOPER_DIR"] = developer
    controller = run.work / "utm-proof-controller"
    run.command(
        "controller-build",
        [
            "/usr/bin/xcrun",
            "swiftc",
            "-parse-as-library",
            "-module-cache-path",
            run.work / "swift-modules",
            "-framework",
            "AppKit",
            "-framework",
            "ScriptingBridge",
            protocol,
            bridge,
            "-o",
            controller,
        ],
        timeout=300,
    )
    require(controller.is_file(), "Swift compiler returned no proof controller")
    run.result["controller"] = {
        "transport": "ScriptingBridge exact PID",
        "sourceSHA256": digest(bridge),
        "protocolSHA256": digest(protocol),
        "executableSHA256": digest(controller),
    }
    return controller


def discover_controller(run, controller, app):
    for attempt in range(20):
        code, output = run.command(
            "controller-discover-" + str(attempt),
            [controller, app, "discover"],
            timeout=5,
            check=False,
        )
        if code == 0:
            observed = json.loads(output)
            require(
                type(observed.get("pid")) is int
                and observed["pid"] > 0
                and type(observed.get("launchTime")) in (int, float),
                "Controller returned an invalid process identity",
            )
            require(
                Path(observed.get("executable", "")).resolve()
                == (app / "Contents/MacOS/UTM").resolve(),
                "Controller observed an executable outside the packaged built app",
            )
            run.result["controller"].update(observed)
            run.flush()
            return [controller, app, str(observed["pid"]), str(observed["launchTime"])]
        time.sleep(1)
    raise RuntimeError(
        "Cannot bind exactly one built UTM process. Inspect controller-discover logs for wrong app paths or competing copies; no VM command was sent"
    )


def boot(run, args):
    run.platform()
    built = Path(args.build).resolve(strict=True)
    prior = json.loads((built / "evidence/result.json").read_text())
    require(
        prior["build"] == "passed" and prior["package"] == "passed",
        "Boot requires a successful build AND unsigned package result",
    )
    app = built / "work/UTM.app"
    require(
        digest(app / "Contents/MacOS/UTM") == prior["appExecutableSHA256"],
        "Packaged app changed since build",
    )
    run.result["builtRunId"] = prior["runId"]
    run.result["app"] = prior["app"]
    run.result["backend"] = args.backend
    run.result["runnerKind"] = args.runner_kind
    require(
        args.runner_kind == "physical"
        or args.backend == "qemu-tcg"
        or args.allow_nested,
        "Nested HVF is unproven: use a physical Mac baseline or explicitly --allow-nested for this exact configuration",
    )
    template = Path(args.vm_template).resolve(strict=True)
    require(template.suffix == ".utm", "Supply a dedicated stopped .utm guest fixture")
    require(
        not any(p.is_symlink() for p in template.rglob("*")),
        "VM fixture contains symlinks; use self-contained internal disks",
    )
    with (template / "config.plist").open("rb") as f:
        config = plistlib.load(f)
    validate_vm(config, args.backend)
    if args.backend == "qemu-hvf":
        expected = {"arm64": "aarch64", "x86_64": "x86_64"}.get(
            run.result["hostArchitecture"]
        )
        require(
            config["System"]["Architecture"] == expected,
            "HVF requires native guest architecture; use TCG for cross-architecture emulation",
        )
    images = {}
    for drive in config["Drive"]:
        image = template / "Data" / drive["ImageName"]
        require(
            image.is_file(), "Guest internal image is missing: " + drive["ImageName"]
        )
        # A QCOW2 backing path or external data file would reach outside our copy.
        with image.open("rb") as stream:
            header = stream.read(104)
        if header[:4] == b"QFI\xfb":
            require(len(header) >= 72, "Truncated QCOW2 image")
            version, offset, size = struct.unpack(">IQI", header[4:20])
            require(
                offset == 0 and size == 0,
                "Flatten QCOW2 backing chains before using this fixture",
            )
            if version >= 3:
                require(
                    len(header) >= 104
                    and not (struct.unpack(">Q", header[72:80])[0] & 4),
                    "QCOW2 external data files are unsupported",
                )
        images[drive["ImageName"]] = digest(image)
    run.result["guestDiskSHA256"] = images
    run.result["templateConfigSHA256"] = digest(template / "config.plist")
    run.result["guestImageProvenance"] = args.guest_image
    # Never address a personal VM by its UUID. Each run gets an independent bundle.
    vm = run.work / "proof.utm"
    shutil.copytree(template, vm)
    ident = str(uuid.uuid4()).upper()
    config["Information"]["UUID"] = ident
    config["Information"]["Name"] = "Crabbox proof " + run.result["runId"]
    config["QEMU"]["DebugLog"] = True
    with (vm / "config.plist").open("wb") as f:
        plistlib.dump(config, f)
    controller = prepare_controller(run, built, prior)
    code, _ = run.command("existing-app", ["pgrep", "-x", "UTM"], check=False)
    require(
        code == 1,
        "Quit all existing UTM instances before this isolated workload; installed apps and personal VMs are preserved",
    )
    # Drop old launch output from the copied fixture before this boot.
    for old_log in vm.rglob("debug.log"):
        old_log.unlink()
    run.result["taskVMUUID"] = ident
    run.flush()
    registered = False
    start_attempted = False
    try:
        run.command("app-launch", ["open", "-n", "-a", app])
        ctl = discover_controller(run, controller, app)
        run.command("controller-identity", [*ctl, "identity"], timeout=5)
        run.command("fixture-open", [*ctl, "open", vm])
        # The exact UUID must become visible; zero scripting exit alone is insufficient.
        for attempt in range(20):
            _, state = run.command(
                "fixture-status-" + str(attempt),
                [*ctl, "status", ident],
                timeout=5,
                check=False,
            )
            if state.strip() == "stopped":
                registered = True
                break
            time.sleep(1)
        require(
            registered,
            "Task guest did not register as stopped in the PID-bound built app. Check the actual proof controller's macOS Automation permission and logged-in GUI context (-1743)",
        )
        start_attempted = True
        run.command("guest-start", [*ctl, "start", ident], timeout=60)
        nonce = run.result["runId"]
        expected = "CRABBOX_GUEST_READY:" + nonce
        deadline = time.monotonic() + args.timeout
        attempt = 0
        while time.monotonic() < deadline and attempt < 150:
            # qemu-guest-agent executes in THIS VM, avoiding a stale host/forwarded SSH endpoint.
            code, text = run.command(
                "guest-challenge-" + str(attempt),
                [
                    *ctl,
                    "exec",
                    ident,
                    "--cmd",
                    "/bin/sh",
                    "-c",
                    "printf '%s\n' '"
                    + expected
                    + "'; uname -s; cat /proc/sys/kernel/random/boot_id",
                ],
                timeout=min(30, max(1, deadline - time.monotonic())),
                check=False,
            )
            lines = text.strip().splitlines()
            if (
                code == 0
                and len(lines) == 3
                and lines[0] == expected
                and lines[1] == "Linux"
                and re.fullmatch(r"[0-9a-f-]{36}", lines[2])
            ):
                run.result["guestSignal"] = {
                    "nonce": nonce,
                    "os": lines[1],
                    "bootId": lines[2],
                }
                backend_log = capture_backend(run, vm)
                actual_backend = observed_backend(backend_log)
                run.result["observedBackend"] = actual_backend
                run.flush()
                require(
                    actual_backend == args.backend,
                    "Observed launch accelerator differs from declared backend; inspect evidence/backend.log",
                )
                run.command("controller-final-identity", [*ctl, "identity"], timeout=5)
                run.result["runtime"] = "passed"
                break
            attempt += 1
            time.sleep(2)
        require(
            run.result["runtime"] == "passed",
            "Guest never answered fresh readiness challenge; check guest-agent installation/channel, backend support, nesting and guest boot logs",
        )
    finally:
        capture_backend(run, vm)
        if registered:
            if start_attempted:
                run.command("guest-stop", [*ctl, "stop", "--force", ident], check=False)
            for attempt in range(15):
                _, state = run.command(
                    "cleanup-status-" + str(attempt),
                    [*ctl, "status", ident],
                    timeout=5,
                    check=False,
                )
                if state.strip() == "stopped":
                    break
                time.sleep(1)
            require(
                state.strip() == "stopped",
                "Cannot confirm task VM stopped; retain work/proof.utm and stop ONLY its recorded UUID manually",
            )
            run.command("guest-delete", [*ctl, "delete", ident])
            for _ in range(20):
                if not vm.exists():
                    break
                time.sleep(0.25)
            require(
                not vm.exists(),
                "UTM did not delete task copy; inspect recorded UUID before removing it",
            )
            run.result["cleanup"] = "task VM stopped and deleted via its fresh UUID"
        else:
            run.result["cleanup"] = (
                "unregistered copy retained; inspect task bundle only"
            )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="lane", required=True)
    b = sub.add_parser("build")
    b.add_argument("--source", required=True)
    b.add_argument("--revision", required=True)
    b.add_argument("--dependencies", required=True)
    b.add_argument("--developer-dir", required=True)
    b.add_argument("--arch", choices=("arm64", "x86_64"), required=True)
    b.add_argument("--timeout", type=int, default=7200)
    r = sub.add_parser("boot")
    r.add_argument("--build", required=True)
    r.add_argument("--vm-template", required=True)
    r.add_argument(
        "--guest-image",
        required=True,
        help="Public image URL/version/SHA256, without credentials",
    )
    r.add_argument("--backend", choices=("qemu-tcg", "qemu-hvf"), required=True)
    r.add_argument("--runner-kind", choices=("physical", "virtual"), required=True)
    r.add_argument("--allow-nested", action="store_true")
    r.add_argument("--timeout", type=int, default=300)
    for command in (b, r):
        command.add_argument("--output", required=True)
    args = parser.parse_args()
    require(
        sys.version_info >= (3, 12),
        "Use Python 3.12+ for safe dependency archive extraction",
    )
    require(args.timeout > 0, "Timeout must be positive")
    run = Run(args.output, args.lane)

    def interrupted(_signum, _frame):
        raise KeyboardInterrupt()

    signal.signal(signal.SIGTERM, interrupted)
    try:
        (build if args.lane == "build" else boot)(run, args)
        run.result["status"] = "passed"
    except (Exception, KeyboardInterrupt) as exc:  # noqa: BLE001 - publish a failed manifest for every workload error
        run.result["status"] = "failed"
        run.result["error"] = run.scrub(
            (
                str(exc) or "Interrupted; inspect cleanup result before rerunning"
            ).encode()
        ).decode()
        print(run.result["error"], file=sys.stderr)
        return 1
    finally:
        run.flush()
    print(str(run.evidence / "result.json"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
