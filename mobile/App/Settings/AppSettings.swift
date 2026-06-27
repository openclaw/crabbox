//
//  AppSettings.swift
//  Crabbox
//
//  The single source of truth for user-configurable preferences and the secrets
//  that back sandbox provisioning. Non-secret prefs live in UserDefaults; the two
//  secrets (the crabbox.sh session token and an optional islo.dev API key) live
//  in the Keychain via `KeychainStore` and are *mirrored* into @Published
//  properties so SwiftUI can bind to them without ever writing them to a plist.
//
//  Sandbox provider selection policy (see `makeProvisioner()`):
//    1. If islo is enabled and a key exists -> IsloProvisioner.
//    2. Else nil -> the Sandboxes tab shows its "configure a provider" state.
//  The crabbox.sh token is still used by portal/workspace flows, but crabbox.sh
//  sandbox lifecycle stays unavailable until the coordinator exposes it.
//

import Foundation
import Combine
import CrabboxKit

@MainActor
final class AppSettings: ObservableObject {

    // MARK: - Persisted, non-secret preferences (UserDefaults)

    /// The crabbox coordinator the Portal and CoordinatorProvisioner talk to.
    /// Defaults to CrabboxKit's `defaultCoordinatorURL` ("https://crabbox.sh").
    @Published var coordinatorURL: String {
        didSet { defaults.set(coordinatorURL, forKey: Keys.coordinatorURL) }
    }

    /// Whether the optional direct islo.dev sandbox provider is enabled at all.
    /// When off, the islo key is ignored even if present.
    @Published var isloEnabled: Bool {
        didSet { defaults.set(isloEnabled, forKey: Keys.isloEnabled) }
    }

    /// The islo API base URL (overridable for staging/self-hosted islo).
    @Published var isloBaseURL: String {
        didSet { defaults.set(isloBaseURL, forKey: Keys.isloBaseURL) }
    }

    // MARK: - Secrets (Keychain-backed, mirrored for binding)

    /// crabbox.sh session token. Setting this writes through to the Keychain;
    /// setting `nil`/empty clears it. This is the *primary* credential.
    @Published var crabboxToken: String? {
        didSet {
            guard crabboxToken != oldValue else { return }
            KeychainStore.set(crabboxToken, for: .crabboxToken)
        }
    }

    /// Optional islo.dev API key. Only consulted when `isloEnabled` is true.
    @Published var isloKey: String? {
        didSet {
            guard isloKey != oldValue else { return }
            KeychainStore.set(isloKey, for: .isloKey)
        }
    }

    // MARK: - Init

    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults

        // Non-secret prefs, with sensible CrabboxKit-backed defaults.
        self.coordinatorURL = defaults.string(forKey: Keys.coordinatorURL) ?? defaultCoordinatorURL
        self.isloEnabled = defaults.bool(forKey: Keys.isloEnabled) // defaults to false
        self.isloBaseURL = defaults.string(forKey: Keys.isloBaseURL) ?? "https://api.islo.dev"

        // Secrets are loaded from the Keychain, never from UserDefaults.
        self.crabboxToken = KeychainStore.get(.crabboxToken)
        self.isloKey = KeychainStore.get(.isloKey)

        #if DEBUG
        // DEBUG-only: allow preloading the direct-islo provider from the launch
        // environment (SIMCTL_CHILD_CRABBOX_ISLO_KEY=…) for screenshot/demo runs.
        // Never used in Release; the key is still mirrored into the Keychain.
        if let injected = ProcessInfo.processInfo.environment["CRABBOX_ISLO_KEY"],
           !injected.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            self.isloEnabled = true
            self.isloKey = injected
        }
        #endif
    }

    // MARK: - Derived state

    /// True when a crabbox.sh token is present and non-empty.
    var hasCrabboxToken: Bool {
        (crabboxToken?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false)
    }

    /// True when islo is enabled and a non-empty key is present.
    var hasIsloProvider: Bool {
        sandboxSelection == .isloDirect
    }

    private var sandboxSelection: SandboxProviderSelection {
        selectSandboxProvider(
            isloEnabled: isloEnabled,
            isloKey: isloKey,
            crabboxToken: crabboxToken
        )
    }

    /// A short, user-facing label for whichever sandbox provider `makeProvisioner()`
    /// will select, or a prompt to configure one. Mirrors the selection policy exactly.
    var providerLabel: String {
        sandboxProviderLabel(for: sandboxSelection)
    }

    // MARK: - Provisioner factory

    /// Builds the active `SandboxProvisioner` per the selection policy, or `nil`
    /// when no supported sandbox lifecycle provider is configured.
    func makeProvisioner() -> (any SandboxProvisioner)? {
        if isloEnabled,
           let key = isloKey?.trimmingCharacters(in: .whitespacesAndNewlines),
           !key.isEmpty,
           let normalizedIslo = normalizeCredentialEndpointURL(isloBaseURL),
           let provisioner = IsloProvisioner(apiKey: key, baseURL: normalizedIslo) {
            return provisioner
        }

        return nil
    }

    // MARK: - UserDefaults keys

    private enum Keys {
        static let coordinatorURL = "settings.coordinatorURL"
        static let isloEnabled = "settings.islo.enabled"
        static let isloBaseURL = "settings.islo.baseURL"
    }
}
