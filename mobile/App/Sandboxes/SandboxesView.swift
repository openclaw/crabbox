//
//  SandboxesView.swift
//  Crabbox
//
//  The "Sandboxes" tab: the demo surface for provisioning a leased Linux box that
//  runs Ollama, then handing the resulting `SandboxEngine` to the Assistant tab.
//
//  Everything provider-specific is funnelled through CrabboxKit's
//  `SandboxProvisioner` seam — this view never knows whether it is talking to
//  crabbox.sh or directly to islo.dev. It asks `AppSettings.makeProvisioner()`
//  for whatever is configured and drives the provider-agnostic flow:
//
//      list()                         -> render existing leases
//      launchLLMSandbox(provisioner:) -> create -> bootstrap -> expose -> ready
//      stop(id:)                      -> tear a lease down
//
//  On a successful launch the returned `SandboxEngine` is registered into the
//  shared `EngineHub`, and we ask the app to switch to the Assistant tab so the
//  user can immediately chat with the box they just created.
//

import SwiftUI
import CrabboxKit

// MARK: - Shared engine store

/// The cross-tab registry of chat engines produced at runtime (today: sandbox
/// Ollama engines minted by this view). `AssistantView` / its `ChatStore` read
/// `sandboxEngines` to populate the engine picker alongside the always-present
/// on-device and Apple Foundation Models engines.
///
/// Kept intentionally tiny: it is a published list plus de-duplicating insert.
@MainActor
final class EngineHub: ObservableObject {
    /// Engines minted from leased sandboxes, newest first.
    @Published private(set) var sandboxEngines: [any LLMEngine] = []

    /// Registers a sandbox engine, replacing any existing entry with the same
    /// display name (re-launching the same sandbox refreshes its endpoint rather
    /// than stacking duplicates in the picker).
    func register(_ engine: any LLMEngine) {
        sandboxEngines.removeAll { $0.displayName == engine.displayName }
        sandboxEngines.insert(engine, at: 0)
    }

    /// Drops an engine by display name (used when its backing sandbox is stopped).
    func remove(displayName: String) {
        sandboxEngines.removeAll { $0.displayName == displayName }
    }
}

// MARK: - Launch progress

/// The user-visible phases of `launchLLMSandbox`. CrabboxKit performs the whole
/// flow in one call, so these are advanced optimistically on a timeline to give
/// the launch a sense of motion; `.ready`/`.failed` reflect the real outcome.
private enum LaunchPhase: Equatable {
    case idle
    case creating
    case installingOllama
    case exposing
    case ready
    case failed(String)

    var label: String {
        switch self {
        case .idle: return ""
        case .creating: return "Creating sandbox…"
        case .installingOllama: return "Installing Ollama…"
        case .exposing: return "Exposing endpoint…"
        case .ready: return "Ready"
        case .failed(let msg): return msg
        }
    }

    /// Fractional progress for the launch bar (0…1).
    var fraction: Double {
        switch self {
        case .idle, .failed: return 0
        case .creating: return 0.25
        case .installingOllama: return 0.6
        case .exposing: return 0.85
        case .ready: return 1
        }
    }

    var isRunning: Bool {
        switch self {
        case .creating, .installingOllama, .exposing: return true
        default: return false
        }
    }
}

// MARK: - View

struct SandboxesView: View {
    @EnvironmentObject private var settings: AppSettings
    @EnvironmentObject private var engineHub: EngineHub

    /// Binding to the root `TabView` selection so a successful launch can jump the
    /// user straight to the Assistant tab.
    @Binding var selectedTab: RootTab

    // Existing leases.
    @State private var handles: [SandboxHandle] = []
    @State private var isRefreshing = false
    @State private var listError: String?

    // Launch flow.
    @State private var model = SandboxModel.default.id
    @State private var phase: LaunchPhase = .idle
    @State private var lastLaunchedEngineName: String?

    // Provider settings sheet.
    @State private var showingProviderSettings = false

