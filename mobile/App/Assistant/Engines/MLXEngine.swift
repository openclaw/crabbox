//
//  MLXEngine.swift
//  Crabbox
//
//  On-device LLM engine backed by MLX Swift (Apple-silicon Metal GPU).
//
//  ─────────────────────────────────────────────────────────────────────────
//  PROJECT CONFIGURATION (config agent — add to project.yml):
//
//    packages:
//      mlx-swift-lm:
//        url: https://github.com/ml-explore/mlx-swift-lm
//        from: 3.31.3
//
//    targets.Crabbox.dependencies:
//      - package: mlx-swift-lm
//        product: MLXLLM
//      - package: mlx-swift-lm
//        product: MLXLMCommon
//      - package: mlx-swift-lm
//        product: MLXHuggingFace   # hub/tokenizer loaders used below
//
//  Until the package is linked, the `#if canImport(MLXLLM)` guard keeps the
//  whole app compiling: this engine simply reports `isReady() == false` and
//  throws `.unavailable` from `reply(...)`.
//  ─────────────────────────────────────────────────────────────────────────
//

import Foundation
import CrabboxKit
#if canImport(Metal)
import Metal
#endif

// MLX is only present once the SwiftPM package is added to the target.
#if canImport(MLXLLM) && canImport(MLXLMCommon)
import MLXLLM
import MLXLMCommon
#if canImport(MLXHuggingFace)
import MLXHuggingFace
#endif
#endif

