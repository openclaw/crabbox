//
//  CrabboxBinaryEngine.swift
//  Crabbox
//
//  Thin Swift bridge to the compiled Go CrabboxMobile static library.
//

import Foundation
import CrabboxKit

#if canImport(CrabboxMobile)
import CrabboxMobile
#endif

struct CrabboxBinaryResult: Decodable, Equatable, Sendable {
    let exitCode: Int
    let stdout: String
    let stderr: String
    let error: String?
}

enum CrabboxBinaryEngine {
    static var isAvailable: Bool {
        #if canImport(CrabboxMobile)
        true
        #else
        false
        #endif
    }

    static func run(
        args: [String],
        env: [String: String],
        stdin: String = "",
        timeoutSeconds: Int = 1_800
    ) throws -> CrabboxBinaryResult {
        #if canImport(CrabboxMobile)
        let body = try JSONSerialization.data(withJSONObject: [
            "args": args,
            "env": env,
            "stdin": stdin,
            "timeoutSeconds": timeoutSeconds,
        ] as [String: Any])
        let request = String(data: body, encoding: .utf8) ?? "{}"
        let raw = request.withCString { pointer in
            CrabboxMobileRun(UnsafeMutablePointer(mutating: pointer))
        }
        guard let raw else { throw LLMError.invalidResponse }
        defer { CrabboxMobileFree(raw) }
        let response = String(cString: raw)
        guard let data = response.data(using: .utf8) else {
            throw LLMError.invalidResponse
        }
        return try JSONDecoder().decode(CrabboxBinaryResult.self, from: data)
        #else
        throw LLMError.unavailable("CrabboxMobile is not linked into this build")
        #endif
    }
}
