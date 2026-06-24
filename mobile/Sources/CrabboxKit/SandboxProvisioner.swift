import Foundation

/// Result of running a command in a sandbox (any provider).
public struct ExecResult: Sendable {
    public let exitCode: Int
    public let stdout: String
    public let stderr: String
    public init(exitCode: Int, stdout: String, stderr: String) {
        self.exitCode = exitCode
        self.stdout = stdout
        self.stderr = stderr
    }
}

/// A leased sandbox, provider-agnostic.
public struct SandboxHandle: Sendable, Equatable {
    public let id: String
    public let provider: String
    public var status: String
    /// Public Ollama endpoint once exposed by a sandbox-capable provider.
    /// `nil` until `expose` is called.
    public var ollamaEndpoint: String?
    public init(id: String, provider: String, status: String = "unknown", ollamaEndpoint: String? = nil) {
        self.id = id
        self.provider = provider
        self.status = status
        self.ollamaEndpoint = ollamaEndpoint
    }
}

/// The seam that lets the app provision a sandbox without caring which provider
/// is behind it. crabbox.sh credentials cover portal/workspace flows; islo.dev
/// is an optional direct sandbox provider the user can enable and save a key for.
///
/// Extended with pause/resume for direct control of remote sandboxes (primarily
/// supported by islo.dev). Coordinator may report unavailable.
public protocol SandboxProvisioner: Sendable {
    var providerName: String { get }
    func launch(name: String, model: String) async throws -> SandboxHandle
    func list() async throws -> [SandboxHandle]
    func stop(id: String) async throws
    func pause(id: String) async throws
    func resume(id: String) async throws
}

// MARK: - islo (direct) provisioner

/// Optional direct-to-islo provisioner. islo is the one brokerless crabbox
/// provider, so it cannot be driven through crabbox.sh; this talks to
/// api.islo.dev directly with a saved key.
public struct IsloProvisioner: SandboxProvisioner {
    public let providerName = "islo.dev"
    private let client: IsloClient
    private let spec: IsloSandboxSpec

    public init(client: IsloClient, spec: IsloSandboxSpec = IsloSandboxSpec()) {
        self.client = client
        self.spec = spec
    }

    public init?(apiKey: String, baseURL: String = "https://api.islo.dev", spec: IsloSandboxSpec = IsloSandboxSpec()) {
        guard let client = IsloClient(apiKey: apiKey, baseURL: baseURL) else { return nil }
        self.init(client: client, spec: spec)
    }

    public func launch(name: String, model: String) async throws -> SandboxHandle {
        let sandbox = try await client.createSandbox(name: name, spec: spec)
        // Launch the (multi-minute) Ollama install + model pull DETACHED so the
        // exec returns immediately — islo's exec/stream has a max duration. The
        // detached job persists; the returned engine's isReady()/first chat polls
        // until it's up.
        _ = try await client.exec(name: sandbox.name, script: isloDetachedLaunch(script: isloOllamaBootstrapScript(model: model)))
        let share = try await client.createShare(name: sandbox.name, port: 11434)
        return SandboxHandle(id: sandbox.name, provider: providerName, status: sandbox.status, ollamaEndpoint: share.url)
    }

    public func list() async throws -> [SandboxHandle] {
        try await client.listSandboxes().map { SandboxHandle(id: $0.name, provider: providerName, status: $0.status) }
    }

    public func stop(id: String) async throws {
        try await client.deleteSandbox(name: id)
    }

    public func pause(id: String) async throws {
        try await client.pauseSandbox(name: id)
    }

    public func resume(id: String) async throws {
        try await client.resumeSandbox(name: id)
    }
}

// MARK: - crabbox.sh (coordinator) provisioner — portal/workspace

/// The coordinator provisioner: holds a crabbox.sh session token only. Coordinator
/// workspace APIs are supported by `CoordinatorClient`; sandbox lifecycle is
/// intentionally fail-closed until crabbox.sh exposes a supported endpoint for it.
public struct CoordinatorProvisioner: SandboxProvisioner {
    public let providerName = "crabbox.sh"
    private let client: CoordinatorClient

    public init(client: CoordinatorClient) { self.client = client }

    public init?(coordinatorURL: String, token: String) {
        guard let client = CoordinatorClient(coordinatorURL: coordinatorURL, token: token) else { return nil }
        self.init(client: client)
    }

    public func launch(name: String, model: String) async throws -> SandboxHandle {
        throw LLMError.unavailable("crabbox.sh sandbox lifecycle is not supported by the current coordinator API; use workspaces or enable islo.dev direct")
    }

    public func list() async throws -> [SandboxHandle] {
        throw LLMError.unavailable("crabbox.sh sandbox lifecycle is not supported by the current coordinator API; use workspaces or enable islo.dev direct")
    }

    public func stop(id: String) async throws {
        throw LLMError.unavailable("crabbox.sh sandbox lifecycle is not supported by the current coordinator API; use workspaces or enable islo.dev direct")
    }

    public func pause(id: String) async throws {
        throw LLMError.unavailable("pause/resume is not supported by the current coordinator API; use islo.dev direct for full control")
    }

    public func resume(id: String) async throws {
        throw LLMError.unavailable("pause/resume is not supported by the current coordinator API; use islo.dev direct for full control")
    }
}

/// Provisions a sandbox running Ollama and returns a ready-to-chat engine. This
/// is the one call the "Sandboxes" tab makes for the demo, regardless of
/// provider.
public func launchLLMSandbox(
    provisioner: SandboxProvisioner,
    name: String,
    model: String
) async throws -> (handle: SandboxHandle, engine: SandboxEngine) {
    let handle = try await provisioner.launch(name: name, model: model)
    guard let endpoint = handle.ollamaEndpoint, let engine = SandboxEngine(endpoint: endpoint, model: model) else {
        throw LLMError.unavailable("sandbox \(handle.id) did not expose an Ollama endpoint")
    }
    return (handle, engine)
}

public func waitForEngineReady(
    _ engine: any LLMEngine,
    attempts: Int = 90,
    sleep: @Sendable () async -> Void
) async -> Bool {
    guard attempts > 0 else { return await engine.isReady() }
    for _ in 0..<attempts {
        if await engine.isReady() { return true }
        await sleep()
    }
    return await engine.isReady()
}
