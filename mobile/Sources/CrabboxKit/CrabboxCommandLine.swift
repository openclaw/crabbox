import Foundation

public enum CrabboxCommandLineError: Error, CustomStringConvertible, Equatable {
    case unterminatedQuote(Character)
    case trailingEscape
    case empty

    public var description: String {
        switch self {
        case .unterminatedQuote(let quote): return "unterminated quote \(quote)"
        case .trailingEscape: return "trailing escape"
        case .empty: return "empty command"
        }
    }
}

/// Parses a small POSIX-like command line for the mobile Crabbox terminal.
/// Leading `crabbox` is accepted and stripped, because users naturally type the
/// desktop command form.
public func parseCrabboxCommandLine(_ line: String) throws -> [String] {
    var args: [String] = []
    var current = ""
    var quote: Character?
    var escaping = false

    for char in line {
        if escaping {
            current.append(char)
            escaping = false
            continue
        }
        if char == "\\" {
            escaping = true
            continue
        }
        if let active = quote {
            if char == active {
                quote = nil
            } else {
                current.append(char)
            }
            continue
        }
        if char == "'" || char == "\"" {
            quote = char
            continue
        }
        if char.isWhitespace {
            if !current.isEmpty {
                args.append(current)
                current = ""
            }
            continue
        }
        current.append(char)
    }

    if escaping { throw CrabboxCommandLineError.trailingEscape }
    if let quote { throw CrabboxCommandLineError.unterminatedQuote(quote) }
    if !current.isEmpty { args.append(current) }
    if args.first == "crabbox" { args.removeFirst() }
    if args.isEmpty { throw CrabboxCommandLineError.empty }
    return args
}

public func commandLineNeedsIsloKey(_ args: [String]) -> Bool {
    args.enumerated().contains { index, value in
        value == "islo" && index > 0 && ["--provider", "-provider"].contains(args[index - 1])
    } || args.contains("--provider=islo") ||
        args.contains("-provider=islo") ||
        args.contains("--islo-base-url")
}
