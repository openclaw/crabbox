//
//  AssistantView.swift
//  Crabbox
//
//  The Assistant tab: an engine-agnostic chat screen. A top picker switches
//  between the available `LLMEngine`s (on-device MLX, on-sandbox Ollama, Apple
//  Intelligence), a scrolling bubble list shows the conversation, and a
//  composer sends messages. A status line reports the selected engine's
//  displayName, kind, and readiness.
//

import SwiftUI
import CrabboxKit

// Design tokens come from the shared `Theme` in Theme.swift (bg / panel /
// accent / subtle / hairline), so the Assistant matches the rest of the app
// and there is no duplicate `Theme` declaration within the module.

struct AssistantView: View {
    @ObservedObject var store: ChatStore
    @State private var draft: String = ""
    @FocusState private var composerFocused: Bool

    var body: some View {
        VStack(spacing: 0) {
            enginePicker
            statusLine
            Divider().overlay(Theme.hairline)
            conversation
            composer
        }
        .background(Theme.bg.ignoresSafeArea())
        .preferredColorScheme(.dark)
    }

    // MARK: Engine picker

    /// A menu of every available engine. We use a Menu rather than a segmented
    /// control because engine count is dynamic (sandboxes are added at runtime)
    /// and names can be long.
    private var enginePicker: some View {
        Menu {
            ForEach(engineRows, id: \.id) { row in
                Button {
                    store.select(row.engine)
                } label: {
                    Label {
                        Text(row.engine.displayName)
                    } icon: {
                        if row.isSelected { Image(systemName: "checkmark") }
                    }
                }
            }
            if !store.messages.isEmpty {
                Divider()
                Button(role: .destructive) { store.clear() } label: {
                    Label("Clear conversation", systemImage: "trash")
                }
            }
        } label: {
            HStack(spacing: 10) {
                Image(systemName: kindSymbol(store.selected?.kind))
                    .foregroundStyle(Theme.accent)
                Text(store.selected?.displayName ?? "No engine")
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(.white)
                    .lineLimit(1)
                Image(systemName: "chevron.up.chevron.down")
                    .font(.caption2)
                    .foregroundStyle(Theme.subtle)
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Theme.panel, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .stroke(Theme.hairline, lineWidth: 1)
            )
        }
        .padding(.horizontal, 16)
        .padding(.top, 12)
    }

    private var engineRows: [EngineRow] {
        store.engines.map {
            EngineRow(engine: $0, isSelected: $0.displayName == store.selected?.displayName)
        }
    }

    // MARK: Status line

    /// Small line: engine kind + readiness dot. Mirrors the Portal's status pill.
    private var statusLine: some View {
        HStack(spacing: 8) {
            Circle()
                .fill(readinessColor)
                .frame(width: 7, height: 7)
            Text(statusText)
                .font(.caption2)
                .foregroundStyle(Theme.subtle)
            Spacer()
        }
        .padding(.horizontal, 18)
        .padding(.vertical, 8)
    }

    private var statusText: String {
        guard let engine = store.selected else { return "No engine selected" }
        let kind = kindLabel(engine.kind)
        switch store.selectedReady {
        case .some(true):  return "\(kind) · Ready"
        case .some(false): return "\(kind) · Unavailable"
        case .none:        return "\(kind) · Checking…"
        }
    }

    private var readinessColor: Color {
        switch store.selectedReady {
        case .some(true):  return Theme.accent
        case .some(false): return .orange
        case .none:        return Theme.subtle
        }
    }

    // MARK: Conversation

