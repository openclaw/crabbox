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

    func testWhitelistIsHTTPSOnlyByDefault() {
        XCTAssertEqual(webViewOriginWhitelist("https://crabbox.sh"), ["https://*", "about:*"])
    }

    func testWhitelistAddsOnlyActiveLoopbackOrigin() {
        XCTAssertEqual(
            webViewOriginWhitelist("http://localhost:8787"),
            ["https://*", "about:*", "http://localhost:8787"]
        )
    }
}
