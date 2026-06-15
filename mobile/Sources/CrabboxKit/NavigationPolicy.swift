import Foundation

/// Whether a tapped URL should be handed to the OS instead of loaded in the web
/// view (mail, phone, SMS, App Store links).
public func shouldOpenExternally(_ url: String) -> Bool {
    guard let range = url.range(of: "^(mailto|tel|sms|itms-apps):", options: [.regularExpression, .caseInsensitive]) else {
        return false
    }
    return range.lowerBound == url.startIndex
}

/// A human-friendly host label for the header. Mirrors the original client's
/// `new URL(value).host` (host + optional port) with a `crabbox.sh` fallback so
/// it never throws on malformed input.
public func hostLabel(_ value: String) -> String {
    guard let components = URLComponents(string: value), let host = components.host, !host.isEmpty else {
        return "crabbox.sh"
    }
    if let port = components.port {
        return "\(hostForOrigin(host)):\(port)"
    }
    return hostForOrigin(host)
}

/// The scheme + host (+ port) origin of a URL, or `nil` when unparseable.
func originOf(_ url: String) -> String? {
    guard let components = URLComponents(string: url),
          let scheme = components.scheme?.lowercased(),
          let host = components.host, !host.isEmpty
    else { return nil }
    var origin = "\(scheme)://\(hostForOrigin(host))"
    if let port = components.port { origin += ":\(port)" }
    return origin
}

/// Whether `url` is permitted by a web-view origin whitelist. `https://*`
/// matches any HTTPS origin, `about:*` matches `about:` URLs, and any concrete
/// origin entry matches by exact origin.
public func isWithinWhitelist(_ url: String, _ whitelistOrigins: [String]) -> Bool {
    guard let components = URLComponents(string: url), let scheme = components.scheme?.lowercased() else {
        return false
    }
    let origin = originOf(url)

    for entry in whitelistOrigins {
        switch entry {
        case "https://*":
            if scheme == "https" { return true }
        case "about:*":
            if scheme == "about" { return true }
        default:
            if let origin, origin == entry { return true }
        }
    }
    return false
}

/// The decision returned to the web view's "should start load" hook.
public enum LoadDecision: String, Equatable {
    case load
    case external
}

/// Decides whether a navigation should load in the web view or be opened
/// externally.
///
/// - `enforce == false` (default) mirrors the original client exactly: only the
///   URL scheme is consulted (external schemes go out, everything else loads).
/// - `enforce == true` (used by the headless simulator and as defense-in-depth)
///   additionally requires in-web-view navigations to be inside the whitelist.
public func shouldStartLoadInWebView(
    _ url: String,
    whitelistOrigins: [String] = [],
    enforce: Bool = false
) -> LoadDecision {
    if shouldOpenExternally(url) { return .external }
    if !enforce { return .load }
    return isWithinWhitelist(url, whitelistOrigins) ? .load : .external
}

/// Whether navigating to `url` is allowed given the current `homeURL`
/// coordinator (inside the whitelist, or an external scheme we hand off).
public func isAllowedNavigation(_ url: String, homeURL: String) -> Bool {
    isWithinWhitelist(url, webViewOriginWhitelist(homeURL)) || shouldOpenExternally(url)
}
