import Foundation

#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

/// A client for the crabbox coordinator (crabbox.sh) `/v1` HTTP API — the
/// managed control plane that owns provider credentials and lease state. The
/// phone authenticates with a crabbox session token (the same identity used by
/// the portal) and never holds raw provider keys.
///
/// NOTE: the exact `/v1` request/response shapes for managed-sandbox creation
/// are confirmed against the deployed coordinator at integration time; the paths
/// here mirror the worker surface (`/v1/sandboxes`, `/v1/runs`, `/v1/health`).
/// The provider-agnostic chat engine does not depend on these specifics.
public struct CoordinatorClient: Sendable {
    public let baseURL: URL
    private let token: String
    public let timeout: TimeInterval

    public init?(coordinatorURL: String = "https://crabbox.sh", token: String, timeout: TimeInterval = 120) {
        let trimmed = coordinatorURL.hasSuffix("/") ? String(coordinatorURL.dropLast()) : coordinatorURL
        guard !token.isEmpty, let url = URL(string: trimmed) else { return nil }
        self.baseURL = url
        self.token = token
        self.timeout = timeout
    }

    public func health() async -> Bool {
        (try? await send("GET", "/v1/health")) != nil
    }

    /// Asks the coordinator to lease a managed sandbox running Ollama with the
    /// requested model and return an endpoint the phone can chat with.
    public func launchLLMSandbox(name: String, model: String) async throws -> SandboxHandle {
        let body: [String: Any] = [
            "name": name,
            "image": "docker.io/library/ubuntu:26.04",
            "setup": isloOllamaBootstrapScript(model: model),
            "expose": [["port": 11434, "name": "ollama"]],
        ]
        let data = try await send("POST", "/v1/sandboxes", json: body)
        guard let dict = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw LLMError.decode("coordinator sandbox response")
        }
        let id = (dict["id"] as? String) ?? (dict["name"] as? String) ?? name
        let status = (dict["status"] as? String) ?? "starting"
        let endpoint = Self.endpoint(from: dict)
        return SandboxHandle(id: id, provider: "crabbox.sh", status: status, ollamaEndpoint: endpoint)
    }

    public func listSandboxes() async throws -> [SandboxHandle] {
        let data = try await send("GET", "/v1/sandboxes")
        guard let obj = try? JSONSerialization.jsonObject(with: data) else { return [] }
        let rows = (obj as? [[String: Any]]) ?? ((obj as? [String: Any])?["sandboxes"] as? [[String: Any]]) ?? []
        return rows.map {
            SandboxHandle(
                id: ($0["id"] as? String) ?? ($0["name"] as? String) ?? "?",
                provider: "crabbox.sh",
                status: ($0["status"] as? String) ?? "unknown",
                ollamaEndpoint: Self.endpoint(from: $0)
            )
        }
    }

    public func stopSandbox(id: String) async throws {
        _ = try await send("DELETE", "/v1/sandboxes/\(id.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? id)")
    }

    private static func endpoint(from dict: [String: Any]) -> String? {
        if let url = dict["url"] as? String { return url }
        if let shares = dict["shares"] as? [[String: Any]],
           let ollama = shares.first(where: { ($0["name"] as? String) == "ollama" || ($0["port"] as? Int) == 11434 }),
           let url = ollama["url"] as? String {
            return url
        }
        return nil
    }

    private func send(_ method: String, _ path: String, json: [String: Any]? = nil) async throws -> Data {
        guard let url = URL(string: baseURL.absoluteString + path) else { throw LLMError.invalidResponse }
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.timeoutInterval = timeout
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