    // IDs we are currently stopping (for per-row spinners).
    @State private var stoppingIDs: Set<String> = []

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 20) {
                    providerCard
                    if settings.makeProvisioner() != nil {
                        launchCard
                        leasesCard
                    } else {
                        noProviderCard
                    }
                }
                .padding(20)
            }
            .background(Theme.background.ignoresSafeArea())
            .navigationTitle("Sandboxes")
            .toolbarColorScheme(.dark, for: .navigationBar)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button {
                        showingProviderSettings = true
                    } label: {
                        Image(systemName: "slider.horizontal.3")
                            .foregroundStyle(Theme.accent)
                    }
                    .accessibilityLabel("Provider settings")
                }
            }
            .sheet(isPresented: $showingProviderSettings, onDismiss: refreshAfterSettingsChange) {
                ProviderSettingsView()
                    .environmentObject(settings)
            }
            .task(id: providerIdentity) { await refresh() }
            .refreshable { await refresh() }
        }
    }

    // MARK: - Provider card

    private var providerCard: some View {
        Card {
            HStack(spacing: 14) {
                Image(systemName: providerSymbol)
                    .font(.system(size: 22, weight: .semibold))
                    .foregroundStyle(Theme.accent)
                    .frame(width: 44, height: 44)
                    .background(Theme.accent.opacity(0.12), in: RoundedRectangle(cornerRadius: 12, style: .continuous))

                VStack(alignment: .leading, spacing: 3) {
                    Text("Active provider")
                        .font(.caption)
                        .foregroundStyle(Theme.textMuted)
                    Text(settings.providerLabel)
                        .font(.headline)
                        .foregroundStyle(Theme.textPrimary)
                }
                Spacer()
                Button("Change") { showingProviderSettings = true }
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(Theme.accent)
            }
        }
    }

    private var providerSymbol: String {
        if settings.hasCrabboxToken { return "shippingbox.fill" }
        if settings.hasIsloProvider { return "cube.transparent.fill" }
        return "exclamationmark.shield.fill"
    }

    // MARK: - No-provider state

    private var noProviderCard: some View {
        Card {
            VStack(spacing: 14) {
                Image(systemName: "key.horizontal.fill")
                    .font(.system(size: 30, weight: .semibold))
                    .foregroundStyle(Theme.accent)
                Text("Connect a provider")
                    .font(.headline)
                    .foregroundStyle(Theme.textPrimary)
                Text("Add a crabbox.sh token to provision sandboxes through the coordinator, or enable the optional islo.dev provider with your own key.")
                    .font(.subheadline)
                    .multilineTextAlignment(.center)
                    .foregroundStyle(Theme.textMuted)
                Button {
                    showingProviderSettings = true
                } label: {
                    Text("Open provider settings")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(PrimaryButtonStyle())
            }
        }
    }

    // MARK: - Launch card

    private var launchCard: some View {
        Card {
            VStack(alignment: .leading, spacing: 16) {
                Text("Launch an LLM sandbox")
                    .font(.headline)
                    .foregroundStyle(Theme.textPrimary)

                // Model picker.
                VStack(alignment: .leading, spacing: 8) {
                    Text("Model")
                        .font(.caption)
                        .foregroundStyle(Theme.textMuted)
                    Menu {
                        ForEach(SandboxModel.catalog) { option in
                            Button {
                                model = option.id
                            } label: {
                                if model == option.id {
                                    Label(option.label, systemImage: "checkmark")
                                } else {
                                    Text(option.label)
                                }
                            }
                        }
                    } label: {
                        HStack {
                            Image(systemName: "cpu")
                                .foregroundStyle(Theme.accent)
                            Text(SandboxModel.label(for: model))
                                .foregroundStyle(Theme.textPrimary)
                            Spacer()
                            Image(systemName: "chevron.up.chevron.down")
                                .font(.footnote)
                                .foregroundStyle(Theme.textMuted)
                        }
                        .padding(.horizontal, 14)
                        .padding(.vertical, 12)
                        .background(Theme.field, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                    }
                    .disabled(phase.isRunning)
                }

                // Launch button + progress.
                Button {
                    Task { await launch() }
                } label: {
                    HStack(spacing: 8) {
                        if phase.isRunning {
                            ProgressView()
                                .tint(.black)
                        } else {
                            Image(systemName: "bolt.fill")
                        }
                        Text(phase.isRunning ? phase.label : "Launch LLM sandbox")
                    }
                    .frame(maxWidth: .infinity)
                }
                .buttonStyle(PrimaryButtonStyle())
                .disabled(phase.isRunning)

                if phase != .idle {
                    LaunchProgressView(phase: phase)
                }

                // Post-success affordance: jump to the Assistant with this engine.
                if case .ready = phase, let name = lastLaunchedEngineName {
                    Button {
                        selectedTab = .assistant
                    } label: {
                        HStack {
                            Image(systemName: "bubble.left.and.text.bubble.right.fill")
                            Text("Chat with \(name)")
                            Spacer()
                            Image(systemName: "arrow.right")
                        }
                        .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(SecondaryButtonStyle())
                }
            }
        }
    }

    // MARK: - Leases card

    private var leasesCard: some View {
        Card {
            VStack(alignment: .leading, spacing: 14) {
                HStack {
                    Text("Active leases")
                        .font(.headline)
                        .foregroundStyle(Theme.textPrimary)
                    Spacer()
                    if isRefreshing {
                        ProgressView().tint(Theme.accent)
                    } else {
                        Button {
                            Task { await refresh() }
                        } label: {
                            Image(systemName: "arrow.clockwise")
                                .foregroundStyle(Theme.accent)
                        }
                        .accessibilityLabel("Refresh leases")
                    }
                }

                if let listError {
                    Label(listError, systemImage: "exclamationmark.triangle.fill")
                        .font(.footnote)
                        .foregroundStyle(.orange)
                } else if handles.isEmpty {
                    Text("No sandboxes yet. Launch one above to get started.")
                        .font(.subheadline)
                        .foregroundStyle(Theme.textMuted)
                        .frame(maxWidth: .infinity, alignment: .leading)
                } else {
                    ForEach(handles, id: \.id) { handle in
                        SandboxRow(
                            handle: handle,
                            isStopping: stoppingIDs.contains(handle.id),
                            onStop: { Task { await stop(handle) } }
                        )
                        if handle.id != handles.last?.id {
                            Divider().overlay(Theme.divider)
                        }
                    }
                }
            }
        }
    }

    // MARK: - Identity for `.task` invalidation

    /// A value that changes whenever the selected provider changes, so the leases
    /// list re-loads when the user switches providers in settings.
    private var providerIdentity: String {
        "\(settings.providerLabel)|\(settings.coordinatorURL)|\(settings.isloBaseURL)"
    }

    // MARK: - Actions

    private func refreshAfterSettingsChange() {
        Task { await refresh() }
    }

    /// Loads existing leases via the active provisioner.
    @MainActor
    private func refresh() async {
        guard let provisioner = settings.makeProvisioner() else {
            handles = []
            listError = nil
            return
        }
        isRefreshing = true
        listError = nil
        defer { isRefreshing = false }
        do {
            handles = try await provisioner.list()
        } catch {
            handles = []
            listError = "Couldn't load leases: \(describe(error))"
        }
    }

    /// Drives the full launch flow and registers the resulting engine.
    @MainActor
    private func launch() async {
        guard let provisioner = settings.makeProvisioner() else {
            phase = .failed("No provider configured")
            return
        }

        lastLaunchedEngineName = nil
        // Optimistic phase animation; the real work is one CrabboxKit call.
        phase = .creating
        let ticker = Task { @MainActor in
            try? await Task.sleep(nanoseconds: 1_200_000_000)
            if phase == .creating { phase = .installingOllama }
            try? await Task.sleep(nanoseconds: 2_000_000_000)
            if phase == .installingOllama { phase = .exposing }
        }

        do {
            let name = "crab-\(Int(Date().timeIntervalSince1970))"
            let (handle, engine) = try await launchLLMSandbox(
                provisioner: provisioner,
                name: name,
                model: model
            )
            ticker.cancel()
            engineHub.register(engine)
            lastLaunchedEngineName = engine.displayName
            phase = .ready
            // Reflect the new lease immediately, then reconcile with the server.
            if !handles.contains(where: { $0.id == handle.id }) {
                handles.insert(handle, at: 0)
            }
            await refresh()
        } catch {
            ticker.cancel()
            phase = .failed("Launch failed: \(describe(error))")
        }
    }

    /// Stops a lease and removes its engine from the hub.
    @MainActor
    private func stop(_ handle: SandboxHandle) async {
        guard let provisioner = settings.makeProvisioner() else { return }
        stoppingIDs.insert(handle.id)
        defer { stoppingIDs.remove(handle.id) }
        do {
            try await provisioner.stop(id: handle.id)
            handles.removeAll { $0.id == handle.id }
            engineHub.remove(displayName: "Sandbox · \(handle.id)")
        } catch {
            listError = "Couldn't stop \(handle.id): \(describe(error))"
        }
    }

    /// Compact, user-facing rendering of CrabboxKit / network errors.
    private func describe(_ error: Error) -> String {
        if let llm = error as? LLMError { return llm.description }
        return (error as NSError).localizedDescription
    }
}

// MARK: - Lease row

private struct SandboxRow: View {
    let handle: SandboxHandle
    let isStopping: Bool
    let onStop: () -> Void

    var body: some View {
        HStack(spacing: 12) {
            Circle()
                .fill(statusColor)
                .frame(width: 10, height: 10)
                .shadow(color: statusColor.opacity(0.7), radius: 4)

            VStack(alignment: .leading, spacing: 3) {
                Text(handle.id)
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(Theme.textPrimary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                HStack(spacing: 6) {
                    Text(handle.provider)
                    Text("·")
                    Text(handle.status)
                }
                .font(.caption)
                .foregroundStyle(Theme.textMuted)
            }
            Spacer()

            if isStopping {
                ProgressView().tint(Theme.accent)
            } else {
                Button(role: .destructive, action: onStop) {
                    Image(systemName: "stop.circle.fill")
                        .font(.title3)
                        .foregroundStyle(.red.opacity(0.85))
                }
                .accessibilityLabel("Stop \(handle.id)")
            }
        }
        .padding(.vertical, 6)
    }

    private var statusColor: Color {
        switch handle.status.lowercased() {
        case "running", "ready", "active": return Theme.accent
        case "creating", "pending", "starting": return .yellow
        case "stopped", "error", "failed": return .red
        default: return Theme.textMuted
        }
    }
}

// MARK: - Launch progress bar

private struct LaunchProgressView: View {
    let phase: LaunchPhase

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if case .failed(let msg) = phase {
                Label(msg, systemImage: "xmark.octagon.fill")
                    .font(.footnote)
                    .foregroundStyle(.red)
            } else {
                HStack {
                    Text(phase.label)
                        .font(.footnote)
                        .foregroundStyle(phase == .ready ? Theme.accent : Theme.textMuted)
                    Spacer()
                    if phase == .ready {
                        Image(systemName: "checkmark.circle.fill")
                            .foregroundStyle(Theme.accent)
                    }
                }
                GeometryReader { geo in
                    ZStack(alignment: .leading) {
                        Capsule().fill(Theme.field)
                        Capsule()
                            .fill(Theme.accent)
                            .frame(width: geo.size.width * phase.fraction)
                            .animation(.easeInOut(duration: 0.4), value: phase)
                    }
                }
                .frame(height: 6)
            }
        }
    }
}

// MARK: - Model catalog

/// The small curated set of Ollama models we offer for sandbox launches. Default
/// is intentionally tiny so a fresh lease becomes chat-ready quickly.
struct SandboxModel: Identifiable {
    let id: String
    let label: String

    static let `default` = SandboxModel(id: "qwen2.5:0.5b", label: "Qwen2.5 0.5B (fast)")

    static let catalog: [SandboxModel] = [
        .default,
        SandboxModel(id: "gemma3:1b", label: "Gemma 3 1B"),
        SandboxModel(id: "llama3.2:1b", label: "Llama 3.2 1B"),
        SandboxModel(id: "qwen2.5:3b", label: "Qwen2.5 3B"),
    ]

    static func label(for id: String) -> String {
        catalog.first { $0.id == id }?.label ?? id
    }
}