    private var conversation: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 12) {
                    if visibleMessages.isEmpty && !store.busy {
                        emptyState
                    }
                    ForEach(Array(visibleMessages.enumerated()), id: \.offset) { _, message in
                        MessageBubble(message: message)
                            .id(message.id)
                    }
                    if store.busy {
                        ThinkingIndicator()
                            .id("thinking")
                    }
                    if let error = store.lastError {
                        errorBanner(error)
                    }
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 16)
            }
            // Keep the latest content in view as messages and the thinking
            // indicator appear.
            .onChange(of: store.messages.count) { _, _ in scrollToBottom(proxy) }
            .onChange(of: store.busy) { _, _ in scrollToBottom(proxy) }
        }
    }

    private var visibleMessages: [ChatMessage] {
        // System turns are infrastructure; never render them as bubbles.
        store.messages.filter { $0.role != .system }
    }

    private var emptyState: some View {
        VStack(spacing: 12) {
            Image(systemName: "bubble.left.and.text.bubble.right")
                .font(.system(size: 38, weight: .light))
                .foregroundStyle(Theme.accent.opacity(0.8))
            Text("Ask Crab anything")
                .font(.headline)
                .foregroundStyle(.white)
            Text("Pick an engine above, then start the conversation.")
                .font(.subheadline)
                .foregroundStyle(Theme.subtle)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.top, 64)
    }

    private func errorBanner(_ text: String) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
            Text(text)
                .font(.footnote)
                .foregroundStyle(.white.opacity(0.9))
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: 12, style: .continuous))
    }

    // MARK: Composer

    private var composer: some View {
        VStack(spacing: 0) {
            Divider().overlay(Theme.hairline)
            HStack(alignment: .bottom, spacing: 10) {
                TextField("Message Crab…", text: $draft, axis: .vertical)
                    .lineLimit(1...5)
                    .textInputAutocapitalization(.sentences)
                    .focused($composerFocused)
                    .foregroundStyle(.white)
                    .padding(.horizontal, 14)
                    .padding(.vertical, 10)
                    .background(Theme.panel, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
                    .overlay(
                        RoundedRectangle(cornerRadius: 18, style: .continuous)
                            .stroke(Theme.hairline, lineWidth: 1)
                    )
                    .onSubmit(send)

                Button(action: send) {
                    Image(systemName: "arrow.up")
                        .font(.system(size: 17, weight: .bold))
                        .foregroundStyle(canSend ? Theme.bg : Theme.subtle)
                        .frame(width: 40, height: 40)
                        .background(
                            Circle().fill(canSend ? Theme.accent : Theme.panel)
                        )
                }
                .disabled(!canSend)
                .animation(.easeInOut(duration: 0.15), value: canSend)
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 12)
        }
        .background(Theme.bg)
    }

    private var canSend: Bool {
        !store.busy && !draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    // MARK: Actions

    private func send() {
        let text = draft
        guard canSend else { return }
        draft = ""
        Task { await store.send(text) }
    }

    private func scrollToBottom(_ proxy: ScrollViewProxy) {
        withAnimation(.easeOut(duration: 0.2)) {
            if store.busy {
                proxy.scrollTo("thinking", anchor: .bottom)
            } else if let last = visibleMessages.last {
                proxy.scrollTo(last.id, anchor: .bottom)
            }
        }
    }

    // MARK: Engine kind glyphs

    private func kindSymbol(_ kind: EngineKind?) -> String {
        switch kind {
        case .onDevice: return "cpu"
        case .sandbox:  return "shippingbox"
        case .system:   return "apple.logo"
        case .none:     return "questionmark.circle"
        }
    }

    private func kindLabel(_ kind: EngineKind) -> String {
        switch kind {
        case .onDevice: return "On-device"
        case .sandbox:  return "Sandbox"
        case .system:   return "System"
        }
    }
}

// MARK: - Engine row identity

private struct EngineRow: Identifiable {
    let engine: any LLMEngine
    let isSelected: Bool
    var id: String { engine.displayName }
}

// MARK: - Message bubble

/// A single chat bubble. User turns are accent-tinted and right-aligned;
/// assistant turns are panel-colored and left-aligned.
private struct MessageBubble: View {
    let message: ChatMessage

    private var isUser: Bool { message.role == .user }

    var body: some View {
        HStack {
            if isUser { Spacer(minLength: 40) }
            Text(message.content)
                .font(.body)
                .foregroundStyle(isUser ? Theme.bg : .white)
                .textSelection(.enabled)
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
                .background(bubbleBackground)
            if !isUser { Spacer(minLength: 40) }
        }
    }

    @ViewBuilder private var bubbleBackground: some View {
        if isUser {
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(Theme.accent)
        } else {
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(Theme.panel)
                .overlay(
                    RoundedRectangle(cornerRadius: 16, style: .continuous)
                        .stroke(Theme.hairline, lineWidth: 1)
                )
        }
    }
}

// MARK: - Thinking indicator

/// Three pulsing dots shown while the engine is generating a reply.
private struct ThinkingIndicator: View {
    @State private var phase = 0.0

    var body: some View {
        HStack(spacing: 6) {
            ForEach(0..<3, id: \.self) { i in
                Circle()
                    .fill(Theme.accent)
                    .frame(width: 7, height: 7)
                    .opacity(opacity(for: i))
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 12)
        .background(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(Theme.panel)
                .overlay(
                    RoundedRectangle(cornerRadius: 16, style: .continuous)
                        .stroke(Theme.hairline, lineWidth: 1)
                )
        )
        .frame(maxWidth: .infinity, alignment: .leading)
        .onAppear {
            withAnimation(.easeInOut(duration: 0.9).repeatForever(autoreverses: true)) {
                phase = 1
            }
        }
    }

    /// Staggered opacity so the dots appear to ripple.
    private func opacity(for index: Int) -> Double {
        let offset = Double(index) * 0.25
        let v = (phase + offset).truncatingRemainder(dividingBy: 1)
        return 0.3 + 0.7 * abs(sin(v * .pi))
    }
}

// MARK: - ChatMessage identity
//
// CrabboxKit's ChatMessage is value-type and has no id; we derive a stable
// identity for SwiftUI from its content + role + position via this helper.
private extension ChatMessage {
    var id: String { "\(role.rawValue):\(content.hashValue)" }
}
