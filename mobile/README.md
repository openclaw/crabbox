# Crabbox iOS

A native SwiftUI client for [`crabbox.sh`](https://crabbox.sh) — plus an
on-device and sandbox-backed LLM assistant, and first-class management of
crabbox.sh-provisioned sandboxes. No web wrapper, no Expo, no third-party
runtime: just SwiftUI, WebKit, and (for on-device inference) MLX.

## What it is

Three native tabs over one portable brain (`CrabboxKit`):

- **Portal** — a real `WKWebView` pointed at your Crabbox coordinator
  (`https://crabbox.sh` by default). GitHub OAuth and portal cookies persist in
  the default `WKWebsiteDataStore`, navigation is HTTPS-only and origin-
  whitelisted, and the chrome (host + status pill, back/reload/home, settings
  sheet to switch coordinators) is fully native to satisfy App Store
  Guideline 4.2.
- **Assistant** — a provider-agnostic chat. Pick an engine kind and talk to it:
  - **On-device** (MLX) — runs a small model locally, fully offline.
  - **Sandbox** (Ollama) — talks to a model running on a crabbox.sh- or
    islo.dev-provisioned sandbox via `CrabboxKit.SandboxEngine`.
  - **System** (Apple Foundation Models) — the OS-provided on-device model on
    supported hardware.
- **Sandboxes** — list / create / stop sandboxes through the
  `SandboxProvisioner` abstraction. The crabbox.sh coordinator is the primary
  provider; an optional islo.dev section lets you paste and save (Keychain) a
  direct islo key. Launching an LLM sandbox makes its Ollama endpoint
  immediately selectable as an Assistant engine.

## Architecture

The codebase is split so that all logic is testable on Linux with no Apple SDK:

- **`CrabboxKit`** — the portable brain. Pure Swift, no UIKit/SwiftUI/WebKit:
  coordinator-URL normalization, the navigation/whitelist policy, the
  `reduce()` app state machine, and the LLM/sandbox clients
  (`OllamaClient`, `SandboxEngine`, `IsloClient`, `CoordinatorClient`,
  `SandboxProvisioner`). Compiles and is unit-tested on macOS **and** Linux.
- **SwiftUI app** — the iOS target. Thin views that import `CrabboxKit`, wrap
  `WKWebView` via `UIViewRepresentable` + `Coordinator`, and drive an
  `ObservableObject` around `reduce()` — mapping `WKNavigationDelegate`/KVO
  callbacks to `AppAction`s and rendering `AppState`. The views never
  reimplement URL/nav/state logic.
- **`crabbox-sim`** — a headless end-to-end runner that drives the *exact*
  `reduce()` the app uses through 18 checks / 17 scenarios, asserting 13
  safety/UI invariants after every step. Optionally driven by a tiny local LLM
  (`--agent`). This is what runs on a sandbox in CI/e2e.
- **`crabbox-mac`** — a tiny macOS preview harness (real `WKWebView` + native
  chrome) for exercising the portal on a Mac that only has the Command Line
  Tools. A developer convenience; the shippable artifact is the iOS app.

See [`docs/architecture.md`](docs/architecture.md) for the full picture.

## Build

The portable targets need only a Swift toolchain (macOS or Linux):

```sh
swift build            # build CrabboxKit + crabbox-sim (+ crabbox-mac on macOS)
swift test             # run the CrabboxKit unit suite
swift run crabbox-sim  # run the headless e2e (deterministic, no network)
swift run crabbox-mac  # macOS-only: real WKWebView preview of the portal
```

The iOS app target is generated with [XcodeGen](https://github.com/yonaskolb/XcodeGen)
and built in Xcode (the iOS SDK is required):

```sh
xcodegen generate      # produce Crabbox.xcodeproj
open Crabbox.xcodeproj  # build & run the Crabbox scheme in Xcode
```

> Do not expect `swift build` to compile the iOS app target — the SwiftUI/WebKit
> views need Xcode and the iOS SDK. Linux/CLI builds cover `CrabboxKit`,
> `crabbox-sim`, and the tests.

## Run on a device

- **Try it today (free):** with a free Apple ID you can use *free provisioning*
  — open `Crabbox.xcodeproj` in Xcode, pick your iPhone, set a unique bundle id
  under Signing & Capabilities, and run. The app installs for ~7 days before the
  provisioning profile expires and must be re-signed.
- **Real distribution (paid):** TestFlight and the App Store require a paid
  Apple Developer Program account. With one, archive the app and upload to
  App Store Connect for TestFlight builds and review.

Full details, including the PWA add-to-home-screen alternative that needs no
Apple account at all, are in [`docs/distribution.md`](docs/distribution.md).

## LLM engines

The Assistant is engine-agnostic (`protocol LLMEngine`). Three engine kinds ship:

- **On-device (MLX)** — local inference, offline, no key.
- **Sandbox (Ollama)** — `SandboxEngine` over an Ollama endpoint running on a
  sandbox you launched from the Sandboxes tab.
- **System (Apple Foundation Models)** — the OS model on supported devices.

## Sandboxes & the islo.dev provider

Sandboxes are managed through the `SandboxProvisioner` protocol:

- **crabbox.sh coordinator (primary)** — `CoordinatorProvisioner`. The
  coordinator brokers sandbox lifecycle for you.
- **islo.dev (optional, direct)** — `IsloProvisioner`. islo is brokerless by
  Crabbox design, so the app talks to islo.dev directly with a key you save in
  the Keychain. Use this if you want to bring your own islo account.

`launchLLMSandbox(provisioner:name:model:)` boots a sandbox and returns a ready
`SandboxEngine`, which the app then offers as a selectable Assistant engine.

## License

See [`LICENSE`](LICENSE).
