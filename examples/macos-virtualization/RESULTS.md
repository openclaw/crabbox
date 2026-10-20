# UTM workload acceptance record

The maintained recipe was exercised against UTM **5.0.8 (128)**, source
`535d6592a8bdc22b48de53703a371b80bf60d587`, on a physical Apple Silicon Mac
running macOS **27.0.1 (26A434)**. Build/package execution and guest runtime have
separate receipts and contexts.

| Property | Observed result |
| --- | --- |
| Crabbox route | Static SSH macOS/ARM64, controlled by a separate Mac running Crabbox 0.78.1 |
| Build | Passed with a new private source copy and output directory |
| Unsigned package | Passed upstream unsigned development packaging and deep signature verification |
| Artifact collection | Required fresh manifest downloaded through Crabbox; bytes matched the runner manifest |
| Physical QEMU HVF runtime | Passed in the runner's approved GUI context using the separately built app identified below |
| QEMU TCG emulation | Untested |
| Apple virtualization workload | Untested by this recipe; provider macOS provisioning has a separate acceptance contract |
| Nested virtualization | Untested |
| Graphics/renderer workload | Untested; guest boot does not establish graphics behavior |

## Reproducible inputs

The full developer selection was `/Applications/Xcode.app/Contents/Developer`,
Xcode **27.1 (27A9275)** with macOS SDK **27.0** and the official Metal toolchain
component **27A266a**. The recipe's Metal execution probe passed before building.
The selected application architecture was `arm64`. Recursive submodule state
was recorded against the pinned release, and the input source remained clean.

The native dependency artifact was `Sysroot-macos-arm64` from the successful
[release workflow run](https://github.com/utmapp/UTM/actions/runs/38996133972)
for that exact source revision. Its extracted `sysroot.tgz` SHA256 was
`789dcab397acadcc82eee54dbcc4fb279c7a8bf00e819cacfe61515cf54b6f26`.
Packaging mode was `unsigned-development (ad-hoc)`, with app identifier
`com.utmapp.UTM`, version `5.0.8` and build `128`. This does not establish USB
passthrough, bridged networking or Developer ID signing.

The disposable runtime fixture used the official Ubuntu 24.04 ARM64 cloud image,
SHA256 `50e24e1fb79e89116e939d90aa7738f7af48c442fa1bc2510131e0b39c50348e`,
with a prepared working guest agent. Its internal disk and config hashes were
captured before copying. Each boot used a new VM UUID and a new guest nonce.

## Build and runtime receipts

The Static SSH build/package workload run was
`e128c0af-d814-4cc1-a8f7-540362f3812e`, carried by Crabbox run
`run_bf2e7db1d583cf68d5ccd118ab04aa5f`. Its app executable SHA256 was
`26773fbf25c8a208f8e95abf66bbd2572f6620796639e301f2f3b04d70ca4675`
and its DMG SHA256 was
`9ecaa5de2f8c1a0697f97488ee27159716c39b073f4f6a28fd612cf3f475c43f`.
The required artifact archive was 48,253 bytes, SHA256
`158597a74e794dcded44e6a0c4e451d696c56b28d34701a2a308315c723835e0`.
The downloaded manifest exactly matched the fresh runner manifest.

Runtime was proved separately on the same physical runner in the approved GUI
context. Boot run `6f0cec0e-d6ac-4c91-b47d-1dc197e66868` used the packaged app
from earlier successful build/package run
`49e3f6e9-eee3-4d7b-9a11-1c672aa70c24`; that app executable SHA256 was
`d7f2064148098f6444447363340ca7e2a80435d34c57e9d1f6df07a8d8db3fa6`.
The helper checked those unchanged bytes before launch, pinned the running
executable path, PID and launch time, and used only PID-bound AppleEvents.
The guest returned the exact fresh nonce, Linux identity and a boot ID. A fresh
launch record contained explicit `-accel hvf`, matching the declared backend.
Final process identity verification passed. The copied VM was observed stopped
and deleted through its exact UUID; the source fixture stayed unchanged.
This is a successful GUI runtime proof, not a claim that the SSH context was
authorized to automate UTM.

Earlier failed attempts remain failures in private evidence: a missing Metal
component blocked compilation, macOS app/component approvals blocked initial
runtime setup, a strict guest-result decoder expected a Cocoa key instead of
the scripting `exited` key, and a launch matcher expected the wrong log format.
The maintained helper and its regression checks now use the observed scripting
and launch formats. Fresh subsequent receipts established the passes above.

Run the commands in [the recipe](README.md), retain each new bounded `evidence/`
directory and Crabbox artifact receipt privately, and compare their identities.
Public results omit host/account identities, private paths, guest credentials
and personal VM configuration. CI runs deterministic Python checks on Linux and
macOS and the pure Swift identity/result tests on macOS; CI does not boot UTM or
grant Automation permissions.
