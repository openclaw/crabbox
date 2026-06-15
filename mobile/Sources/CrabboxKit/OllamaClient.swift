import Foundation

#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

/// A tiny, portable client for the Ollama HTTP API (`/api/chat`, `/api/tags`).
/// Used both by the e2e test driver and by `SandboxEngine` (the model running on
/// a leased islo box). Uses a continuation-wrapped `dataTask` so it works the
/// same on macOS, iOS, and Linux.
public struct OllamaClient: Sendable {
    public let baseURL: URL
    public let timeout: TimeInterval

    public init(baseURL: URL, timeout: TimeInterval = 60) {
        self.baseURL = baseURL
        self.timeout = timeout
    }

    public init?(host: String, timeout: TimeInterval = 60) {
        guard let url = URL(string: host) else { return nil }
        self.init(baseURL: url, timeout: timeout)
    }

    /// Non-streaming chat completion. `format` is an optional JSON Schema passed
    /// through to Ollama's grammar-constrained structured output.
    public func chat(
        model: String,
        messages: [ChatMessage],
        options: LLMOptions = .deterministic,
        format: [String: Any]? = nil
    ) async throws -> String {
        var optionDict: [String: Any] = ["temperature": options.temperature]
        if let seed = options.seed { optionDict["seed"] = seed }
        if let numCtx = options.numCtx { optionDict["num_ctx"] = numCtx }
        if let numPredict = options.numPredict { optionDict["num_predict"] = numPredict }

        var body: [String: Any] = [
            "model": model,
            "stream": false,
            "keep_alive": -1,
            "options": optionDict,
            "messages": messages.map { ["role": $0.role.rawValue, "content": $0.content] },
        ]
        if let format { body["format"] = format }

        let data = try await post(path: "/api/chat", body: body)
        guard let envelope = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let message = envelope["message"] as? [String: Any],
              let content = message["content"] as? String
        else { throw LLMError.decode("unexpected /api/chat response") }
        return content
    }

    /// Lists locally available model tags.
    public func tags() async throws -> [String] {
        let data = try await get(path: "/api/tags")
        guard let envelope = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let models = envelope["models"] as? [[String: Any]]
        else { throw LLMError.decode("unexpected /api/tags response") }
        return models.compactMap { $0["name"] as? String }
    }

    public func isReachable() async -> Bool {
        (try? await tags()) != nil
    }

    // MARK: - Transport

    private func post(path: String, body: [String: Any]) async throws -> Data {
        let payload = try JSONSerialization.data(withJSONObject: body)
        var request = URLRequest(url: baseURL.appendingPathComponent(path))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = payload
        request.timeoutInterval = timeout
        return try await send(request)
    }

    private func get(path: String) async throws -> Data {
        var request = URLRequest(url: baseURL.appendingPathComponent(path))
        request.httpMethod = "GET"
        request.timeoutInterval = timeout
        return try await send(request)
    }

    private func send(_ request: URLRequest) async throws -> Data {
        try await withCheckedThrowingContinuation { continuation in
            URLSession.shared.dataTask(with: request) { data, response, error in
                if let error { continuation.resume(throwing: error); return }
                if let http = response as? HTTPURLResponse, !(200...299).contains(http.statusCode) {
                    continuation.resume(throwing: LLMError.http(http.statusCode)); return
                }
                guard let data else { continuation.resume(throwing: LLMError.invalidResponse); return }
                continuation.resume(returning: data)
            }.resume()
        }
    }
}
