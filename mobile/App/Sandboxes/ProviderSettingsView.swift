//
//  ProviderSettingsView.swift
//  Crabbox
//
//  The provider configuration sheet for the Sandboxes tab. crabbox.sh credentials
//  are used for portal/workspace flows, but coordinator sandbox lifecycle is
//  intentionally unavailable until the API exists. islo.dev is an OPTIONAL direct
//  provider (islo is the one brokerless crabbox provider, so it can't be driven
//  through crabbox.sh) that the user can enable with their own key.
//
//  Secrets are written straight to the Keychain via AppSettings' Keychain-backed
//  @Published properties; nothing here persists to UserDefaults.
//

import SwiftUI
import CrabboxKit

struct ProviderSettingsView: View {
    @EnvironmentObject private var settings: AppSettings
    @Environment(\.dismiss) private var dismiss

    // Local editing mirrors so SecureFields don't fight @Published writes.
    @State private var coordinatorURL = ""
    @State private var crabboxToken = ""
    @State private var isloEnabled = false
    @State private var isloBaseURL = ""
    @State private var isloKey = ""

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 20) {
                    coordinatorCard
                    isloCard
                    Text("crabbox.sh supports portal and workspace flows. Enable islo.dev direct when you need sandbox lifecycle from the app.")
                        .font(.footnote)
                        .foregroundStyle(Theme.textMuted)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 4)
                }
                .padding(20)
            }
            .background(Theme.background.ignoresSafeArea())
            .navigationTitle("Providers")
            .toolbarColorScheme(.dark, for: .navigationBar)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button("Done") { save(); dismiss() }
                        .fontWeight(.semibold)
                        .foregroundStyle(Theme.accent)
                }
            }
        }
        .onAppear(perform: load)
    }

    // MARK: crabbox.sh (primary)

    private var coordinatorCard: some View {
        Card {
            VStack(alignment: .leading, spacing: 14) {
                Label("crabbox.sh", systemImage: "shippingbox.fill")
                    .font(.headline)
                    .foregroundStyle(Theme.textPrimary)
                Text("Portal and workspace control plane. Sandbox lifecycle is unavailable until the coordinator API supports it.")
                    .font(.caption)
                    .foregroundStyle(Theme.textMuted)

                fieldLabel("Coordinator URL")
                TextField("https://crabbox.sh", text: $coordinatorURL)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .keyboardType(.URL)
                    .styledField()

                fieldLabel("Session token")
                SecureField("paste your crabbox token", text: $crabboxToken)
                    .styledField()
            }
        }
    }

    // MARK: islo.dev (optional)

    private var isloCard: some View {
        Card {
            VStack(alignment: .leading, spacing: 14) {
                Toggle(isOn: $isloEnabled) {
                    Label("islo.dev (direct)", systemImage: "cube.transparent.fill")
                        .font(.headline)
                        .foregroundStyle(Theme.textPrimary)
                }
                .tint(Theme.accent)

                Text("Optional. Talks to api.islo.dev directly with your key — islo is brokerless, so it isn't managed by crabbox.sh.")
                    .font(.caption)
                    .foregroundStyle(Theme.textMuted)

                if isloEnabled {
                    fieldLabel("islo base URL")
                    TextField("https://api.islo.dev", text: $isloBaseURL)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .keyboardType(.URL)
                        .styledField()

                    fieldLabel("API key")
                    SecureField("ak_…", text: $isloKey)
                        .styledField()
                }
            }
        }
    }

    private func fieldLabel(_ text: String) -> some View {
        Text(text.uppercased())
            .font(.system(size: 11, weight: .bold))
            .foregroundStyle(Theme.textMuted)
    }

    // MARK: Load / save

    private func load() {
        coordinatorURL = settings.coordinatorURL
        crabboxToken = settings.crabboxToken ?? ""
        isloEnabled = settings.isloEnabled
        isloBaseURL = settings.isloBaseURL
        isloKey = settings.isloKey ?? ""
    }

    private func save() {
        let url = coordinatorURL.trimmingCharacters(in: .whitespacesAndNewlines)
        settings.coordinatorURL = url.isEmpty ? defaultCoordinatorURL : url
        settings.crabboxToken = crabboxToken.trimmingCharacters(in: .whitespacesAndNewlines)
        settings.isloEnabled = isloEnabled
        let isloURL = isloBaseURL.trimmingCharacters(in: .whitespacesAndNewlines)
        settings.isloBaseURL = isloURL.isEmpty ? "https://api.islo.dev" : isloURL
        settings.isloKey = isloKey.trimmingCharacters(in: .whitespacesAndNewlines)
    }
}

private extension View {
    /// Standard dark text-field chrome used throughout the settings sheet.
    func styledField() -> some View {
        self
            .foregroundStyle(Theme.textPrimary)
            .padding(.horizontal, 14)
            .padding(.vertical, 12)
            .background(Theme.field, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: 12, style: .continuous)
                    .strokeBorder(Theme.hairline, lineWidth: 1)
            )
    }
}
