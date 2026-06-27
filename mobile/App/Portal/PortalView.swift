import SwiftUI
import CrabboxKit

/// The Portal tab: a native shell around the coordinator web view.
///
/// Layout (top → bottom):
///   • Header — 🦀 Crabbox wordmark, the live host (`selectCurrentHost`), a
///     status pill (`selectStatusLabel`), and a settings gear.
///   • Progress hairline — width driven by `selectLoadingWidthPct`, shown only
///     while loading.
///   • WebView — the coordinator itself.
///   • Bottom nav bar — back / home / reload / share / open-in-browser.
///
/// The native chrome (header, nav bar, coordinator switcher) is deliberate: it
/// keeps this from being a bare web wrapper and satisfies App Store Guideline 4.2.
/// Every interactive control dispatches an `AppAction` into the `PortalModel`;
/// nothing here mutates state directly.
struct PortalView: View {
    @StateObject private var model = PortalModel()
    /// Drives the iOS share sheet for the current page URL.
    @State private var shareItem: ShareItem?

    var body: some View {
        VStack(spacing: 0) {
            header
            progressBar
            webViewArea
            navBar
        }
        .background(Theme.bg.ignoresSafeArea())
        .onAppear { model.boot() }
        // Coordinator switcher.
        .sheet(isPresented: settingsBinding) {
            CoordinatorSettingsSheet(model: model)
                .presentationDetents([.medium])
                .presentationDragIndicator(.visible)
        }
        // Native share sheet for the current URL.
        .sheet(item: $shareItem) { item in
            ActivityView(items: [item.url])
        }
    }

    // MARK: - Header

    private var header: some View {
        HStack(spacing: 12) {
            HStack(spacing: 8) {
                Text("🦀")
                    .font(.system(size: 22))
                VStack(alignment: .leading, spacing: 1) {
                    Text("Crabbox")
                        .font(.system(size: 17, weight: .bold, design: .rounded))
                        .foregroundStyle(Theme.textPrimary)
                    Text(selectCurrentHost(model.state))
                        .font(.system(size: 12, weight: .medium, design: .rounded))
                        .foregroundStyle(Theme.textSecondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
            }

            Spacer()

            statusPill

            Button {
                model.dispatch(.openSettings)
            } label: {
                Image(systemName: "slider.horizontal.3")
                    .font(.system(size: 17, weight: .semibold))
                    .foregroundStyle(Theme.textPrimary)
                    .frame(width: 38, height: 38)
                    .background(Theme.raised)
                    .clipShape(RoundedRectangle(cornerRadius: 11, style: .continuous))
            }
            .accessibilityLabel("Coordinator settings")
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .background(Theme.panel)
    }

    /// "Live" (accent), "Loading" (accent), or "Offline" (danger).
    private var statusPill: some View {
        let label = selectStatusLabel(model.state)
        let color: Color = label == .offline ? Theme.danger : Theme.accent
        return Pill(text: label.rawValue, color: color)
    }

    // MARK: - Progress

    @ViewBuilder
    private var progressBar: some View {
        GeometryReader { geo in
            let pct = CGFloat(selectLoadingWidthPct(model.state)) / 100
            Rectangle()
                .fill(Theme.accent)
                .frame(width: model.state.isLoading ? geo.size.width * pct : 0)
                .animation(.easeOut(duration: 0.25), value: model.state.progress)
                .animation(.easeOut(duration: 0.25), value: model.state.isLoading)
        }
        .frame(height: 2)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Theme.panel)
    }

    // MARK: - Web view + failure overlay

    private var webViewArea: some View {
        ZStack {
            // `webViewKey` forces a fresh web view (and a clean back stack) when
            // the coordinator changes.
            WebView(model: model)
                .id(model.state.webViewKey)
                .ignoresSafeArea(edges: .bottom)

            if model.state.loadFailed {
                failureOverlay
            }
        }
    }

    private var failureOverlay: some View {
        VStack(spacing: 14) {
            Image(systemName: "wifi.exclamationmark")
                .font(.system(size: 40, weight: .regular))
                .foregroundStyle(Theme.danger)
            Text("Couldn’t reach \(selectCurrentHost(model.state))")
                .font(.system(size: 16, weight: .semibold, design: .rounded))
                .foregroundStyle(Theme.textPrimary)
                .multilineTextAlignment(.center)
            Button("Try again") { model.dispatch(.reload) }
                .buttonStyle(AccentButtonStyle(fullWidth: false))
        }
        .padding(28)
        .card()
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Theme.bg.opacity(0.96))
    }

    // MARK: - Bottom nav bar

    private var navBar: some View {
        HStack(spacing: 0) {
            navButton(system: "chevron.backward",
                      enabled: model.state.canGoBack,
                      label: "Back") {
                model.dispatch(.goBack)
            }
            navButton(system: "house", enabled: true, label: "Home") {
                model.dispatch(.goHome)
            }
            navButton(system: "arrow.clockwise", enabled: true, label: "Reload") {
                model.dispatch(.reload)
            }
            navButton(system: "square.and.arrow.up", enabled: true, label: "Share") {
                if let url = URL(string: currentShareURL) {
                    shareItem = ShareItem(url: url)
                }
            }
            navButton(system: "safari", enabled: true, label: "Open in browser") {
                if let url = URL(string: currentShareURL) {
                    UIApplication.shared.open(url)
                }
            }
        }
        .padding(.horizontal, 8)
        .padding(.top, 8)
        .background(Theme.panel)
    }

    /// A single bottom-bar control. Disabled controls dim and stop responding.
    private func navButton(system: String,
                           enabled: Bool,
                           label: String,
                           action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: system)
                .font(.system(size: 19, weight: .medium))
                .foregroundStyle(enabled ? Theme.textPrimary : Theme.textSecondary.opacity(0.4))
                .frame(maxWidth: .infinity)
                .frame(height: 30)
        }
        .disabled(!enabled)
        .accessibilityLabel(label)
    }

    // MARK: - Helpers

    /// URL to share / open externally: the live page, falling back to home.
    private var currentShareURL: String {
        model.state.currentURL.isEmpty ? model.state.homeURL : model.state.currentURL
    }

    /// Two-way binding between `state.settingsVisible` and the `.sheet`. Closing
    /// the sheet dispatches `closeSettings` so state stays authoritative.
    private var settingsBinding: Binding<Bool> {
        Binding(
            get: { model.state.settingsVisible },
            set: { visible in if !visible { model.dispatch(.closeSettings) } }
        )
    }
}

