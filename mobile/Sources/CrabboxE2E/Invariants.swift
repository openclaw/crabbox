import CrabboxKit
import Foundation

/// The safety/behavior invariants that must hold after every `reduce`. These
/// encode the security guarantees (HTTPS-only, whitelist) and UI consistency
/// rules of the app, and are checked for every step of every scenario and every
/// LLM-chosen action.
func checkInvariants(before: AppState, action: AppAction, result: ReduceResult, env: AppEnv) -> [String] {
    let after = result.state
    let effects = result.effects
    var v: [String] = []

    let whitelist = webViewOriginWhitelist(after.homeURL)

    // 1. The current URL is always inside the whitelist or an external scheme.
    if !(isWithinWhitelist(after.currentURL, whitelist) || shouldOpenExternally(after.currentURL)) {
        v.append("currentURL '\(after.currentURL)' is neither whitelisted nor external")
    }

    if !after.booting {
        // 2 & 3. homeURL is always a normalized, non-empty acceptable coordinator.
        let renorm = normalizeCoordinatorURL(after.homeURL, allowLocalHTTP: env.allowLocalHTTP)
        if renorm != after.homeURL {
            v.append("homeURL '\(after.homeURL)' is not a normalized coordinator (renorm: \(renorm ?? "nil"))")
        }
        if after.homeURL.isEmpty {
            v.append("homeURL is empty after boot")
        }
    }

    // 4. Successful SAVE persists home == current and the persisted value matches.
    if action == .saveCoordinator, effects.contains(where: isPersist) {
        if !(after.persistedCoordinator == after.homeURL && after.homeURL == after.currentURL) {
            v.append("after SAVE: persisted/home/current diverge")
        }
        if persistedValue(effects) != after.homeURL {
            v.append("after SAVE: persist effect value != homeURL")
        }
    }

    // 5. RESET returns everything to the default coordinator.
    if action == .resetCoordinator {
        if !(after.homeURL == defaultCoordinatorURL && after.currentURL == defaultCoordinatorURL
            && after.draftURL == defaultCoordinatorURL && after.persistedCoordinator == defaultCoordinatorURL) {
            v.append("after RESET: not fully reset to default")
        }
    }

    // 6. RELOAD changes nothing but the loading flags.
    if action == .reload {
        if after.homeURL != before.homeURL || after.currentURL != before.currentURL
            || after.draftURL != before.draftURL || after.persistedCoordinator != before.persistedCoordinator
            || after.webViewKey != before.webViewKey {
            v.append("RELOAD mutated more than loading flags")
        }
        if !(after.isLoading && !after.loadFailed) {
            v.append("RELOAD did not set isLoading=true, loadFailed=false")
        }
    }

    // 7. Settings visibility / error coupling for SAVE and RESET.
    if action == .saveCoordinator || action == .resetCoordinator {
        let succeeded = effects.contains(where: isPersist)
        if succeeded {
            if after.settingsVisible { v.append("settings still visible after successful \(describe(action))") }
        } else {
            // SAVE failure only
            if action == .saveCoordinator, !(after.settingsVisible && !after.urlError.isEmpty) {
                v.append("failed SAVE must keep settings open with an error")
            }
        }
    }

    // 8. Never simultaneously loading and failed.
    if after.isLoading && after.loadFailed {
        v.append("isLoading and loadFailed both true")
    }

    // 9. Progress is bounded and the loading bar never collapses below 6%.
    if after.progress < 0 || after.progress > 1 {
        v.append("progress out of range: \(after.progress)")
    }
    if selectLoadingWidthPct(after) < 6 {
        v.append("loading width below 6%")
    }

    // 10. A surfaced URL error never coincides with a coordinator mutation.
    if !after.urlError.isEmpty {
        if after.homeURL != before.homeURL || after.currentURL != before.currentURL
            || after.persistedCoordinator != before.persistedCoordinator {
            v.append("urlError set while coordinator changed in same step")
        }
    }

    // 11. webViewKey is monotonic and bumps only for the three remount actions.
    if after.webViewKey < before.webViewKey {
        v.append("webViewKey decreased")
    }
    let bumped = after.webViewKey > before.webViewKey
    let shouldBump: Bool = {
        switch action {
        case .saveCoordinator: return effects.contains(where: isPersist)
        case .resetCoordinator, .goHome: return true
        default: return false
        }
    }()
    if bumped != shouldBump {
        v.append("webViewKey bump mismatch for \(describe(action)) (bumped: \(bumped), expected: \(shouldBump))")
    }

    // 12. GO_BACK emits a goBack effect exactly when back navigation is possible.
    if action == .goBack {
        let emitted = effects.contains(.webViewGoBack)
        if emitted != before.canGoBack {
            v.append("goBack effect mismatch (emitted: \(emitted), canGoBack: \(before.canGoBack))")
        }
    }

    // 13. No effect without a cause.
    for effect in effects {
        switch effect {
        case .persistCoordinator:
            if !(action == .saveCoordinator || action == .resetCoordinator) {
                v.append("persist effect without SAVE/RESET")
            }
        case .webViewReload:
            if action != .reload { v.append("reload effect without RELOAD") }
        case .webViewGoBack:
            if action != .goBack { v.append("goBack effect without GO_BACK") }
        case let .openExternal(url):
            if case .shouldStartLoad = action {
                if !shouldOpenExternally(url) { v.append("openExternal for non-external URL") }
            } else {
                v.append("openExternal effect without SHOULD_START_LOAD")
            }
        }
    }

    return v
}

private func isPersist(_ effect: AppEffect) -> Bool {
    if case .persistCoordinator = effect { return true }
    return false
}

private func persistedValue(_ effects: [AppEffect]) -> String? {
    for effect in effects {
        if case let .persistCoordinator(value) = effect { return value }
    }
    return nil
}
