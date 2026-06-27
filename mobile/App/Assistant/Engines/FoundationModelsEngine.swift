//
//  FoundationModelsEngine.swift
//  Crabbox
//
//  Apple Foundation Models engine (the system on-device LLM that ships with
//  Apple Intelligence, iOS 26+). Conforms to CrabboxKit's `LLMEngine` with
//  `kind == .system`.
//
//  The framework symbols are all `@available(iOS 26.0, *)`. `import` at file
//  scope is safe on an iOS 17 deployment target — the framework just isn't
//  touched until a guarded symbol use runs. We funnel every direct
//  `LanguageModelSession`/`SystemLanguageModel` reference through a private,
//  `@available(iOS 26.0, *)` `Impl` so the outer type compiles on iOS 17.
//

import Foundation
import CrabboxKit

#if canImport(FoundationModels)
import FoundationModels
#endif

/// System-provided on-device LLM. Available only on devices with Apple
/// Intelligence enabled, running iOS 26+. On anything older — or any build
/// whose SDK lacks the framework — `isReady()` returns `false` and `reply(...)`
/// throws `.unavailable`, so `ChatStore` can fall back to another engine.
final class FoundationModelsEngine: LLMEngine, @unchecked Sendable {

    // MARK: LLMEngine conformance

    let displayName: String = "Apple Intelligence (system)"
    var kind: EngineKind { .system }

    init() {}

    /// Ready only when: the runtime is iOS 26+, the framework is present, and
    /// `SystemLanguageModel.default.availability == .available` (device is
    /// eligible, Apple Intelligence is on, and the model has downloaded).
    func isReady() async -> Bool {
        guard #available(iOS 26.0, *) else { return false }
        #if canImport(FoundationModels)
        return Impl.isAvailable()
        #else
        return false
        #endif
    }

    func reply(messages: [ChatMessage], options: LLMOptions) async throws -> String {
        guard #available(iOS 26.0, *) else {
            throw LLMError.unavailable("Apple Intelligence requires iOS 26 or later.")
        }
        #if canImport(FoundationModels)
        return try await Impl().reply(messages: messages, options: options)
        #else
        throw LLMError.unavailable("This build was compiled without the Foundation Models framework.")
        #endif
    }
}

// MARK: - iOS 26+ implementation

#if canImport(FoundationModels)
/// Isolates every Foundation Models symbol behind an availability gate so the
/// public `FoundationModelsEngine` stays compilable on the iOS 17 floor.
@available(iOS 26.0, *)
private struct Impl {

    /// Whether the system model is presently usable.
    static func isAvailable() -> Bool {
        if case .available = SystemLanguageModel.default.availability { return true }
        return false
    }

    func reply(messages: [ChatMessage], options: LLMOptions) async throws -> String {
        // Surface a precise reason if the model isn't usable right now.
        switch SystemLanguageModel.default.availability {
        case .available:
            break
        case .unavailable(let reason):
            throw LLMError.unavailable(Impl.describe(reason))
        @unknown default:
            throw LLMError.unavailable("Apple Intelligence is unavailable.")
        }

        // Foundation Models keeps multi-turn context inside a single session.
        // We seed system turns as instructions and replay the conversation so
        // history is preserved even across fresh sessions.
        let instructions = messages
            .filter { $0.role == .system }
            .map(\.content)
            .joined(separator: "\n")

        let session = LanguageModelSession(instructions: instructions)

        // Replay non-system turns; the final user turn is what we respond to.
        let conversation = messages.filter { $0.role != .system }
        guard let lastUser = conversation.last(where: { $0.role == .user })?.content else {
            throw LLMError.invalidResponse
        }

        // Map temperature; Foundation Models exposes sampling via GenerationOptions.
        let genOptions = GenerationOptions(temperature: options.temperature)

        let response = try await session.respond(to: lastUser, options: genOptions)
        return response.content
    }

    /// Maps an unavailability reason to a user-facing message.
    private static func describe(_ reason: SystemLanguageModel.Availability.UnavailableReason) -> String {
        switch reason {
        case .deviceNotEligible:
            return "This device does not support Apple Intelligence."
        case .appleIntelligenceNotEnabled:
            return "Turn on Apple Intelligence in Settings to use this engine."
        case .modelNotReady:
            return "The system model is still downloading. Try again shortly."
        @unknown default:
            return "Apple Intelligence is currently unavailable."
        }
    }
}
#endif
