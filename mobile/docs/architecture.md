# Architecture

Crabbox iOS is deliberately split into three layers so that every piece of
logic that *can* be tested without an Apple SDK *is* — on Linux, in CI, and on a
sandbox — while the parts that genuinely need UIKit/WebKit stay thin and
declarative.

```
                ┌──────────────────────────────────────────┐
                │            SwiftUI app (iOS)               │
                │  Portal · Assistant · Sandboxes tabs       │
                │  WKWebView (UIViewRepresentable)           │
                │  ObservableObject around reduce()          │
                └──────────────────┬───────────────────────┘
                                   │ import (no logic reimplemented)
                ┌──────────────────▼───────────────────────┐
                │               CrabboxKit                  │
                │  pure Swift · no UIKit/SwiftUI/WebKit      │
                │  URL policy · nav guards · reduce()        │
                │  LLM + sandbox clients                     │
                └───────┬───────────────────────┬───────────┘
              imports   │                       │   imports
        ┌───────────────▼──────┐        ┌───────▼──────────────┐
        │     crabbox-sim      │        │     crabbox-mac      │
        │  headless e2e        │        │  macOS WKWebView     │
        │  (+ tiny-LLM agent)  │        │  preview harness     │
        └──────────────────────┘        └──────────────────────┘
```

## CrabboxKit — the portable brain

`CrabboxKit` is pure Swift with **no** UIKit, SwiftUI, or WebKit imports. It
compiles and is unit-tested on macOS *and* Linux, which is what lets the whole
state machine and security policy be verified without an iOS device. It owns:

- **Coordinator-URL policy** — `normalizeCoordinatorURL`, `hostLabel`,
  `webViewOriginWhitelist`. HTTPS is required in production; loopback HTTP
  (`localhost`/`127.0.0.1`/`[::1]`) is accepted only when `allowLocalHTTP` is
  set, and LAN HTTP is always rejected.
- **Navigation guards** — `shouldOpenExternally`, `isWithinWhitelist`,
  `isAllowedNavigation`, `shouldStartLoadInWebView`. These decide, for any URL,
  whether the WebView loads it, blocks it, or hands it to the system
  (`mailto:`, `tel:`, `itms-apps:`, …).
- **The app state machine** — `AppState`, `AppAction`, `AppEffect`, and the pure
  `reduce(state, action, env) -> ReduceResult`. All Portal behavior (boot,
  switch coordinator, reload, back, home, load/error transitions, progress) is a
  function of state + action, so it is fully testable without a UI.
- **LLM + sandbox clients** — `LLMEngine`, `OllamaClient`, `SandboxEngine`,
  `IsloClient`, `CoordinatorClient`, and the `SandboxProvisioner` protocol with
  its two implementations.

## The SwiftUI app

The iOS target is intentionally thin. Views import `CrabboxKit` and **never**
reimplement URL, navigation, or state logic. The Portal in particular:

- Wraps `WKWebView` via `UIViewRepresentable` + a `Coordinator`.
- Drives an `ObservableObject` around `reduce()`: `WKNavigationDelegate` and KVO
  callbacks (`estimatedProgress`, `title`, `canGoBack`) are translated into
  `AppAction`s; the view renders the resulting `AppState`.
- Uses `config.websiteDataStore = .default()` so GitHub OAuth and portal cookies
  persist, `allowsBackForwardNavigationGestures = true`, a `UIRefreshControl`
  for pull-to-refresh, and a `createWebViewWith` that returns `nil` so OAuth
  popups load in place.
- Routes `decidePolicyFor` through `shouldOpenExternally` + `isAllowedNavigation`
  (HTTPS-only + whitelist), opening external schemes via `UIApplication.open`.

Because the policy and state live in `CrabboxKit`, the app cannot drift from the
behavior that `crabbox-sim` proves.

## crabbox-sim — headless e2e

`crabbox-sim` is a headless end-to-end runner built on the same `reduce()` the
app uses. It models WebView load effects deterministically (no real I/O) and
checks the security/UI invariants after every dispatched action. It can also be
driven by a tiny local LLM (`--agent`) that explores the state space while the
invariants act as the judge. See [`e2e.md`](e2e.md).

## crabbox-mac — preview harness

A small macOS app (real `WKWebView` + native chrome) that reuses the exact
`CrabboxKit` navigation policy, for exercising the portal on a Mac with only the
Command Line Tools installed. It is a developer convenience; the shippable
artifact is the iOS app target.

## The SandboxProvisioner abstraction

Sandbox lifecycle is hidden behind one protocol so the Sandboxes tab and the
Assistant don't care where a sandbox comes from:

```swift
protocol SandboxProvisioner: Sendable {
    var providerName: String { get }
    func launch(name: String, model: String) async throws -> SandboxHandle
    func list() async throws -> [SandboxHandle]
    func stop(id: String) async throws
}
```

Two providers ship:

- **`CoordinatorProvisioner` (crabbox.sh — primary).** The crabbox.sh
  coordinator is the manager: it brokers sandbox creation, listing, and
  teardown, and returns an Ollama endpoint for LLM sandboxes. This is the
  default path and needs only a coordinator token.
- **`IsloProvisioner` (islo.dev — optional, direct).** islo is **brokerless by
  Crabbox design** — there is no coordinator in front of it — so the app talks
  to `https://api.islo.dev` directly using a key the user pastes and saves in
  the Keychain. This is the "bring your own islo account" escape hatch.

`launchLLMSandbox(provisioner:name:model:)` ties it together: it launches a
sandbox via whichever provisioner is selected, waits for its Ollama endpoint,
and returns a ready `SandboxEngine`.

## Why the chat engine is provider-agnostic

The Assistant talks only to `protocol LLMEngine` (`displayName`, `kind`,
`isReady()`, `reply(messages:options:)`). Concrete engines —
on-device MLX, `SandboxEngine` over Ollama, and Apple Foundation Models —
are interchangeable behind it. That means:

- A sandbox launched from the Sandboxes tab becomes a selectable engine with no
  Assistant-side changes.
- On-device and system engines work with no network and no provider at all.
- The same chat UI, message model (`ChatMessage`), and options (`LLMOptions`)
  drive every engine, so `crabbox-sim --chat` can smoke-test the *real*
  `SandboxEngine` path the app ships.