/// On-device chat engine. Runs a small 4-bit quantized model fully offline on
/// the device GPU via MLX. Conforms to CrabboxKit's `LLMEngine` so it is a
/// drop-in alongside the sandbox/Ollama and Apple Foundation Models engines.
///
/// The type is an `actor` because MLX model load + generation are stateful and
/// must be serialized: we hold a single `ModelContainer` and lazily load it on
/// first use, reporting download progress along the way.
actor MLXEngine: LLMEngine {

    // MARK: LLMEngine conformance

    nonisolated let displayName: String
    nonisolated var kind: EngineKind { .onDevice }

    // MARK: Model selection
    //
    // Defaults come straight from the on-device research:
    //   • Default       → Gemma 3 1B QAT 4-bit  (~735 MB, best quality/size)
    //   • Tiny fallback → Qwen2.5 0.5B 4-bit     (~265 MB, low-RAM devices)
    //
    // We choose between them at init time based on available process memory so
    // we never try to load a model the device can't hold.

    /// HF repo id of the model this engine loads.
    nonisolated let modelID: String

    /// Approximate RAM (bytes) below which we degrade to the tiny model.
    /// Gemma 1B comfortably needs well over 1 GB of headroom once KV cache and
    /// working buffers are included; below ~1.6 GB free we pick Qwen 0.5B.
    private static let gemmaMemoryFloor: UInt64 = 1_600 * 1_024 * 1_024

    static let defaultModelID = "mlx-community/gemma-3-1b-it-qat-4bit"
    static let tinyModelID    = "mlx-community/Qwen2.5-0.5B-Instruct-4bit"

    #if canImport(MLXLLM) && canImport(MLXLMCommon)
    /// Lazily-loaded container; `nil` until the first `loadIfNeeded()`.
    private var container: ModelContainer?
    private let configuration: ModelConfiguration
    #endif

    /// 0...1 download/load progress, observable by the UI via `progress`.
    private(set) var downloadProgress: Double = 0

    /// Designated init. Picks the model tier from available memory unless an
    /// explicit `modelID` is supplied.
    init(modelID: String? = nil, displayName: String? = nil) {
        let resolved = modelID ?? MLXEngine.pickModelForDevice()
        self.modelID = resolved
        self.displayName = displayName ?? MLXEngine.label(for: resolved)

        #if canImport(MLXLLM) && canImport(MLXLMCommon)
        // Prefer the registry constant for the default Gemma model (it carries
        // the correct chat template + extra config); fall back to a raw id.
        if resolved == MLXEngine.defaultModelID {
            self.configuration = LLMRegistry.gemma3_1B_qat_4bit
        } else if resolved == MLXEngine.tinyModelID {
            self.configuration = LLMRegistry.qwen205b4bit
        } else {
            self.configuration = ModelConfiguration(id: resolved)
        }
        #endif
    }

    /// Human-readable label derived from a model id.
    private static func label(for id: String) -> String {
        switch id {
        case defaultModelID: return "Gemma 3 1B (4-bit, on-device)"
        case tinyModelID:    return "Qwen2.5 0.5B (4-bit, on-device)"
        default:             return "On-device LLM"
        }
    }

    /// Choose a model tier based on the memory iOS will let us use.
    private static func pickModelForDevice() -> String {
        #if os(iOS) && !targetEnvironment(simulator)
        let available = UInt64(max(0, os_proc_available_memory()))
        return available >= gemmaMemoryFloor ? defaultModelID : tinyModelID
        #else
        return defaultModelID
        #endif
    }

    /// Current load/download progress in 0...1 (exposed for UI binding).
    var progress: Double { downloadProgress }

    // MARK: Readiness

    /// `true` only when MLX can actually run here: a real device with a Metal
    /// GPU and the model loadable. The Simulator has no MLX Metal support, so
    /// we hard-fail there and let `ChatStore` fall back to another engine.
    func isReady() async -> Bool {
        #if targetEnvironment(simulator)
        return false
        #elseif canImport(MLXLLM) && canImport(MLXLMCommon) && os(iOS)
        guard MTLCreateSystemDefaultDevice() != nil else { return false }
        // Probe an actual load — this also primes the container for the first
        // `reply(...)`, so the user doesn't pay the full latency twice.
        return (try? await loadIfNeeded()) != nil
        #else
        // Package not linked (or non-iOS): on-device inference unavailable.
        return false
        #endif
    }

    // MARK: Generation

    func reply(messages: [ChatMessage], options: LLMOptions) async throws -> String {
        #if canImport(MLXLLM) && canImport(MLXLMCommon) && os(iOS) && !targetEnvironment(simulator)
        let container = try await loadIfNeeded()

        // Map CrabboxKit chat history → MLX chat history.
        let history: [Chat.Message] = messages.map { msg in
            switch msg.role {
            case .system:    return .system(msg.content)
            case .assistant: return .assistant(msg.content)
            case .user:      return .user(msg.content)
            }
        }

        // Translate CrabboxKit's LLMOptions into MLX generate parameters.
        // `numPredict` bounds output length (and therefore KV-cache growth,
        // which matters for on-device memory pressure).
        var params = GenerateParameters(temperature: Float(options.temperature))
        if let predict = options.numPredict { params.maxTokens = predict }

        // The last user turn is the prompt; prior turns seed the session.
        let priorHistory = Array(history.dropLast(history.suffix(1).filter { $0.role == .user }.count))
        let lastUser = messages.last(where: { $0.role == .user })?.content ?? ""

        let session = ChatSession(container,
                                  history: priorHistory,
                                  generateParameters: params)

        // Prefer streaming so we could surface tokens incrementally later; for
        // now we accumulate into the final string the protocol returns.
        var output = ""
        for try await chunk in session.streamResponse(to: lastUser) {
            output += chunk
        }
        return output
        #else
        throw LLMError.unavailable("On-device MLX engine is not available on this build/device.")
        #endif
    }

    // MARK: Loading

    #if canImport(MLXLLM) && canImport(MLXLMCommon)
    /// Loads the model container once, reporting progress as it downloads.
    @discardableResult
    private func loadIfNeeded() async throws -> ModelContainer {
        if let container { return container }
        let loaded = try await LLMModelFactory.shared.loadContainer(
            configuration: configuration
        ) { [weak self] progress in
            // progressHandler is non-isolated; hop back onto the actor.
            Task { await self?.setProgress(progress.fractionCompleted) }
        }
        container = loaded
        downloadProgress = 1
        return loaded
    }

    private func setProgress(_ value: Double) { downloadProgress = value }
    #endif
}
