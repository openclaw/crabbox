import Foundation

/// The coordinator the app points at out of the box.
public let defaultCoordinatorURL = "https://crabbox.sh"

/// Normalizes a user-entered coordinator URL using the same rules as the
/// original React Native client:
///
/// - A bare host (`crabbox.sh`) is promoted to `https://`.
/// - Only `https://` is accepted, except `http://` to a loopback host when
///   `allowLocalHTTP` is true (development builds only).
/// - Query strings and fragments are stripped; trailing slashes are removed.
///
/// Returns `nil` for anything that is not an acceptable coordinator URL.
public func normalizeCoordinatorURL(_ value: String, allowLocalHTTP: Bool = false) -> String? {
    let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
    if trimmed.isEmpty { return nil }

    let withProtocol = hasURLScheme(trimmed) ? trimmed : "https://\(trimmed)"

    guard let components = URLComponents(string: withProtocol),
          let scheme = components.scheme?.lowercased(),
          let host = components.host, !host.isEmpty
    else { return nil }

    if scheme != "https" {
        guard allowLocalHTTP, scheme == "http", isLoopbackHost(host) else { return nil }
    }

    var origin = "\(scheme)://\(hostForOrigin(host))"
    if let port = components.port { origin += ":\(port)" }

    var path = components.percentEncodedPath
    while path.hasSuffix("/") { path.removeLast() }

    return origin + path
}

/// The `originWhitelist` handed to the web view. HTTPS and `about:` are always
/// allowed; a single loopback HTTP origin is added only when the coordinator is
/// itself a loopback HTTP URL (development).
public func webViewOriginWhitelist(_ coordinatorURL: String) -> [String] {
    var origins = ["https://*", "about:*"]

    if let components = URLComponents(string: coordinatorURL),
       components.scheme?.lowercased() == "http",
       let host = components.host, isLoopbackHost(host) {
        var origin = "http://\(hostForOrigin(host))"
        if let port = components.port { origin += ":\(port)" }
        origins.append(origin)
    }

    return origins
}

// MARK: - Internal helpers

func hasURLScheme(_ value: String) -> Bool {
    guard let range = value.range(of: "^[a-z][a-z0-9+.-]*://", options: [.regularExpression, .caseInsensitive]) else {
        return false
    }
    return range.lowerBound == value.startIndex
}

/// Re-adds IPv6 brackets when reconstructing an origin from a parsed host.
func hostForOrigin(_ host: String) -> String {
    if host.contains(":") && !host.hasPrefix("[") {
        return "[\(host)]"
    }
    return host
}

func isLoopbackHost(_ hostname: String) -> Bool {
    var host = hostname.lowercased()
    if host.hasPrefix("[") { host.removeFirst() }
    if host.hasSuffix("]") { host.removeLast() }

    if host == "localhost" || host == "::1" { return true }

    return host.range(of: "^127(\\.[0-9]{1,3}){0,3}$", options: .regularExpression) != nil
}
