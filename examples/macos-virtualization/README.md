# macOS virtualization application workload

This recipe uses existing Crabbox Static SSH macOS execution to build and package
UTM, then proves one declared QEMU backend on a prepared Mac. It adds no provider
or core dependency. The build lane can run on a macOS VM. Use a physical Mac for
the initial hardware virtualization baseline; a nested configuration needs a
separate successful backend boot on that exact host/guest combination.

The maintained example targets [UTM v5.0.8 source](https://github.com/utmapp/UTM/tree/535d6592a8bdc22b48de53703a371b80bf60d587)
and follows its [macOS development guide](https://github.com/utmapp/UTM/blob/535d6592a8bdc22b48de53703a371b80bf60d587/Documentation/MacDevelopment.md).
The helper uses Python 3.12 or later and macOS tools; it does not install software,
accept an Xcode license, change the global developer selection, or install UTM
in `/Applications`. Run native compilation with modest concurrency on a dedicated
runner. Keep source, dependency archives, app bundles and guest disks outside the
Crabbox repository and outside collected evidence.

## Choose the proof

| Lane | Required observation | What it establishes |
| --- | --- | --- |
| Build | Matching source/submodules/sysroot and a new app archive | Compilation for the selected architecture |
| Unsigned package | New DMG, packaged app identity, executable hash and signature verification | UTM development packaging completed |
| QEMU TCG | Fresh QEMU launch arguments without HVF, fresh Linux guest nonce and boot ID | Guest executed under the declared emulation configuration |
| QEMU HVF | Native guest architecture, fresh explicit `-accel hvf` launch arguments, guest nonce and boot ID | This configuration actually booted with hardware virtualization |
| Apple Virtualization | Separate guest SSH or serial challenge through an independently bound guest endpoint | Untested by this helper; UTM's QEMU guest-agent API cannot prove this backend |
| Graphics | Selected renderer/driver evidence and an actual guest graphics workload result | Untested by this helper; boot, screenshots and DMGs do not prove graphics |

Prefer the separate build and guest-agent lanes over a single shell command that
reports success after `open`: the latter proves no guest execution. The guest
agent binds the challenge to the fresh VM UUID without guessing an SSH address.
A small native controller also binds every AppleEvent to the verified built app's
PID, launch time and executable path. It never resolves a bundle identifier or URL
to choose the recipient.
The helper deliberately limits its automated baseline to Linux QEMU guests.
Additional backend/graphics lanes must keep their own result, never inherit a
pass from this one.

## Prepare source and dependencies

Use full Xcode, including its platform SDK, selected only for the workload:

```sh
export DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer
xcodebuild -version
xcrun --sdk macosx --show-sdk-version
xcrun --sdk macosx metal --version
python3 --version
```

Complete first-launch components and any license approval using the runner's
normal administration procedure before starting. Command Line Tools alone are
insufficient. Some Xcode releases install the Metal compiler separately. If the
Metal execution probe reports a missing toolchain, prepare it with
`xcodebuild -downloadComponent MetalToolchain`, then repeat the probe. The helper
checks Metal execution before cloning/building and does not download components.
Missing components and license failures should remain preparation failures, not
be hidden by automatic license acceptance.

Prepare an independent recursive source checkout. Keep it clean: the build helper
clones it into its own run directory so building never changes these inputs.

```sh
git clone --recursive https://github.com/utmapp/UTM.git /tmp/utm-source
git -C /tmp/utm-source checkout --detach 535d6592a8bdc22b48de53703a371b80bf60d587
git -C /tmp/utm-source submodule update --init --recursive
git -C /tmp/utm-source submodule status --recursive
```

Find the successful **release** Build workflow run for that same source revision.
Download its `Sysroot-macos-arm64` or `Sysroot-macos-x86_64` artifact for a local
single-architecture build. A matching `Sysroot-macos-universal` artifact is also
accepted, selecting just the requested application architecture. Do not substitute
the newest unrelated Actions run. The helper does not resolve releases or fetch
artifacts, so its inputs remain explicit and reproducible.

Use operator-managed GitHub authorization for the download; never put tokens in
commands, manifest fields, repository files, shell tracing or artifacts. For
example, with an already authenticated `gh`:

```sh
gh run view 38996133972 --repo utmapp/UTM --json headSha,conclusion,event
gh run download 38996133972 --repo utmapp/UTM \
  --name Sysroot-macos-arm64 --dir /tmp/utm-dependencies
shasum -a 256 /tmp/utm-dependencies/sysroot.tgz
```

Confirm the run's `headSha` equals the pinned source commit and the conclusion is
successful. `gh run download` extracts the artifact ZIP and yields `sysroot.tgz`;
when downloading a ZIP directly, unzip it first. Record both the downloaded ZIP's
published artifact digest, if available, and the local archive hash separately.
The helper validates the **tgz** hash. It safely extracts the tgz into a new staging
directory, checks `lib/`, `include/` and native `Frameworks/`, and installs the
selected `sysroot-macOS-ARCH` directory in the private build copy. A hash verifies
bytes, not provenance: verify the Actions run/revision association before writing
the manifest.

Create `/tmp/utm-dependencies.json` with the observed hash and absolute tgz path:

```json
{
  "sourceRevision": "535d6592a8bdc22b48de53703a371b80bf60d587",
  "artifactName": "Sysroot-macos-arm64",
  "actionsRunURL": "https://github.com/utmapp/UTM/actions/runs/38996133972",
  "archive": "/tmp/utm-dependencies/sysroot.tgz",
  "sha256": "REPLACE_WITH_OBSERVED_TGZ_SHA256"
}
```

Missing or expired artifacts require building the matching native dependencies
using UTM's development guide on a fresh build image. This helper does not silently
fall back to rebuilding or mix dependency versions. Record that alternative's
source, tools and outputs before using it as a different baseline.

## Run the unsigned build and package lane

From this repository, choose a **new**, whitespace-free output directory on the
runner. Upstream archive discovery is not safe with whitespace in its output path.
Existing output directories are rejected, including an earlier successful run.
The independent source clone, recursive submodules, Swift packages and Xcode
build may require public network access. Git credentials remain host-managed.

```sh
python3 examples/macos-virtualization/utm_workload.py build \
  --source /tmp/utm-source \
  --revision 535d6592a8bdc22b48de53703a371b80bf60d587 \
  --dependencies /tmp/utm-dependencies.json \
  --developer-dir /Applications/Xcode.app/Contents/Developer \
  --arch arm64 --output /tmp/utm-build-proof
```

The helper calls upstream `scripts/build_utm.sh -k macosx -s macOS -a ARCH`, then
`scripts/package_mac.sh unsigned`. It mounts the new DMG read-only, copies its
packaged app into `work/UTM.app`, verifies its signature, and detaches the mount.
Build and package outcomes remain distinct in `evidence/result.json`. No paid
Apple signing identity is needed. UTM calls this an unsigned development package;
its executables have ad-hoc signatures and reduced entitlements. USB passthrough
and bridged networking are outside this package's proof contract.

Optional Developer ID packaging requires the paid account, installed certificate,
and separate approved Hypervisor provisioning profiles for UTM, QEMUHelper and
QEMULauncher described upstream. Perform it as a separate private lane, with its
own entitlements and functionality checks. Never collect provisioning profiles,
keychains, signing environment variables or signed download URLs as evidence.
A successful unsigned build cannot establish that a signed feature works.

## Prepare a task guest fixture

Create a dedicated Linux `.utm` fixture using UTM's normal wizard and a publicly
identified guest image. Record its public URL, version and SHA256; verify the image
against its publisher's checksum before importing it. The guest needs a working
`qemu-guest-agent` service and UTM's guest-agent virtio channel. For Debian/Ubuntu,
install `qemu-guest-agent` inside the guest and start its service; confirm it can
answer a guest command, then shut the guest down fully. Use the UTM guest-agent
API only with a trusted disposable guest, since commands run with guest-agent
privileges.

Use these fixture constraints:

- All disks are internal files in the bundle's `Data/` directory. Detach installer
  media after installation. Flatten QCOW2 backing chains, disable external data
  files, and remove symlinks. No shared directories or additional QEMU arguments.
- Select the QEMU backend explicitly. Turn Hypervisor off for `qemu-tcg`, or on
  for `qemu-hvf`. HVF needs the host's native guest architecture (`aarch64` on an
  `arm64` Mac, `x86_64` on an Intel Mac). Never infer HVF from the wizard label.
- Use shared/emulated networking rather than bridging for the unsigned baseline.
  No host port forwarding is necessary for guest-agent proof. Avoid USB devices,
  host directory sharing, saved state, snapshots and personal credentials.
- Shut the fixture down before copying it. Do not use a personal VM as the fixture.
  Keep the fixture and its original disks; each proof creates a fresh UUID and copy.

A cloud image is an alternative to installer media. It normally needs a NoCloud
seed to install the guest agent and shut down on first boot. On macOS, `hdiutil
makehybrid -iso -joliet -default-volume-name cidata -o seed.iso seed-directory`
can create that seed from `user-data` and `meta-data`. Use disposable credentials
and keep those seed files out of evidence. Detach the seed after preparation.
Creating an ISO, creating the VM bundle or reaching cloud-init completion is not
itself the backend proof below.

## Boot and challenge the guest

Perform runtime setup in a logged-in macOS GUI session on the **same runner**.
Keep the successful build's source copy and full Xcode selection: the helper
compiles `UTMProofController.swift` against that exact source's generated
`Scripting/UTMScripting.swift`. It leaves the packaged app unchanged and records
controller source, protocol and executable hashes.

Grant the actual controlling context permission to automate the built UTM app.
A successful terminal permission check does not establish the SSH session's TCC
permission. Error `-1743` requires correcting the approved GUI launch context.
The standalone controller may appear under its responsible application or without
a friendly application name in a macOS prompt. Inspect the actual caller and
recipient; do not infer approval from the packaged app's bundled CLI. Do not
disable TCC or edit its database. Build-only remote execution remains useful when
runtime Automation is unavailable.

Before invoking the helper, inspect the existing UTM inventory. If any VM is
running, preserve that session and use a separate runner/session. An idle installed
UTM can be quit through normal app controls. The helper refuses an already running
UTM process before setup. It launches the **packaged built app** by its path,
reads back the exact running executable path and launch time, and pins its PID.
Every subsequent open, status, start, guest command and cleanup event uses
`SBApplication(processIdentifier:)`. The bundled `utmctl` is deliberately unused:
its URL-based ScriptingBridge lookup can launch another copy with the same bundle
identifier, even when the CLI itself lives inside the built app.

A second UTM copy, process exit, reused PID, changed launch time or changed
executable path prevents a passing proof. Cleanup can still stop/delete the exact
verified built process's task UUID if a competing copy appears, while retaining
all PID/path/launch-time checks. It never sends cleanup to the competing app or
relaunches a vanished process. If identity cannot be established, retain the task
bundle and inspect the recorded process identity before manual recovery.

```sh
python3 examples/macos-virtualization/utm_workload.py boot \
  --build /tmp/utm-build-proof \
  --vm-template /tmp/utm-guest-fixture.utm \
  --guest-image 'PUBLIC_IMAGE_URL version IMAGE_SHA256' \
  --backend qemu-hvf --runner-kind physical \
  --output /tmp/utm-boot-proof
```

Use a new output directory for each backend. On a VM runner, `--runner-kind virtual`
with HVF requires `--allow-nested` to acknowledge a configuration-specific probe;
it does not turn unsupported nesting into support. TCG can run without nested
hardware virtualization, subject to observed host capabilities and performance.

The helper hashes fixture disks, makes an independent copy with a fresh UUID,
removes copied debug logs, starts the VM, and polls its guest agent with a new
nonce. Passing requires exactly the nonce, Linux OS identity and a guest boot ID,
plus fresh UTM launch arguments matching the requested accelerator. The backend
artifact retains launch arguments and excludes UTM's environment dump. It stops and
deletes only that UUID's copied VM after confirming it is stopped. Installed apps,
fixture inputs and the existing VM inventory are preserved. The newly launched
app can remain open after the run; quit it normally when finished.

Failures remain nonzero and `status=failed`, with individual command exits in the
manifest. Missing guest-agent/channel, boot failure, unsupported nesting, absent
Automation permission and timeouts must be investigated from this run's evidence.
SIGINT/SIGTERM trigger task cleanup; power loss or SIGKILL cannot guarantee it.
If stop confirmation fails, retain the run directory, inspect its recorded
`taskVMUUID`, and stop only that VM before deleting its copy. Never use wildcard
VM deletion or kill every UTM/QEMU process. Remove the task's run directories only
after no task VM or DMG mount remains and any desired evidence has been retained.

## Execute through Crabbox and collect evidence

Use a prepared [Static SSH macOS route](../../docs/providers/ssh.md) with host-managed
credentials. Ensure the absolute input directories above exist on that host.
Crabbox syncs this recipe from the repository; it does not provision Xcode,
dependencies or guest fixtures. macOS Actions hydration is unsupported: use direct
preparation commands or a prepared image.

```sh
crabbox run --provider ssh --target macos --arch arm64 \
  --static-host mac.example.com --static-user builder \
  --preflight --preflight-tools default,python3,xcodebuild \
  --artifact-glob 'workload-proof/**' \
  --require-artifact workload-proof/result.json -- \
  /bin/sh -c 'test ! -e workload-proof && python3 examples/macos-virtualization/utm_workload.py build --source /tmp/utm-source --revision 535d6592a8bdc22b48de53703a371b80bf60d587 --dependencies /tmp/utm-dependencies.json --developer-dir /Applications/Xcode.app/Contents/Developer --arch arm64 --output /tmp/utm-build-proof && cp -R /tmp/utm-build-proof/evidence workload-proof'
```

Use unique `/tmp` and workspace output names for each invocation. The existence
guard above refuses retained proof; helper output also refuses reuse. Do not use
Linux-only `--require-artifact-change` on macOS. Static Mac desktop artifacts and
`crabbox screenshot` are unsupported and unnecessary for this procedure.
Collect only `evidence/`, never `work/`: it contains source clones, app/DMG outputs,
guest disks and potentially private guest configuration. Each command log is
capped at 1 MiB, with a separate 64 KiB tail and a 16 MiB aggregate head-log budget; platform/source/dependency identities and outcomes are in the
manifest, and guest/backend evidence is fresh. Review logs before sharing.
Crabbox run artifact download is success-only; retain the host evidence directory
and retrieve its bounded evidence explicitly through the approved SSH transport
after a failure. A failed workload must not be wrapped with unconditional `exit 0`
to make artifacts collect.

If SSH Automation is denied, execute the boot helper in the approved GUI context
on that same runner and retain its separate manifest. Record that limitation;
a remote build pass must not be reported as an end-to-end remote runtime pass.
See [the result record](RESULTS.md) for the current observed acceptance status.

## Check the helper

```sh
python3 -m unittest discover -s examples/macos-virtualization -p 'test_*.py'
```

CI runs this command in both the ordinary `scripts` job on Linux and the existing
`apple-vm` job on macOS, using the latest Python 3.12 patch. Linux discovers all
checks and explicitly skips the native Swift case. On macOS that same case
compiles `UTMProofController.swift` with `-DPROOF_IDENTITY_TESTS` and
`test_identity.swift`, then executes the pure identity and guest-result contract
suite. It needs the macOS SDK through `xcrun swiftc`; it does not need a UTM app,
a guest image or nested virtualization. The native suite runs once through the
unittest command above.

These focused checks exercise stale output rejection, dependency extraction safety,
backend/fixture constraints, fresh challenge validation and bounded subprocess
failure handling. The native case covers PID/path/launch-time identity and typed
scripting-result decoding using supplied records, without inspecting or controlling
live apps. These checks do not compile UTM, grant Automation permissions or boot
a VM. Run the actual build and boot commands to establish those outcomes.
