// swift-tools-version:5.9
import PackageDescription

// The macOS preview harness (`crabbox-mac`) is only added when the manifest is
// evaluated on macOS, so Linux CI keeps building just the portable targets.
#if os(macOS)
let macHarness: [Product] = [.executable(name: "crabbox-mac", targets: ["crabbox-mac"])]
let macTargets: [Target] = [.executableTarget(name: "crabbox-mac", dependencies: ["CrabboxKit"])]
#else
let macHarness: [Product] = []
let macTargets: [Target] = []
#endif

// CrabboxKit is the cross-platform brain of the Crabbox iOS app. It contains the
// pure coordinator-URL policy, navigation guards, and the app state machine — no
// UIKit, no SwiftUI — so it compiles on macOS and Linux and is exercised both by
// the SwiftUI app target (in Xcode) and by the headless `crabbox-sim` e2e runner
// (which is what the islo provider runs on a Linux sandbox).
let package = Package(
    name: "CrabboxKit",
    platforms: [.iOS(.v17), .macOS(.v13)],
    products: [
        .library(name: "CrabboxKit", targets: ["CrabboxKit"]),
        .library(name: "CrabboxE2E", targets: ["CrabboxE2E"]),
        .executable(name: "crabbox-sim", targets: ["crabbox-sim"]),
    ] + macHarness,
    targets: [
        // Pure logic shared with the iOS app. Keep this UIKit/SwiftUI-free.
        .target(name: "CrabboxKit"),

        // Headless end-to-end simulator: scenarios, invariants, the fake WebView
        // event model, and the tiny-LLM (Ollama) action driver. Depends only on
        // CrabboxKit so it drives the EXACT logic the app uses.
        .target(name: "CrabboxE2E", dependencies: ["CrabboxKit"]),

        // Thin CLI entry point: `swift run crabbox-sim` (deterministic scenarios)
        // or `swift run crabbox-sim --agent` (tiny-LLM exploration).
        .executableTarget(name: "crabbox-sim", dependencies: ["CrabboxE2E"]),

        .testTarget(name: "CrabboxKitTests", dependencies: ["CrabboxKit", "CrabboxE2E"]),
    ] + macTargets
)
