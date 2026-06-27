//
//  CrabboxRuntimePaths.swift
//  Crabbox
//
//  The embedded Go crabbox core (CrabboxMobile) reuses the desktop CLI's
//  filesystem conventions: it reads/writes lease claims under an XDG/`HOME`
//  derived "state dir" (see internal/cli/claim.go -> crabboxStateDir, which
//  consults XDG_STATE_HOME and falls back to os.UserConfigDir()/$HOME).
//
//  An iOS app must NOT rely on `$HOME/Library/Application Support` resolving the
//  same way it does on a Mac. To guarantee the core always has a writable place
//  for its claims/config/cache, we point the XDG_* + HOME env vars at the app's
//  own sandbox container before every run. Everything stays inside the app's
//  Application Support / Caches directories, which are always writable.
//

import Foundation

enum CrabboxRuntimePaths {

    /// Environment variables that pin the Go core's state/config/cache/home to
    /// writable locations inside the iOS app sandbox. Directories are created
    /// eagerly so the core's `os.MkdirAll`/read paths never hit a missing parent.
    ///
    /// Merge this FIRST when assembling the env for `CrabboxBinaryEngine.run`,
    /// then layer command-specific values (ISLO_API_KEY, coordinator, …) on top.
    static func sandboxEnvironment() -> [String: String] {
        let fm = FileManager.default

        // App's Application Support container (created if needed); fall back to
        // the temp dir if for some reason it can't be resolved.
        let appSupport = (try? fm.url(
            for: .applicationSupportDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )) ?? fm.temporaryDirectory

        let root = appSupport.appendingPathComponent("crabbox", isDirectory: true)
        let state = root.appendingPathComponent("state", isDirectory: true)
        let config = root.appendingPathComponent("config", isDirectory: true)
        let cache = fm.temporaryDirectory.appendingPathComponent("crabbox-cache", isDirectory: true)

        for dir in [root, state, config, cache] {
            try? fm.createDirectory(at: dir, withIntermediateDirectories: true)
        }

        return [
            // os.UserHomeDir() reads $HOME; NSHomeDirectory() is the writable
            // sandbox container on iOS.
            "HOME": NSHomeDirectory(),
            "XDG_STATE_HOME": state.path,
            "XDG_CONFIG_HOME": config.path,
            "XDG_CACHE_HOME": cache.path,
            "XDG_DATA_HOME": root.appendingPathComponent("data", isDirectory: true).path,
        ]
    }
}
