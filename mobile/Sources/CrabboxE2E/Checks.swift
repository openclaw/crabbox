import CrabboxKit
import Foundation

/// Dependency-free unit vectors for the pure logic, mirroring the XCTest cases
/// so the full suite is verifiable with `swift run crabbox-sim` even on a
/// machine without Xcode (where `swift test`/XCTest is unavailable). CI runs the
/// XCTest target; this keeps local proof honest.
public func runUnitChecks() -> [String] {
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
    eq(normalizeCoordinatorURL("http://[::1]:8787", allowLocalHTTP: true), "http://[::1]:8787", "allow ipv6 loopback dev")
    eq(normalizeCoordinatorURL("http://192.168.1.50:8787", allowLocalHTTP: true), nil, "reject LAN http even in dev")
    eq(normalizeCoordinatorURL(""), nil, "reject empty")
    eq(normalizeCoordinatorURL("   "), nil, "reject whitespace")
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

    return v
}
