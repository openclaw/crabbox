import Foundation

/// The mobile app has two credential surfaces, but only one currently supports
/// sandbox lifecycle. Keep that policy in CrabboxKit so the SwiftUI settings UI
/// and portable tests cannot drift.
public enum SandboxProviderSelection: Equatable, Sendable {
    case isloDirect
    case workspaceOnly
    case none
}

public func selectSandboxProvider(
    isloEnabled: Bool,
    isloKey: String?,
    crabboxToken: String?
) -> SandboxProviderSelection {
    if isloEnabled && hasNonEmptySecret(isloKey) {
        return .isloDirect
    }
    if hasNonEmptySecret(crabboxToken) {
        return .workspaceOnly
    }
    return .none
}

public func sandboxProviderLabel(for selection: SandboxProviderSelection) -> String {
    switch selection {
    case .isloDirect:
        return "islo.dev (direct)"
    case .workspaceOnly:
        return "crabbox.sh (workspace only)"
    case .none:
        return "No sandbox provider configured"
    }
}

private func hasNonEmptySecret(_ value: String?) -> Bool {
    value?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false
}
