import XCTest
@testable import CrabboxKit

final class NavigationPolicyTests: XCTestCase {
    func testExternalSchemes() {
        for url in ["mailto:a@b.com", "tel:+15551234", "sms:+15551234", "itms-apps://apps.apple.com/app/id1"] {
            XCTAssertTrue(shouldOpenExternally(url), "\(url) should be external")
        }
        for url in ["https://crabbox.sh", "http://localhost:8787", "about:blank"] {
            XCTAssertFalse(shouldOpenExternally(url), "\(url) should not be external")
        }
    }

    func testHostLabelFallback() {
        XCTAssertEqual(hostLabel("https://crabbox.sh/team"), "crabbox.sh")
        XCTAssertEqual(hostLabel("http://localhost:8787"), "localhost:8787")
        XCTAssertEqual(hostLabel(":::: not a url"), "crabbox.sh")
    }

    func testWhitelistMatching() {
        let prodWL = webViewOriginWhitelist("https://crabbox.sh")
        XCTAssertTrue(isWithinWhitelist("https://crabbox.sh/anything", prodWL))
        XCTAssertTrue(isWithinWhitelist("https://other.example.com", prodWL))
        XCTAssertTrue(isWithinWhitelist("about:blank", prodWL))
        XCTAssertFalse(isWithinWhitelist("http://evil.example.com", prodWL))

        let devWL = webViewOriginWhitelist("http://localhost:8787")
        XCTAssertTrue(isWithinWhitelist("http://localhost:8787/run", devWL))
        XCTAssertFalse(isWithinWhitelist("http://localhost:9999/run", devWL))
    }

    func testShouldStartLoadEnforce() {
        let wl = webViewOriginWhitelist("https://crabbox.sh")
        XCTAssertEqual(shouldStartLoadInWebView("https://crabbox.sh/x", whitelistOrigins: wl, enforce: true), .load)
        XCTAssertEqual(shouldStartLoadInWebView("mailto:a@b.com", whitelistOrigins: wl, enforce: true), .external)
        XCTAssertEqual(shouldStartLoadInWebView("http://evil.example.com", whitelistOrigins: wl, enforce: true), .external)
        // Parity mode (enforce:false) only branches on scheme.
        XCTAssertEqual(shouldStartLoadInWebView("http://evil.example.com", enforce: false), .load)
    }

    func testIsAllowedNavigation() {
        XCTAssertTrue(isAllowedNavigation("https://anything.example.com", homeURL: "https://crabbox.sh"))
        XCTAssertTrue(isAllowedNavigation("mailto:a@b.com", homeURL: "https://crabbox.sh"))
        XCTAssertFalse(isAllowedNavigation("http://evil.example.com", homeURL: "https://crabbox.sh"))
    }
}
