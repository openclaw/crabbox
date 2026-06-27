//
//  CrabboxApp.swift
//  Crabbox
//
//  App entry point. Wires the native tabs (Run / Sandboxes / Assistant / Portal) and the
//  shared environment: AppSettings (prefs + provider credentials), EngineHub
//  (runtime sandbox engines), and a single ChatStore (the Assistant conversation).
//
//  Cross-tab bridge: when the Sandboxes tab mints a SandboxEngine it registers it
//  in the EngineHub; RootView observes the hub and syncs those engines into the
//  ChatStore so they appear in the Assistant's engine picker immediately.
//

import SwiftUI
import Combine
import CrabboxKit

/// The root tabs, used both for the TabView selection and for programmatic jumps
/// (e.g. "Chat with this sandbox" switches to `.assistant`).
enum RootTab: Hashable {
    case run, sandboxes, assistant, portal
}

/// Shared store for active remote sandboxes (islo.dev or crabbox.sh).
/// SandboxesView populates it; Run tab reads it to let the user pick targets
/// and distribute commands across remote Crabbox sandboxes. This is what turns
/// the iOS app into the command center / orchestrator for fleets of islo sandboxes.
@MainActor
final class SandboxStore: ObservableObject {
    @Published private(set) var handles: [SandboxHandle] = []
    @Published private(set) var isRefreshing = false
    @Published private(set) var lastError: String?

    /// Refreshes from the currently configured provisioner (islo or coordinator).
    func refresh(using settings: AppSettings) async {
        guard let provisioner = settings.makeProvisioner() else {
            handles = []
            lastError = nil
            return
        }
        isRefreshing = true
        lastError = nil
        defer { isRefreshing = false }
        do {
            handles = try await provisioner.list()
        } catch {
            handles = []
            lastError = "Couldn't load sandboxes: \(describe(error))"
        }
    }

    func clear() {
        handles = []
        lastError = nil
    }

    private func describe(_ error: Error) -> String {
        if let llm = error as? LLMError { return llm.description }
        return (error as NSError).localizedDescription
    }
}

@main
struct CrabboxApp: App {
    var body: some Scene {
        WindowGroup {
            RootView()
                .preferredColorScheme(.dark)
        }
    }
}

struct RootView: View {
    @StateObject private var settings = AppSettings()
    @StateObject private var engineHub = EngineHub()
    @StateObject private var sandboxStore = SandboxStore()
    @StateObject private var chat = ChatStore()
    @State private var selectedTab: RootTab = RootView.initialTab()

    /// DEBUG-only: lets `SIMCTL_CHILD_CRABBOX_TAB=portal` (etc.) open the app on a
    /// specific tab — used to capture per-tab screenshots without UI automation.
    static func initialTab() -> RootTab {
        #if DEBUG
        switch ProcessInfo.processInfo.environment["CRABBOX_TAB"] {
        case "sandboxes": return .sandboxes
        case "assistant": return .assistant
        case "portal": return .portal
        default: return .run
        }
        #else
        return .run
        #endif
    }

    var body: some View {
        TabView(selection: $selectedTab) {
            CommandRunnerView()
                .tag(RootTab.run)
                .tabItem { Label("Run", systemImage: "terminal") }

            SandboxesView(selectedTab: $selectedTab)
                .tag(RootTab.sandboxes)
                .tabItem { Label("Sandboxes", systemImage: "shippingbox") }

            AssistantView(store: chat)
                .tag(RootTab.assistant)
                .tabItem { Label("Assistant", systemImage: "bubble.left.and.text.bubble.right") }

            PortalView()
                .tag(RootTab.portal)
                .tabItem { Label("Portal", systemImage: "globe") }
        }
        .tint(Theme.accent)
        .environmentObject(settings)
        .environmentObject(engineHub)
        .environmentObject(sandboxStore)
        // Sandbox engines minted in the Sandboxes tab flow into the Assistant.
        .onReceive(engineHub.$sandboxEngines) { engines in
            chat.syncSandboxEngines(engines)
        }
    }
}
