import Foundation

/// Request body for the coordinator `/v1/workspaces` API.
public struct WorkspaceCreateRequest: Sendable, Equatable {
    public let id: String
    public let repo: String
    public let branch: String
    public let command: String
    public let profile: String
    public let ttlSeconds: Int
    public let idleTimeoutSeconds: Int

    public init(
        id: String,
        repo: String = "",
        branch: String = "main",
        command: String,
        profile: String = "default",
        ttlSeconds: Int = 14_400,
        idleTimeoutSeconds: Int = 1_800
    ) {
        self.id = id
        self.repo = repo.trimmingCharacters(in: .whitespacesAndNewlines)
        self.branch = branch.trimmingCharacters(in: .whitespacesAndNewlines)
        self.command = command
        self.profile = profile.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? "default" : profile.trimmingCharacters(in: .whitespacesAndNewlines)
        self.ttlSeconds = ttlSeconds
        self.idleTimeoutSeconds = idleTimeoutSeconds
    }
}

/// Coordinator workspace status returned by `/v1/workspaces/{id}`.
public struct WorkspaceSession: Sendable, Equatable, Identifiable {
    public let id: String
    public let status: String
    public let attachURL: String?
    public let message: String
    public let expiresAt: String?

    public init(id: String, status: String, attachURL: String?, message: String, expiresAt: String?) {
        self.id = id
        self.status = status
        self.attachURL = attachURL
        self.message = message
        self.expiresAt = expiresAt
    }

    public var isReady: Bool { status == "ready" }
    public var isTerminal: Bool { ["failed", "expired", "stopped"].contains(status) }
}

/// Marker printed by `crabboxWorkspaceCommand` so a terminal UI can recover the
/// command exit status from a websocket stream.
public let crabboxWorkspaceExitMarker = "[crabbox exit "

/// Wraps a shell command so the terminal stream includes a deterministic exit
/// marker even when the user command calls `exit`.
public func crabboxWorkspaceCommand(_ command: String) -> String {
    let trimmed = command.trimmingCharacters(in: .whitespacesAndNewlines)
    let body = trimmed.isEmpty ? "pwd" : trimmed
    return """
    (
    \(body)
    )
    status=$?
    printf '\\n\(crabboxWorkspaceExitMarker)%s]\\n' "$status"
    exit "$status"
    """
}

/// Mirrors the worker's `validWorkspaceID` rule: lowercase DNS-style, max 63.
public func isValidWorkspaceID(_ value: String) -> Bool {
    guard !value.isEmpty, value.count <= 63 else { return false }
    guard let first = value.utf8.first, isLowercaseAlphaNumeric(first),
          let last = value.utf8.last, isLowercaseAlphaNumeric(last) else {
        return false
    }
    return value.utf8.allSatisfy { byte in
        isLowercaseAlphaNumeric(byte) || byte == UInt8(ascii: "-")
    }
}

private func isLowercaseAlphaNumeric(_ byte: UInt8) -> Bool {
    (byte >= UInt8(ascii: "a") && byte <= UInt8(ascii: "z")) ||
        (byte >= UInt8(ascii: "0") && byte <= UInt8(ascii: "9"))
}
