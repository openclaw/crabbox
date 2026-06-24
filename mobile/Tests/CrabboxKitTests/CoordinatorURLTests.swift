import XCTest
@testable import CrabboxKit

final class CoordinatorURLTests: XCTestCase {
    func testNormalizesBareHTTPSCoordinators() {
        XCTAssertEqual(normalizeCoordinatorURL("crabbox.sh/team?token=redacted#section"), "https://crabbox.sh/team")
    }

    func testTrimsTrailingSlashOnlyPaths() {
        XCTAssertEqual(normalizeCoordinatorURL("https://broker.example.com////"), "https://broker.example.com")
    }

    func testRejectsProductionHTTP() {
        XCTAssertNil(normalizeCoordinatorURL("http://broker.example.com"))
    }

    func testRejectsLocalhostHTTPOutsideDev() {
        XCTAssertNil(normalizeCoordinatorURL("http://localhost:8787"))
    }

    func testAllowsLocalhostHTTPInDev() {
        XCTAssertEqual(normalizeCoordinatorURL("http://localhost:8787", allowLocalHTTP: true), "http://localhost:8787")
    }

    func testAllowsIPv4LoopbackHTTPInDev() {
        XCTAssertEqual(normalizeCoordinatorURL("http://127.0.0.1:8787", allowLocalHTTP: true), "http://127.0.0.1:8787")
    }

    func testRejectsMalformedIPv4LoopbackHTTPInDev() {
        XCTAssertNil(normalizeCoordinatorURL("http://127.999.999.999:8787", allowLocalHTTP: true))
    }

    func testAllowsIPv6LoopbackHTTPInDev() {
        XCTAssertEqual(normalizeCoordinatorURL("http://[::1]:8787", allowLocalHTTP: true), "http://[::1]:8787")
    }

    func testRejectsLANHTTPEvenInDev() {
        XCTAssertNil(normalizeCoordinatorURL("http://192.168.1.50:8787", allowLocalHTTP: true))
    }

    func testEmptyAndWhitespaceAreNil() {
        XCTAssertNil(normalizeCoordinatorURL(""))
        XCTAssertNil(normalizeCoordinatorURL("   "))
    }

    func testCredentialEndpointNormalizesBareHTTPS() {
        XCTAssertEqual(normalizeCredentialEndpointURL("api.islo.dev/v1?token=redacted#frag"), "https://api.islo.dev/v1")
    }

    func testCredentialEndpointRejectsHTTP() {
        XCTAssertNil(normalizeCredentialEndpointURL("http://api.islo.dev"))
    }

    func testCredentialEndpointRejectsURLCredentials() {
        XCTAssertNil(normalizeCredentialEndpointURL("https://alice:secret@api.islo.dev"))
    }

    func testWhitelistIsHTTPSOnlyByDefault() {
        XCTAssertEqual(webViewOriginWhitelist("https://crabbox.sh"), ["https://*", "about:*"])
    }

    func testWhitelistAddsOnlyActiveLoopbackOrigin() {
        XCTAssertEqual(
            webViewOriginWhitelist("http://localhost:8787"),
            ["https://*", "about:*", "http://localhost:8787"]
        )
    }

    func testCoordinatorClientNormalizesBeforeSendingBearerTokens() {
        let client = CoordinatorClient(coordinatorURL: "broker.example.com////?secret=redacted", token: " token ")
        XCTAssertEqual(client?.baseURL.absoluteString, "https://broker.example.com")
    }

    func testCoordinatorClientRejectsProductionHTTP() {
        XCTAssertNil(CoordinatorClient(coordinatorURL: "http://broker.example.com", token: "token"))
    }

    func testIsloClientRejectsProductionHTTPBeforeAuthExchange() {
        XCTAssertNil(IsloClient(apiKey: "key", baseURL: "http://api.islo.dev"))
    }

    func testIsloClientNormalizesBeforeSendingAccessKey() async {
        let client = IsloClient(apiKey: " key ", baseURL: "api.islo.dev////?secret=redacted")
        let baseURL = await client?.baseURL.absoluteString
        XCTAssertEqual(baseURL, "https://api.islo.dev")
    }

    func testCoordinatorProvisionerSandboxLifecycleFailsClosed() async {
        guard let provisioner = CoordinatorProvisioner(coordinatorURL: "https://crabbox.sh", token: "token") else {
            XCTFail("expected valid coordinator provisioner")
            return
        }

        do {
            _ = try await provisioner.launch(name: "test", model: "qwen2.5:0.5b")
            XCTFail("expected unsupported coordinator sandbox lifecycle")
        } catch {
            XCTAssertTrue(String(describing: error).contains("not supported"))
        }
    }

    func testIsloBootstrapRequiresPreinstalledOllama() {
        let script = isloOllamaBootstrapScript(model: "qwen'; touch /tmp/pwn #")
        XCTAssertFalse(script.contains("install.sh"))
        XCTAssertFalse(script.contains("curl -fsSL"))
        XCTAssertTrue(script.contains("BOOTSTRAP_FAILED missing_ollama"))
        XCTAssertTrue(script.contains("model='qwen'\"'\"'; touch /tmp/pwn #'"))
        XCTAssertTrue(script.contains("ollama pull \"$model\""))
    }

    func testWaitForEngineReadyFailsWhenAttemptsAreExhausted() async {
        let engine = TestReadinessEngine(successAfter: nil)
        let ready = await waitForEngineReady(engine, attempts: 2) {}
        XCTAssertFalse(ready)
    }

    func testWaitForEngineReadyReturnsWhenEngineBecomesReady() async {
        let engine = TestReadinessEngine(successAfter: 2)
        let ready = await waitForEngineReady(engine, attempts: 3) {}
        XCTAssertTrue(ready)
    }
}

private actor ReadinessCounter {
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

private struct TestReadinessEngine: LLMEngine {
    let displayName = "test"
    let kind: EngineKind = .sandbox
    let counter: ReadinessCounter

    init(successAfter: Int?) {
        self.counter = ReadinessCounter(successAfter: successAfter)
    }

    func isReady() async -> Bool {
        await counter.next()
    }

    func reply(messages: [ChatMessage], options: LLMOptions) async throws -> String {
        "ok"
    }
}
