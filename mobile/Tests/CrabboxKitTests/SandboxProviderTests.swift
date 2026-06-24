import XCTest
@testable import CrabboxKit

final class SandboxProviderTests: XCTestCase {
    func testProviderSelectionRequiresEnabledIsloKeyForSandboxLifecycle() {
        XCTAssertEqual(
            selectSandboxProvider(isloEnabled: true, isloKey: " ak_test ", crabboxToken: " session "),
            .isloDirect
        )
        XCTAssertEqual(
            selectSandboxProvider(isloEnabled: false, isloKey: " ak_test ", crabboxToken: " session "),
            .workspaceOnly
        )
        XCTAssertEqual(
            selectSandboxProvider(isloEnabled: true, isloKey: "   ", crabboxToken: " session "),
            .workspaceOnly
        )
        XCTAssertEqual(
            selectSandboxProvider(isloEnabled: false, isloKey: nil, crabboxToken: nil),
            .none
        )
    }

    func testProviderLabelsMatchSelectionPolicy() {
        XCTAssertEqual(sandboxProviderLabel(for: .isloDirect), "islo.dev (direct)")
        XCTAssertEqual(sandboxProviderLabel(for: .workspaceOnly), "crabbox.sh (workspace only)")
        XCTAssertEqual(sandboxProviderLabel(for: .none), "No sandbox provider configured")
    }

    func testMapReducePlannerCoversFullRangeOnLastShard() {
        let shards = planMapReduceShards(total: 1000, sandboxIDs: ["a", "b", "c"])
        XCTAssertEqual(
            shards,
            [
                MapReduceShard(sandboxID: "a", lowerBound: 1, upperBound: 333),
                MapReduceShard(sandboxID: "b", lowerBound: 334, upperBound: 666),
                MapReduceShard(sandboxID: "c", lowerBound: 667, upperBound: 1000),
            ]
        )
    }

    func testMapReducePlannerSkipsEmptyShards() {
        XCTAssertEqual(
            planMapReduceShards(total: 2, sandboxIDs: ["a", "b", "c"]),
            [
                MapReduceShard(sandboxID: "a", lowerBound: 1, upperBound: 1),
                MapReduceShard(sandboxID: "b", lowerBound: 2, upperBound: 2),
            ]
        )
    }

    func testMapReduceParserAndReducerValidateExpectedTotal() {
        let results = [
            parseMapReduceOutput("55611 1 333", sandboxID: "a"),
            parseMapReduceOutput("166500 334 666", sandboxID: "b"),
            parseMapReduceOutput("278389 667 1000", sandboxID: "c"),
        ].compactMap { $0 }

        let summary = reduceMapResults(total: 1000, results: results)
        XCTAssertEqual(summary.total, 500500)
        XCTAssertEqual(summary.min, 1)
        XCTAssertEqual(summary.max, 1000)
        XCTAssertEqual(summary.expected, 500500)
        XCTAssertEqual(summary.resultCount, 3)
        XCTAssertTrue(summary.ok)
    }

    func testMapReduceRejectsMalformedMapOutput() {
        XCTAssertNil(parseMapReduceOutput("not numbers", sandboxID: "a"))
        XCTAssertNil(parseMapReduceOutput("1 2", sandboxID: "a"))
    }

    func testLaunchLLMSandboxDoesNotReturnUntilEngineReady() async throws {
        let provisioner = FakeProvisioner(handle: SandboxHandle(id: "ready", provider: "fake", status: "running", ollamaEndpoint: "https://example.com"))
        let (_, engine) = try await launchLLMSandbox(
            provisioner: provisioner,
            name: "ready",
            model: "tiny",
            readinessAttempts: 3,
            readinessSleep: {},
            engineFactory: { _, _ in TestEngine(successAfter: 1) }
        )
        XCTAssertEqual(engine.displayName, "test-engine")
    }

    func testSandboxEngineDisplayNameUsesHandleID() {
        let handle = SandboxHandle(id: "sbx_123", provider: "fake", status: "running", ollamaEndpoint: "https://example.com")
        XCTAssertEqual(sandboxEngineDisplayName(for: handle), "Sandbox · sbx_123")
    }

    func testLaunchLLMSandboxFailsBeforeReturningUnreadyEngine() async {
        let provisioner = FakeProvisioner(handle: SandboxHandle(id: "cold", provider: "fake", status: "running", ollamaEndpoint: "https://example.com"))
        do {
            _ = try await launchLLMSandbox(
                provisioner: provisioner,
                name: "cold",
                model: "tiny",
                readinessAttempts: 2,
                readinessSleep: {},
                engineFactory: { _, _ in TestEngine(successAfter: nil) }
            )
            XCTFail("expected unready engine to fail")
        } catch {
            XCTAssertTrue(String(describing: error).contains("did not become ready"))
        }
    }
}

private struct FakeProvisioner: SandboxProvisioner {
    let providerName = "fake"
    let handle: SandboxHandle

    func launch(name: String, model: String) async throws -> SandboxHandle { handle }
    func list() async throws -> [SandboxHandle] { [handle] }
    func stop(id: String) async throws {}
    func pause(id: String) async throws {}
    func resume(id: String) async throws {}
}

private actor TestCounter {
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

private struct TestEngine: LLMEngine {
    let displayName = "test-engine"
    let kind: EngineKind = .sandbox
    let counter: TestCounter

    init(successAfter: Int?) {
        self.counter = TestCounter(successAfter: successAfter)
    }

    func isReady() async -> Bool {
        await counter.next()
    }

    func reply(messages: [ChatMessage], options: LLMOptions) async throws -> String {
        "ok"
    }
}
