import Foundation

/// Where an `LLMEngine` runs. Surfaced in the UI so the user always knows
/// whether inference is private/on-device or remote on a leased sandbox.
public enum EngineKind: String, Sendable {
    case onDevice   // MLX / Apple Foundation Models — fully offline & private
    case sandbox    // Ollama on a leased islo Linux box
    case system     // Apple Foundation Models (built-in)
}

public enum ChatRole: String, Codable, Sendable {
    case system, user, assistant
}

public struct ChatMessage: Codable, Equatable, Sendable {
    public let role: ChatRole
    public let content: String
    public init(role: ChatRole, content: String) {
        self.role = role
        self.content = content
    }
}

/// Decoding/generation knobs common across engines. Defaults are tuned for
/// short, deterministic test-driver replies; the chat UI relaxes them.
public struct LLMOptions: Sendable {
    public var temperature: Double
    public var seed: Int?
    public var numCtx: Int?
    public var numPredict: Int?
    public init(temperature: Double = 0.7, seed: Int? = nil, numCtx: Int? = nil, numPredict: Int? = nil) {
        self.temperature = temperature
        self.seed = seed
        self.numCtx = numCtx
        self.numPredict = numPredict
    }
    public static let deterministic = LLMOptions(temperature: 0, seed: 0, numCtx: 1024, numPredict: 256)
}

public enum LLMError: Error, CustomStringConvertible {
    case unavailable(String)
    case http(Int)
    case decode(String)
    case invalidResponse

    public var description: String {
        switch self {
        case let .unavailable(m): return "engine unavailable: \(m)"
        case let .http(code): return "HTTP \(code)"
        case let .decode(m): return "decode error: \(m)"
        case .invalidResponse: return "invalid response"
        }
    }
}

/// The "simple harness" seam: one async call that turns a conversation into a
/// reply. On-device (MLX / Foundation Models) and remote (islo + Ollama)
/// engines implement this identically, so the chat UI is engine-agnostic.
public protocol LLMEngine: Sendable {
    var displayName: String { get }
    var kind: EngineKind { get }
    /// Whether this engine can serve requests right now (model loaded, sandbox
    /// reachable, device capable).
    func isReady() async -> Bool
    func reply(messages: [ChatMessage], options: LLMOptions) async throws -> String
}
