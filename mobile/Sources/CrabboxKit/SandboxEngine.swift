import Foundation

/// An `LLMEngine` whose model runs remotely on a leased islo Linux sandbox
/// (Ollama serving on a port exposed via an islo share). The phone holds no
/// model; it just chats over HTTPS. This is the "LLM on the sandbox" path.
public struct SandboxEngine: LLMEngine {
    public let client: OllamaClient
    public let model: String
    public let displayName: String
    public var kind: EngineKind { .sandbox }

    public init(client: OllamaClient, model: String, displayName: String? = nil) {
        self.client = client
        self.model = model
        self.displayName = displayName ?? "Sandbox · \(model)"
    }

    /// Convenience for pointing at an islo share URL (or any Ollama endpoint).
    public init?(endpoint: String, model: String) {
        self.init(endpoint: endpoint, model: model, displayName: nil)
    }

    public init?(endpoint: String, model: String, displayName: String?) {
        guard let client = OllamaClient(host: endpoint) else { return nil }
        self.init(client: client, model: model, displayName: displayName)
    }

    public func isReady() async -> Bool {
        guard await client.isReachable() else { return false }
        guard let tags = try? await client.tags() else { return false }
        // Accept an exact match or the same base model with any tag.
        let base = model.split(separator: ":").first.map(String.init) ?? model
        return tags.contains(model) || tags.contains { $0.hasPrefix(base) }
    }

    public func reply(messages: [ChatMessage], options: LLMOptions) async throws -> String {
        try await client.chat(model: model, messages: messages, options: options)
    }
}
