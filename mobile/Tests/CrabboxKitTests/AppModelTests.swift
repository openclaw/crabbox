import XCTest
@testable import CrabboxKit
@testable import CrabboxE2E

final class AppModelTests: XCTestCase {
    let prod = AppEnv(allowLocalHTTP: false)

    func testBootEmptyUsesDefault() {
        let r = reduce(initialState(), .bootLoaded(storedURL: nil), prod)
        XCTAssertEqual(r.state.homeURL, defaultCoordinatorURL)
        XCTAssertNil(r.state.persistedCoordinator)
        XCTAssertFalse(r.state.booting)
    }

    func testSaveValidPersists() {
        var s = reduce(initialState(), .bootLoaded(storedURL: nil), prod).state
        s = reduce(s, .openSettings, prod).state
        s = reduce(s, .setDraft(url: "broker.example.com"), prod).state
        let r = reduce(s, .saveCoordinator, prod)
        XCTAssertEqual(r.state.homeURL, "https://broker.example.com")
        XCTAssertEqual(r.effects, [.persistCoordinator(value: "https://broker.example.com")])
        XCTAssertFalse(r.state.settingsVisible)
        XCTAssertEqual(r.state.webViewKey, s.webViewKey + 1)
    }

    func testSaveInvalidKeepsSettingsOpen() {
        var s = reduce(initialState(), .bootLoaded(storedURL: nil), prod).state
        s = reduce(s, .openSettings, prod).state
        s = reduce(s, .setDraft(url: "http://broker.example.com"), prod).state
        let r = reduce(s, .saveCoordinator, prod)
        XCTAssertFalse(r.state.urlError.isEmpty)
        XCTAssertTrue(r.state.settingsVisible)
        XCTAssertEqual(r.state.homeURL, defaultCoordinatorURL)
        XCTAssertTrue(r.effects.isEmpty)
    }

    func testShouldStartLoadExternal() {
        let s = reduce(initialState(), .bootLoaded(storedURL: nil), prod).state
        let r = reduce(s, .shouldStartLoad(url: "mailto:team@crabbox.sh"), prod)
        XCTAssertEqual(r.loadDecision, .external)
        XCTAssertEqual(r.effects, [.openExternal(url: "mailto:team@crabbox.sh")])
    }

    func testSelectors() {
        var s = initialState()
        s.loadFailed = true
        XCTAssertEqual(selectStatusLabel(s), .offline)
        s.loadFailed = false
        s.isLoading = true
        XCTAssertEqual(selectStatusLabel(s), .loading)
        s.isLoading = false
        XCTAssertEqual(selectStatusLabel(s), .live)
        s.progress = 0
        XCTAssertGreaterThanOrEqual(selectLoadingWidthPct(s), 6)
    }

    /// The headline guarantee: every scenario runs with zero invariant violations.
    func testAllScenariosHoldInvariants() {
        for scenario in scenarios {
            let sim = Sim(env: scenario.env)
            scenario.body(sim)
            XCTAssertTrue(
                sim.violations.isEmpty,
                "scenario \(scenario.name) violated invariants:\n\(sim.violations.joined(separator: "\n"))"
            )
        }
    }

    /// The agentic loop with the deterministic fallback (no Ollama) must also
    /// keep every invariant.
    func testAgentLoopHoldsInvariants() {
        for env in [AppEnv(allowLocalHTTP: false), AppEnv(allowLocalHTTP: true)] {
            let sim = runAgentLoop(env: env, steps: 30)
            XCTAssertTrue(
                sim.violations.isEmpty,
                "agent loop violated invariants:\n\(sim.violations.joined(separator: "\n"))"
            )
        }
    }
}
