//
//  KeychainStore.swift
//  Crabbox
//
//  A deliberately tiny wrapper over the Security framework for storing short
//  String secrets (the crabbox.sh session token, an optional islo.dev API key).
//  We keep this dependency-free on purpose: secrets must never touch UserDefaults
//  or a plist, and pulling in a third-party Keychain library for two strings is
//  not worth the supply-chain surface.
//
//  Keying scheme: each secret is a `kSecClassGenericPassword` item identified by
//  a fixed `service` (the bundle's secret namespace) plus a per-secret `account`.
//

import Foundation
#if canImport(Security)
import Security

/// Namespaced, synchronous String secret store backed by the iOS Keychain.
///
/// All methods are best-effort and non-throwing at the call site: a failed read
/// returns `nil`, a failed write/delete returns `false`. The app treats a missing
/// secret as "not configured", which is exactly the behaviour we want when the
/// Keychain is unavailable (e.g. early boot) rather than crashing.
enum KeychainStore {

    /// The Keychain `service` all Crabbox secrets live under. Distinct from the
    /// bundle id so it reads clearly in any Keychain inspector.
    private static let service = "sh.crabbox.Crabbox.secrets"

    /// Stable account names for each secret we persist.
    enum Account: String {
        case crabboxToken = "crabbox.token"
        case isloKey = "islo.apiKey"
    }

    // MARK: - Read

    /// Returns the stored secret for `account`, or `nil` if absent/unreadable.
    static func get(_ account: Account) -> String? {
        var query = baseQuery(account)
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne

        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        guard status == errSecSuccess,
              let data = item as? Data,
              let value = String(data: data, encoding: .utf8) else {
            return nil
        }
        return value
    }

    // MARK: - Write

    /// Stores (or replaces) the secret for `account`. Passing an empty/whitespace
    /// value deletes the item so callers can treat "" as "clear this secret".
    /// Returns `true` on success.
    @discardableResult
    static func set(_ value: String?, for account: Account) -> Bool {
        let trimmed = value?.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let trimmed, !trimmed.isEmpty else {
            return delete(account)
        }
        guard let data = trimmed.data(using: .utf8) else { return false }

        // Upsert: try to update an existing item first; if there is none, add it.
        let query = baseQuery(account)
        let attributes: [String: Any] = [
            kSecValueData as String: data,
            // Only readable while the device is unlocked; never synced off-device.
            kSecAttrAccessible as String: kSecAttrAccessibleWhenUnlockedThisDeviceOnly,
        ]

        let updateStatus = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
        if updateStatus == errSecSuccess { return true }
        if updateStatus == errSecItemNotFound {
            var addQuery = query
            addQuery.merge(attributes) { _, new in new }
            return SecItemAdd(addQuery as CFDictionary, nil) == errSecSuccess
        }
        return false
    }

    // MARK: - Delete

    /// Removes the secret for `account`. Treats "not found" as success.
    @discardableResult
    static func delete(_ account: Account) -> Bool {
        let status = SecItemDelete(baseQuery(account) as CFDictionary)
        return status == errSecSuccess || status == errSecItemNotFound
    }

    // MARK: - Helpers

    /// The shared identity portion of every query for a given account.
    private static func baseQuery(_ account: Account) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account.rawValue,
        ]
    }
}

#else

// Non-Apple platforms (e.g. the Linux test/sim targets) have no Keychain. Provide
// an in-memory shim so shared code compiles and runs; secrets simply do not
// persist across process launches there, which is acceptable for tests.
enum KeychainStore {
    enum Account: String {
        case crabboxToken = "crabbox.token"
        case isloKey = "islo.apiKey"
    }

    private static var store: [String: String] = [:]

    static func get(_ account: Account) -> String? { store[account.rawValue] }

    @discardableResult
    static func set(_ value: String?, for account: Account) -> Bool {
        let trimmed = value?.trimmingCharacters(in: .whitespacesAndNewlines)
        if let trimmed, !trimmed.isEmpty {
            store[account.rawValue] = trimmed
        } else {
            store[account.rawValue] = nil
        }
        return true
    }

    @discardableResult
    static func delete(_ account: Account) -> Bool {
        store[account.rawValue] = nil
        return true
    }
}

#endif
