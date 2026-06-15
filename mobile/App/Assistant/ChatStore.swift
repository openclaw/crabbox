//
//  ChatStore.swift
//  Crabbox
//
//  View model for the Assistant tab. Holds the conversation, the set of
//  available `LLMEngine`s, and the currently-selected engine, and drives a
//  `send(_:)` flow that calls into whichever engine is active.
//
//  Engines come from two places:
//    • Locally-authored engines (on-device MLX + Apple Foundation Models) are
//      created right here in `makeLocalEngines()`.
//    • Sandbox/Ollama engines are produced by the Sandboxes tab and pushed in
//      via `addSandboxEngine(_:)`. The Sandboxes tab holds a reference to this
//      same `ChatStore` (it's a shared environment object) so a freshly
//      launched sandbox becomes selectable here without any extra plumbing.
//

import Foundation
import SwiftUI
import CrabboxKit

@MainActor
final class ChatStore: ObservableObject {

    // MARK: Published state

    /// The visible conversation. System turns may be present but are not
    /// rendered as bubbles by the view.
    @Published var messages: [ChatMessage] = []

    /// All engines the user can choose between, in display order.
    @Published var engines: [any LLMEngine] = []

    /// The active engine. Changing it does not clear history.
    @Published var selected: (any LLMEngine)?

    /// `true` while a reply is in flight (drives the "thinking" indicator and
    /// disables the composer).
    @Published var busy: Bool = false

    /// Readiness of the selected engine, refreshed when the selection changes.
    /// `nil` means "not yet probed".
    @Published var selectedReady: Bool?

    /// Last error surfaced to the user, shown inline beneath the conversation.
    @Published var lastError: String?

    // MARK: Configuration

    /// System prompt prepended (invisibly) to every conversation.
    private let systemPrompt = ChatMessage(
        role: .system,
        content: "You are Crab, the helpful assistant inside the Crabbox iOS app. Be concise and friendly."
    )

    // MARK: Init

    init(engines: [any LLMEngine]? = nil) {
        let initial = engines ?? ChatStore.makeLocalEngines()
        self.engines = initial
        self.selected = initial.first
        Task { await refreshReadiness() }
    }

    /// Builds the engines authored in this app run. The sandbox engine is added
    /// later by the Sandboxes tab, so it is intentionally absent here.
    static func makeLocalEngines() -> [any LLMEngine] {
        [
            MLXEngine(),               // on-device, Metal
            FoundationModelsEngine(),  // system, Apple Intelligence (iOS 26+)
        ]
    }

    // MARK: Engine management

    /// Selects an engine and re-probes its readiness.
    func select(_ engine: any LLMEngine) {
        selected = engine
        Task { await refreshReadiness() }
    }

    /// Adds (or refreshes) a sandbox-backed engine and selects it. Called by the
    /// Sandboxes tab after a successful `launchLLMSandbox(...)`.
    func addSandboxEngine(_ engine: any LLMEngine) {
        // De-dupe by display name so re-launching the same sandbox replaces the
        // stale entry rather than stacking duplicates.
        engines.removeAll { $0.displayName == engine.displayName }
        engines.append(engine)
        select(engine)
    }

    /// Replaces the set of sandbox-kind engines with `sandbox` (from the shared
    /// `EngineHub`), preserving the always-present on-device/system engines, and
    /// selects a newly-added sandbox engine. Called by RootView via the hub.
    func syncSandboxEngines(_ sandbox: [any LLMEngine]) {
        let previousSandboxNames = Set(engines.filter { $0.kind == .sandbox }.map(\.displayName))
        let locals = engines.filter { $0.kind != .sandbox }
        engines = locals + sandbox
        if let newest = sandbox.first(where: { !previousSandboxNames.contains($0.displayName) }) {
            select(newest)
        } else if selected == nil {
            selected = engines.first
            Task { await refreshReadiness() }
        }
    }

    /// Probes the selected engine's `isReady()` and publishes the result.
    func refreshReadiness() async {
        guard let selected else { selectedReady = nil; return }
        let ready = await selected.isReady()
        // Guard against a race where selection changed mid-probe.
        if selected.displayName == self.selected?.displayName {
            selectedReady = ready
        }
    }

    // MARK: Sending

    /// Appends the user's message and asks the selected engine for a reply.
    /// Errors are caught and surfaced inline; they do not crash the flow.
    func send(_ text: String) async {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, !busy else { return }
        guard let engine = selected else {
            lastError = "No assistant engine is available."
            return
        }

        lastError = nil
        let userMessage = ChatMessage(role: .user, content: trimmed)
        messages.append(userMessage)

        busy = true
        defer { busy = false }

        // Assemble the full prompt: system prompt + visible conversation.
        let payload = [systemPrompt] + messages

        do {
            let reply = try await engine.reply(messages: payload, options: .init())
            let clean = reply.trimmingCharacters(in: .whitespacesAndNewlines)
            messages.append(ChatMessage(role: .assistant, content: clean))
        } catch let error as LLMError {
            lastError = ChatStore.describe(error)
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Clears the visible conversation (history-only; engines untouched).
    func clear() {
        messages.removeAll()
        lastError = nil
    }

    // MARK: Helpers

    private static func describe(_ error: LLMError) -> String {
        switch error {
        case .unavailable(let why): return why
        case .http(let code):       return "Request failed (HTTP \(code))."
        case .decode(let detail):   return "Couldn't read the response: \(detail)"
        case .invalidResponse:      return "The engine returned an invalid response."
        }
    }
}
