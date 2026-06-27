import Foundation

#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

/// A client for the crabbox coordinator (crabbox.sh) `/v1` HTTP API — the
/// managed control plane that owns provider credentials and lease state. The
/// phone authenticates with a crabbox session token (the same identity used by
/// the portal) and never holds raw provider keys.
///
/// Workspace lifecycle uses the supported `/v1/workspaces` surface. Managed LLM
/// sandbox lifecycle is intentionally not implemented here until the coordinator
/// exposes a supported endpoint.
public struct CoordinatorClient: Sendable {
    public let baseURL: URL
    private let token: String
    public let timeout: TimeInterval

    public init?(coordinatorURL: String = "https://crabbox.sh", token: String, timeout: TimeInterval = 120) {
        guard !token.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
              let normalized = normalizeCoordinatorURL(coordinatorURL),
              let url = URL(string: normalized)
        else { return nil }
        self.baseURL = url
        self.token = token.trimmingCharacters(in: .whitespacesAndNewlines)
        self.timeout = timeout
    }

    public func health() async -> Bool {
        (try? await send("GET", "/v1/health")) != nil
    }

    /// Creates a coordinator-managed workspace. This is the native-app analogue
    /// of starting a `crabbox` command session: the coordinator owns the machine,
    /// clones the requested GitHub repo when provided, and exposes a terminal
    /// websocket once the workspace is ready.
    public func createWorkspace(_ request: WorkspaceCreateRequest) async throws -> WorkspaceSession {
        guard isValidWorkspaceID(request.id) else {
            throw LLMError.unavailable("invalid workspace id")
        }
        var body: [String: Any] = [
            "id": request.id,
            "runtime": "crabbox",
            "command": request.command,
            "profile": request.profile,
            "ttlSeconds": request.ttlSeconds,
            "idleTimeoutSeconds": request.idleTimeoutSeconds,
            "capabilities": ["desktop": false],
        ]
        if !request.repo.isEmpty { body["repo"] = request.repo }
        if !request.branch.isEmpty { body["branch"] = request.branch }
        let data = try await send("POST", "/v1/workspaces", json: body, timeout: 30)
        return try Self.decodeWorkspace(data)
    }

    public func getWorkspace(id: String) async throws -> WorkspaceSession {
        guard isValidWorkspaceID(id) else {
            throw LLMError.unavailable("invalid workspace id")
        }
        let escaped = id.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? id
        let data = try await send("GET", "/v1/workspaces/\(escaped)", timeout: 30)
        return try Self.decodeWorkspace(data)
    }

    public func deleteWorkspace(id: String) async throws -> WorkspaceSession {
        guard isValidWorkspaceID(id) else {
            throw LLMError.unavailable("invalid workspace id")
        }
        let escaped = id.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? id
        let data = try await send("DELETE", "/v1/workspaces/\(escaped)", timeout: 30)
        return try Self.decodeWorkspace(data)
    }

    private static func decodeWorkspace(_ data: Data) throws -> WorkspaceSession {
        guard let dict = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw LLMError.decode("workspace response")
        }
        guard let id = (dict["id"] as? String) ?? (dict["workspaceId"] as? String) else {
            throw LLMError.decode("workspace id")
        }
        let status = (dict["status"] as? String) ?? "unknown"
        return WorkspaceSession(
            id: id,
            status: status,
            attachURL: dict["attachUrl"] as? String,
            message: (dict["message"] as? String) ?? status,
            expiresAt: dict["expiresAt"] as? String
        )
    }

    private func send(_ method: String, _ path: String, json: [String: Any]? = nil, timeout: TimeInterval? = nil) async throws -> Data {
        guard let url = URL(string: baseURL.absoluteString + path) else { throw LLMError.invalidResponse }
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.timeoutInterval = timeout ?? self.timeout
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let json {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: json)
        }
        return try await withCheckedThrowingContinuation { continuation in
            URLSession.shared.dataTask(with: request) { data, response, error in
                if let error { continuation.resume(throwing: error); return }
                if let http = response as? HTTPURLResponse, !(200...299).contains(http.statusCode) {
                    continuation.resume(throwing: LLMError.http(http.statusCode)); return
                }
                continuation.resume(returning: data ?? Data())
            }.resume()
        }
    }
}
