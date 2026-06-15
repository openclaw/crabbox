import Foundation

#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

/// A sandbox as reported by islo.
public struct IsloSandbox: Sendable, Equatable {
    public let name: String
    public let status: String
    public init(name: String, status: String) {
        self.name = name
        self.status = status
    }
}

/// A per-port public HTTPS share produced by `POST /sandboxes/{name}/shares` —
/// this is what the phone connects to when the LLM runs on the sandbox.
public struct IsloShare: Sendable, Equatable {
    public let shareID: String
    public let url: String
    public let port: Int
    public init(shareID: String, url: String, port: Int) {
        self.shareID = shareID
        self.url = url
        self.port = port
    }
}

/// Sizing for a new sandbox; mirrors crabbox's `islo.*` config keys.
public struct IsloSandboxSpec: Sendable {
    public var image: String
    public var vcpus: Int?
    public var memoryMB: Int?
    public var diskGB: Int?
    public init(
        image: String = "docker.io/library/ubuntu:26.04",
        vcpus: Int? = 2,
        memoryMB: Int? = 4096,
        diskGB: Int? = 20
    ) {
        self.image = image
        self.vcpus = vcpus
        self.memoryMB = memoryMB
        self.diskGB = diskGB
    }
}

/// A direct client for the islo sandbox API, mirroring crabbox's own islo
/// provider (`internal/providers/islo`). islo is brokerless: the client talks to
/// `https://api.islo.dev` directly with a bearer API key. The phone stores the
/// key in the Keychain and never sends it anywhere else.
public struct IsloClient: Sendable {
    public let baseURL: URL
    private let apiKey: String
    public let timeout: TimeInterval

    public init?(apiKey: String, baseURL: String = "https://api.islo.dev", timeout: TimeInterval = 120) {
        guard !apiKey.isEmpty, let url = URL(string: baseURL.hasSuffix("/") ? String(baseURL.dropLast()) : baseURL)
        else { return nil }
        self.apiKey = apiKey
        self.baseURL = url
        self.timeout = timeout
    }

    // MARK: - Sandbox lifecycle

    public func createSandbox(name: String, spec: IsloSandboxSpec = IsloSandboxSpec()) async throws -> IsloSandbox {
        var body: [String: Any] = ["name": name, "image": spec.image]
        if let v = spec.vcpus { body["vcpus"] = v }
        if let m = spec.memoryMB { body["memory_mb"] = m }
        if let d = spec.diskGB { body["disk_gb"] = d }
        let data = try await send("POST", "/sandboxes", json: body)
        return try Self.decodeSandbox(data)
    }

    public func getSandbox(name: String) async throws -> IsloSandbox {
        let data = try await send("GET", "/sandboxes/\(escape(name))")
        return try Self.decodeSandbox(data)
    }

    public func listSandboxes() async throws -> [IsloSandbox] {
        let data = try await send("GET", "/sandboxes?limit=100&offset=0")
        guard let obj = try? JSONSerialization.jsonObject(with: data) else { throw LLMError.decode("sandboxes") }
        let rows: [[String: Any]]
        if let arr = obj as? [[String: Any]] { rows = arr }
        else if let dict = obj as? [String: Any], let arr = dict["sandboxes"] as? [[String: Any]] { rows = arr }
        else { rows = [] }
        return rows.compactMap { try? Self.sandbox(from: $0) }
    }

    public func deleteSandbox(name: String) async throws {
        _ = try await send("DELETE", "/sandboxes/\(escape(name))")
    }

    public func pauseSandbox(name: String) async throws {
        _ = try await send("POST", "/sandboxes/\(escape(name))/pause")
    }

    public func resumeSandbox(name: String) async throws {
        _ = try await send("POST", "/sandboxes/\(escape(name))/resume")
    }

    // MARK: - Exec & shares

    /// Runs a shell script in the sandbox and collects its output. Reads the
    /// `POST /exec/stream` Server-Sent Events response to completion.
    public func exec(name: String, script: String) async throws -> ExecResult {
        let body: [String: Any] = ["command": ["bash", "--noprofile", "--norc", "-c", script]]
        let data = try await send("POST", "/sandboxes/\(escape(name))/exec/stream", json: body, accept: "text/event-stream")
        return Self.parseExecStream(data)
    }

