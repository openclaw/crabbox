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
/// `https://api.islo.dev` directly.
///
/// Auth is a two-step exchange (matching the islo Go SDK's `customauth`): the
/// long-lived API key (`access_key`) is POSTed to `/auth/token` for a short-lived
/// session JWT, which is then sent as `Authorization: Bearer <jwt>` on every API
/// call and cached until shortly before it expires. The phone stores only the API
/// key (in the Keychain) and never sends it anywhere but `/auth/token`.
public actor IsloClient {
    public let baseURL: URL
    private let apiKey: String
    public let timeout: TimeInterval

    private var cachedToken: String?
    private var tokenExpiry: Date?

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
        if let arr = obj as? [[String: Any]] {
            rows = arr
        } else if let dict = obj as? [String: Any] {
            // islo returns {"items":[…]}; tolerate {"sandboxes":[…]} too.
            rows = (dict["items"] as? [[String: Any]]) ?? (dict["sandboxes"] as? [[String: Any]]) ?? []
        } else {
            rows = []
        }
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

    /// Exchanges the API key for a session JWT (`POST /auth/token` with
    /// `{access_key}` → `{session_token, expires_in}`), caching it until shortly
    /// before expiry. Mirrors the islo Go SDK's `customauth` provider.
    private func authToken() async throws -> String {
        if let token = cachedToken, let expiry = tokenExpiry, Date() < expiry {
            return token
        }
        let data = try await rawSend("POST", "/auth/token", json: ["access_key": apiKey], bearer: nil)
        guard let dict = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let token = dict["session_token"] as? String, !token.isEmpty
        else { throw LLMError.decode("auth/token missing session_token") }
        let ttl = (dict["expires_in"] as? Double) ?? (dict["cookie_max_age"] as? Double) ?? 600
        cachedToken = token
        tokenExpiry = Date().addingTimeInterval(max(0, ttl - 30)) // refresh margin
        return token
    }

    private func send(_ method: String, _ path: String, json: [String: Any]? = nil, accept: String = "application/json") async throws -> Data {
        let token = try await authToken()
        return try await rawSend(method, path, json: json, accept: accept, bearer: token)
    }

    /// One HTTP round-trip. `bearer == nil` is used only by the `/auth/token`
    /// exchange itself (which authenticates with the body, not a header).
    private func rawSend(_ method: String, _ path: String, json: [String: Any]?, accept: String = "application/json", bearer: String?) async throws -> Data {
        guard let url = URL(string: baseURL.absoluteString + path) else { throw LLMError.invalidResponse }
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.timeoutInterval = timeout
        request.setValue(accept, forHTTPHeaderField: "Accept")
        if let bearer { request.setValue("Bearer \(bearer)", forHTTPHeaderField: "Authorization") }
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
        // islo identifies sandboxes by `name`; fall back to `id` if name is absent.
        guard let name = (dict["name"] as? String) ?? (dict["id"] as? String) else {
            throw LLMError.decode("sandbox.name")
        }
        let status = (dict["status"] as? String) ?? (dict["state"] as? String) ?? "unknown"
        return IsloSandbox(name: name, status: status)
    }

    /// Parses an islo `exec/stream` SSE body collected to completion. Events
    /// uses named SSE events: `event: stdout|stderr|exit|error` followed by one
    /// or more `data:` lines, blocks separated by a blank line (matching the islo
    /// Go SDK's `parseIsloSSE`).
    static func parseExecStream(_ data: Data) -> ExecResult {
        let text = String(data: data, encoding: .utf8) ?? ""
        var stdout = "", stderr = "", exit = 0
        var event = ""
        var dataLines: [String] = []

        func flush() {
            guard !event.isEmpty || !dataLines.isEmpty else { return }
            let payload = dataLines.joined(separator: "\n")
            switch event {
            case "stdout": stdout += payload
            case "stderr": stderr += payload
            case "exit": exit = Int(payload.trimmingCharacters(in: .whitespaces)) ?? exit
            default: break
            }
            event = ""
            dataLines.removeAll()
        }

        for line in text.components(separatedBy: "\n") {
            if line.isEmpty { flush(); continue }
            if line.hasPrefix(":") { continue }
            let field: String, value: String
            if let r = line.range(of: ":") {
                field = String(line[..<r.lowerBound])
                var v = String(line[r.upperBound...])
                if v.hasPrefix(" ") { v.removeFirst() }
                value = v
            } else {
                field = line
                value = ""
            }
            if field == "event" { event = value }
            else if field == "data" { dataLines.append(value) }
        }
        flush()
        return ExecResult(exitCode: exit, stdout: stdout, stderr: stderr)
    }
}

/// The script that turns a fresh Ubuntu islo sandbox into an Ollama LLM server
/// the phone can chat with. Pulls a small model and serves on 0.0.0.0:11434.
public func isloOllamaBootstrapScript(model: String) -> String {
    // Deliberately defensive: NO `set` flags and `|| true` on the installer, so
    // the Ollama install script (which exits non-zero trying to reach systemd in
    // a non-systemd sandbox) can't abort the serve/pull that follow. islo
    // sandboxes run as root with no systemd; detached processes persist across
    // exec calls, so we start Ollama bound to 0.0.0.0 with nohup+setsid.
    """
    export DEBIAN_FRONTEND=noninteractive
    if ! command -v ollama >/dev/null 2>&1; then
      apt-get update -y || true
      apt-get install -y curl ca-certificates procps zstd || true
      curl -fsSL https://ollama.com/install.sh | sh || true
    fi

    pkill -f 'ollama serve' 2>/dev/null || true
    nohup setsid env OLLAMA_HOST=0.0.0.0:11434 OLLAMA_KEEP_ALIVE=-1 ollama serve >/tmp/ollama.log 2>&1 </dev/null &
    disown 2>/dev/null || true

    for i in $(seq 1 60); do
      if curl -sf http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then break; fi
      sleep 1
    done

    ollama pull \(model) || true
    echo "BOOTSTRAP_DONE model=\(model) tags=$(curl -s http://127.0.0.1:11434/api/tags | head -c 120)"
    """
}

/// Wraps a long-running script so it runs fully detached in the sandbox and the
/// `exec` call returns immediately. islo's `/exec/stream` has a max duration, so
/// a multi-minute bootstrap (apt + Ollama install + model pull) must NOT run in
/// the foreground of an exec — it gets SIGTERM'd. We base64 the script (avoiding
/// all quoting issues), decode it to a file, and launch it with nohup+setsid.
/// Readiness is then polled separately (the detached job persists across execs).
public func isloDetachedLaunch(script: String) -> String {
    let b64 = Data(script.utf8).base64EncodedString()
    return """
    echo \(b64) | base64 -d > /tmp/crabbox-boot.sh
    nohup setsid bash /tmp/crabbox-boot.sh >/tmp/crabbox-boot.log 2>&1 </dev/null &
    disown 2>/dev/null || true
    echo LAUNCHED pid=$!
    """
}