// MARK: - Coordinator settings sheet

/// Lets the user point the Portal at a different coordinator. The text field is
/// bound to `draftURL`; "Connect" validates+saves via `saveCoordinator` (errors
/// surface in `urlError`), and "Use crabbox.sh" restores the default.
private struct CoordinatorSettingsSheet: View {
    @ObservedObject var model: PortalModel
    @FocusState private var fieldFocused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            HStack {
                Text("Coordinator")
                    .font(.system(size: 20, weight: .bold, design: .rounded))
                    .foregroundStyle(Theme.textPrimary)
                Spacer()
                Button { model.dispatch(.closeSettings) } label: {
                    Image(systemName: "xmark")
                        .font(.system(size: 14, weight: .bold))
                        .foregroundStyle(Theme.textSecondary)
                        .frame(width: 30, height: 30)
                        .background(Theme.raised)
                        .clipShape(Circle())
                }
            }

            Text("Connect to a self-hosted Crabbox coordinator, or use the default.")
                .font(.system(size: 13))
                .foregroundStyle(Theme.textSecondary)

            VStack(alignment: .leading, spacing: 8) {
                TextField(
                    "https://crabbox.sh",
                    text: Binding(
                        get: { model.state.draftURL },
                        set: { model.dispatch(.setDraft(url: $0)) }
                    )
                )
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .keyboardType(.URL)
                .submitLabel(.go)
                .focused($fieldFocused)
                .onSubmit { model.dispatch(.saveCoordinator) }
                .font(.system(size: 15, design: .monospaced))
                .foregroundStyle(Theme.textPrimary)
                .padding(12)
                .background(Theme.raised)
                .clipShape(RoundedRectangle(cornerRadius: 11, style: .continuous))
                .overlay(
                    RoundedRectangle(cornerRadius: 11, style: .continuous)
                        .strokeBorder(model.state.urlError.isEmpty
                                      ? Color.white.opacity(0.07)
                                      : Theme.danger, lineWidth: 1)
                )

                if !model.state.urlError.isEmpty {
                    Text(model.state.urlError)
                        .font(.system(size: 12, weight: .medium))
                        .foregroundStyle(Theme.danger)
                }
            }

            HStack(spacing: 12) {
                Button("Use crabbox.sh") { model.dispatch(.resetCoordinator) }
                    .buttonStyle(SecondaryButtonStyle())
                Button("Connect") { model.dispatch(.saveCoordinator) }
                    .buttonStyle(AccentButtonStyle())
            }

            Spacer(minLength: 0)
        }
        .padding(20)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .background(Theme.panel.ignoresSafeArea())
        .onAppear { fieldFocused = true }
    }
}

// MARK: - Share sheet plumbing

/// Identifiable wrapper so the share `.sheet(item:)` knows when to present.
private struct ShareItem: Identifiable {
    let id = UUID()
    let url: URL
}

/// Minimal `UIActivityViewController` bridge for the native share sheet.
private struct ActivityView: UIViewControllerRepresentable {
    let items: [Any]
    func makeUIViewController(context: Context) -> UIActivityViewController {
        UIActivityViewController(activityItems: items, applicationActivities: nil)
    }
    func updateUIViewController(_ controller: UIActivityViewController, context: Context) {}
}