    /// Exposes a port as a public HTTPS share (e.g. Ollama's 11434).
    public func createShare(name: String, port: Int, ttlSeconds: Int? = nil) async throws -> IsloShare {
        var body: [String: Any] = ["port": port]
        if let ttl = ttlSeconds { body["ttl_seconds"] = ttl }
        let data = try await send("POST", "/sandboxes/\(escape(name))/shares", json: body)
        guard let dict = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let shareID = dict["share_id"] as? String,
              let url = dict["url"] as? String,
              let port = dict["port"] as? Int
        else { throw LLMError.decode("share") }
        return IsloShare(shareID: shareID, url: url, port: port)
    }

    public func listShares(name: String) async throws -> [IsloShare] {
        let data = try await send("GET", "/sandboxes/\(escape(name))/shares")
        guard let obj = try? JSONSerialization.jsonObject(with: data) else { return [] }
        let rows = (obj as? [[String: Any]]) ?? ((obj as? [String: Any])?["shares"] as? [[String: Any]]) ?? []
        return rows.compactMap {
            guard let id = $0["share_id"] as? String, let url = $0["url"] as? String, let port = $0["port"] as? Int
            else { return nil }
            return IsloShare(shareID: id, url: url, port: port)
        }
    }

    // MARK: - Internals

    private func escape(_ s: String) -> String {
        s.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? s
    }

    private func send(_ method: String, _ path: String, json: [String: Any]? = nil, accept: String = "application/json") async throws -> Data {
        var request = URLRequest(url: baseURL.appendingPathComponent(path.hasPrefix("/") ? String(path.dropFirst()) : path))
        // appendingPathComponent percent-encodes '?'; build the URL directly to keep query strings.
        if let url = URL(string: baseURL.absoluteString + path) { request.url = url }
        request.httpMethod = method
        request.timeoutInterval = timeout
        request.setValue("Bearer \(apiKey)", forHTTPHeaderField: "Authorization")
        request.setValue(accept, forHTTPHeaderField: "Accept")
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

    private static func decodeSandbox(_ data: Data) throws -> IsloSandbox {
        guard let dict = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw LLMError.decode("sandbox")
        }
        return try sandbox(from: dict)
    }

    private static func sandbox(from dict: [String: Any]) throws -> IsloSandbox {
        guard let name = dict["name"] as? String else { throw LLMError.decode("sandbox.name") }
        let status = (dict["status"] as? String) ?? (dict["state"] as? String) ?? "unknown"
        return IsloSandbox(name: name, status: status)
    }

    /// Parses an islo `exec/stream` SSE body collected to completion. Events
    /// carry `data:` JSON lines with `stdout`/`stderr` chunks and a final
    /// `exit_code`.
    static func parseExecStream(_ data: Data) -> ExecResult {
        let text = String(data: data, encoding: .utf8) ?? ""
        var stdout = "", stderr = "", exit = 0
        for rawLine in text.split(separator: "\n", omittingEmptySubsequences: true) {
            let line = rawLine.trimmingCharacters(in: .whitespaces)
            guard line.hasPrefix("data:") else { continue }
            let payload = line.dropFirst("data:".count).trimmingCharacters(in: .whitespaces)
            guard let jsonData = payload.data(using: .utf8),
                  let event = try? JSONSerialization.jsonObject(with: jsonData) as? [String: Any]
            else { continue }
            if let chunk = event["stdout"] as? String { stdout += chunk }
            if let chunk = event["stderr"] as? String { stderr += chunk }
            if let code = event["exit_code"] as? Int { exit = code }
            if let code = event["exitCode"] as? Int { exit = code }
        }
        return ExecResult(exitCode: exit, stdout: stdout, stderr: stderr)
    }
}

/// The script that turns a fresh Ubuntu islo sandbox into an Ollama LLM server
/// the phone can chat with. Pulls a small model and serves on 0.0.0.0:11434.
public func isloOllamaBootstrapScript(model: String) -> String {
    """
    set -euo pipefail
    export DEBIAN_FRONTEND=noninteractive
    SUDO=""; [ "$(id -u)" -ne 0 ] && SUDO="sudo"
    if ! command -v ollama >/dev/null 2>&1; then
      $SUDO apt-get update -y
      $SUDO apt-get install -y curl ca-certificates
      curl -fsSL https://ollama.com/install.sh | $SUDO sh
    fi
    (OLLAMA_HOST=0.0.0.0:11434 OLLAMA_KEEP_ALIVE=-1 ollama serve >/tmp/ollama.log 2>&1 &)
    sleep 5
    ollama pull \(model)
    echo "ready: \(model)"
    """
}
