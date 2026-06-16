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
//  Provider selection policy (see `makeProvisioner()`):
//    1. If a crabbox.sh token exists -> CoordinatorProvisioner (the primary path;
//       the phone holds only a session token, never raw provider keys).
//    2. Else if islo is enabled and a key exists -> IsloProvisioner (the direct,
//       brokerless fallback the user opts into).
//    3. Else nil -> the Sandboxes tab shows its "configure a provider" state.
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

    /// Whether the optional direct islo.dev provider is enabled at all. When off,
    /// the islo key is ignored even if present and the crabbox path is the only
    /// provisioner candidate.
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
        isloEnabled && (isloKey?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false)
    }

    /// A short, user-facing label for whichever provider `makeProvisioner()` will
    /// select, or a prompt to configure one. Mirrors the selection policy exactly.
    var providerLabel: String {
        if hasCrabboxToken { return "crabbox.sh" }
        if hasIsloProvider { return "islo.dev (direct)" }
        return "No provider configured"
    }

    // MARK: - Provisioner factory

    /// Builds the active `SandboxProvisioner` per the selection policy, or `nil`
    /// when nothing is configured. crabbox.sh (the coordinator) is always
    /// preferred over the direct islo path when both are available.
    func makeProvisioner() -> (any SandboxProvisioner)? {
        // 1. Primary: crabbox.sh coordinator, identified only by a session token.
        if let token = crabboxToken?.trimmingCharacters(in: .whitespacesAndNewlines),
           !token.isEmpty,
           let provisioner = CoordinatorProvisioner(coordinatorURL: coordinatorURL, token: token) {
            return provisioner
        }

        // 2. Optional direct: islo.dev, only when explicitly enabled with a key.
        if isloEnabled,
           let key = isloKey?.trimmingCharacters(in: .whitespacesAndNewlines),
           !key.isEmpty,
           let provisioner = IsloProvisioner(apiKey: key, baseURL: isloBaseURL) {
            return provisioner
        }

        // 3. Nothing configured.
        return nil
    }

    // MARK: - UserDefaults keys

    private enum Keys {
        static let coordinatorURL = "settings.coordinatorURL"
        static let isloEnabled = "settings.islo.enabled"
        static let isloBaseURL = "settings.islo.baseURL"
    }
}
