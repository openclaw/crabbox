import CrabboxKit
import Foundation

/// Dependency-free unit vectors for the pure logic, mirroring the XCTest cases
/// so the full suite is verifiable with `swift run crabbox-sim` even on a
/// machine without Xcode (where `swift test`/XCTest is unavailable). CI runs the
/// XCTest target; this keeps local proof honest.
public func runUnitChecks() async -> [String] {
    var v: [String] = []
    func eq<T: Equatable>(_ a: T, _ b: T, _ label: String) {
        if a != b { v.append("\(label): expected \(b), got \(a)") }
    }
    func isTrue(_ cond: Bool, _ label: String) { if !cond { v.append(label) } }

    // coordinatorURL
    eq(normalizeCoordinatorURL("crabbox.sh/team?token=redacted#section"), "https://crabbox.sh/team", "normalize bare https")
    eq(normalizeCoordinatorURL("https://broker.example.com////"), "https://broker.example.com", "trim trailing slashes")
    eq(normalizeCoordinatorURL("http://broker.example.com"), nil, "reject prod http")
    eq(normalizeCoordinatorURL("http://localhost:8787"), nil, "reject localhost http in prod")
    eq(normalizeCoordinatorURL("http://localhost:8787", allowLocalHTTP: true), "http://localhost:8787", "allow localhost http in dev")
    eq(normalizeCoordinatorURL("http://127.0.0.1:8787", allowLocalHTTP: true), "http://127.0.0.1:8787", "allow ipv4 loopback dev")
    eq(normalizeCoordinatorURL("http://127.999.999.999:8787", allowLocalHTTP: true), nil, "reject malformed ipv4 loopback dev")
    eq(normalizeCoordinatorURL("http://[::1]:8787", allowLocalHTTP: true), "http://[::1]:8787", "allow ipv6 loopback dev")
    eq(normalizeCoordinatorURL("http://192.168.1.50:8787", allowLocalHTTP: true), nil, "reject LAN http even in dev")
    eq(normalizeCoordinatorURL(""), nil, "reject empty")
    eq(normalizeCoordinatorURL("   "), nil, "reject whitespace")
    eq(normalizeCredentialEndpointURL("https://alice:secret@api.islo.dev"), nil, "reject credential endpoint userinfo")
    eq(webViewOriginWhitelist("https://crabbox.sh"), ["https://*", "about:*"], "https-only whitelist")
    eq(webViewOriginWhitelist("http://localhost:8787"), ["https://*", "about:*", "http://localhost:8787"], "loopback whitelist")

    // navigationPolicy
    for url in ["mailto:a@b.com", "tel:+15551234", "sms:+1", "itms-apps://apps.apple.com/app/id1"] {
        isTrue(shouldOpenExternally(url), "external scheme: \(url)")
    }
    isTrue(!shouldOpenExternally("https://crabbox.sh"), "https not external")
    eq(hostLabel("https://crabbox.sh/team"), "crabbox.sh", "hostLabel https")
    eq(hostLabel("http://localhost:8787"), "localhost:8787", "hostLabel with port")
    eq(hostLabel(":::: not a url"), "crabbox.sh", "hostLabel fallback")
    let wl = webViewOriginWhitelist("https://crabbox.sh")
    isTrue(isWithinWhitelist("https://other.example.com", wl), "https within whitelist")
    isTrue(!isWithinWhitelist("http://evil.example.com", wl), "http not within whitelist")
    eq(shouldStartLoadInWebView("http://evil.example.com", whitelistOrigins: wl, enforce: true), .external, "enforce blocks off-origin")
    eq(shouldStartLoadInWebView("https://crabbox.sh/x", whitelistOrigins: wl, enforce: true), .load, "enforce allows whitelisted")
    isTrue(isAllowedNavigation("mailto:a@b.com", homeURL: "https://crabbox.sh"), "external allowed nav")
    isTrue(!isAllowedNavigation("http://evil.example.com", homeURL: "https://crabbox.sh"), "off-origin http blocked nav")

    // sandbox provider selection
    eq(selectSandboxProvider(isloEnabled: true, isloKey: " ak_test ", crabboxToken: " session "), .isloDirect, "islo wins when enabled")
    eq(selectSandboxProvider(isloEnabled: false, isloKey: " ak_test ", crabboxToken: " session "), .workspaceOnly, "disabled islo key is inactive")
    eq(selectSandboxProvider(isloEnabled: true, isloKey: "   ", crabboxToken: " session "), .workspaceOnly, "blank islo key is inactive")
    eq(selectSandboxProvider(isloEnabled: false, isloKey: nil, crabboxToken: nil), .none, "no sandbox provider")
    eq(sandboxProviderLabel(for: .isloDirect), "islo.dev (direct)", "islo label")
    eq(sandboxProviderLabel(for: .workspaceOnly), "crabbox.sh (workspace only)", "workspace-only label")
    eq(
        sandboxEngineDisplayName(for: SandboxHandle(id: "sbx_123", provider: "unit", status: "running")),
        "Sandbox · sbx_123",
        "sandbox engine identity"
    )

    // map-reduce planner/reducer
    let shards = planMapReduceShards(total: 1000, sandboxIDs: ["a", "b", "c"])
    eq(shards, [
        MapReduceShard(sandboxID: "a", lowerBound: 1, upperBound: 333),
        MapReduceShard(sandboxID: "b", lowerBound: 334, upperBound: 666),
        MapReduceShard(sandboxID: "c", lowerBound: 667, upperBound: 1000),
    ], "map-reduce shards cover 1...1000")
    eq(planMapReduceShards(total: 2, sandboxIDs: ["a", "b", "c"]), [
        MapReduceShard(sandboxID: "a", lowerBound: 1, upperBound: 1),
        MapReduceShard(sandboxID: "b", lowerBound: 2, upperBound: 2),
    ], "map-reduce skips empty shards")
    let mapResults = [
        parseMapReduceOutput("55611 1 333", sandboxID: "a"),
        parseMapReduceOutput("166500 334 666", sandboxID: "b"),
        parseMapReduceOutput("278389 667 1000", sandboxID: "c"),
    ].compactMap { $0 }
    let summary = reduceMapResults(total: 1000, results: mapResults)
    eq(summary.total, 500500, "map-reduce total")
    eq(summary.min, 1, "map-reduce min")
    eq(summary.max, 1000, "map-reduce max")
    eq(summary.expected, 500500, "map-reduce expected")
    isTrue(summary.ok, "map-reduce summary ok")
    eq(parseMapReduceOutput("not numbers", sandboxID: "a"), nil, "reject malformed map output")

    // sandbox launch readiness: never register an unready chat engine.
    do {
        let provisioner = UnitProvisioner(handle: SandboxHandle(id: "ready", provider: "unit", status: "running", ollamaEndpoint: "https://example.com"))
        let (_, engine) = try await launchLLMSandbox(
            provisioner: provisioner,
            name: "ready",
            model: "tiny",
            readinessAttempts: 3,
            readinessSleep: {},
            engineFactory: { _, _ in UnitEngine(successAfter: 1) }
        )
        eq(engine.displayName, "unit-engine", "ready engine returned")
    } catch {
        v.append("ready launch failed: \(error)")
    }
    do {
        let provisioner = UnitProvisioner(handle: SandboxHandle(id: "cold", provider: "unit", status: "running", ollamaEndpoint: "https://example.com"))
        _ = try await launchLLMSandbox(
            provisioner: provisioner,
            name: "cold",
            model: "tiny",
            readinessAttempts: 2,
            readinessSleep: {},
            engineFactory: { _, _ in UnitEngine(successAfter: nil) }
        )
        v.append("unready engine returned instead of failing")
    } catch {
        isTrue(String(describing: error).contains("did not become ready"), "unready launch reports readiness failure")
    }

    return v
}

private struct UnitProvisioner: SandboxProvisioner {
    let providerName = "unit"
    let handle: SandboxHandle

    func launch(name: String, model: String) async throws -> SandboxHandle { handle }
    func list() async throws -> [SandboxHandle] { [handle] }
    func stop(id: String) async throws {}
    func pause(id: String) async throws {}
    func resume(id: String) async throws {}
}

private actor UnitCounter {
    var checks = 0
    let successAfter: Int?

    init(successAfter: Int?) {
        self.successAfter = successAfter
    }

    func next() -> Bool {
        checks += 1
        guard let successAfter else { return false }
        return checks > successAfter
    }
}

private struct UnitEngine: LLMEngine {
    let displayName = "unit-engine"
    let kind: EngineKind = .sandbox
    let counter: UnitCounter

    init(successAfter: Int?) {
        self.counter = UnitCounter(successAfter: successAfter)
    }

    func isReady() async -> Bool {
        await counter.next()
    }

    func reply(messages: [ChatMessage], options: LLMOptions) async throws -> String {
        "ok"
    }
}
