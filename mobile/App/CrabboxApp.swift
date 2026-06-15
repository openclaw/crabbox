//
//  CrabboxApp.swift
//  Crabbox
//
//  App entry point. Wires the three tabs (Portal / Assistant / Sandboxes) and the
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
    case portal, assistant, sandboxes
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
    @StateObject private var chat = ChatStore()
    @State private var selectedTab: RootTab = .portal

    var body: some View {
        TabView(selection: $selectedTab) {
            PortalView()
                .tag(RootTab.portal)
                .tabItem { Label("Portal", systemImage: "globe") }

            AssistantView(store: chat)
                .tag(RootTab.assistant)
                .tabItem { Label("Assistant", systemImage: "bubble.left.and.text.bubble.right") }

            SandboxesView(selectedTab: $selectedTab)
                .tag(RootTab.sandboxes)
                .tabItem { Label("Sandboxes", systemImage: "shippingbox") }
        }
        .tint(Theme.accent)
        .environmentObject(settings)
        .environmentObject(engineHub)
        // Sandbox engines minted in the Sandboxes tab flow into the Assistant.
        .onReceive(engineHub.$sandboxEngines) { engines in
            chat.syncSandboxEngines(engines)
        }
    }
}
