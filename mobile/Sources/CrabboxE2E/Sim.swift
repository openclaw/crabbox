import CrabboxKit
import Foundation

/// One recorded step of a simulation run.
public struct TraceEvent {
    public let index: Int
    public let action: AppAction
    public let effects: [AppEffect]
    public let loadDecision: LoadDecision?
    public let state: AppState
}

/// A headless stand-in for the SwiftUI app. It owns an `AppState`, drives it
/// through the same `reduce` the app uses, models web-view effects
/// deterministically (no real I/O), and checks every invariant after each step.
public final class Sim {
    public private(set) var state: AppState
    public let env: AppEnv
    public private(set) var trace: [TraceEvent] = []
    public private(set) var violations: [String] = []

    public init(env: AppEnv) {
        self.env = env
        self.state = initialState()
    }

    @discardableResult
    public func dispatch(_ action: AppAction) -> ReduceResult {
        let before = state
        let result = reduce(state, action, env)
        state = result.state
        trace.append(
            TraceEvent(
                index: trace.count,
                action: action,
                effects: result.effects,
                loadDecision: result.loadDecision,
                state: state
            )
        )
        violations.append(
            contentsOf: checkInvariants(before: before, action: action, result: result, env: env)
                .map { "step \(trace.count - 1) [\(describe(action))]: \($0)" }
        )
        return result
    }

    /// Models a complete web-view load (start → progress → end → nav state).
    public func loadCycle(url: String, title: String, canGoBack: Bool = false) {
        dispatch(.webViewLoadStart)
        dispatch(.webViewLoadProgress(progress: 1))
        dispatch(.webViewLoadEnd)
        dispatch(.webViewNavState(url: url, title: title, canGoBack: canGoBack))
    }

    /// Scenario-specific assertion; failures are collected like invariant
    /// violations so a run reports every problem at once.
    public func expect(_ condition: Bool, _ message: String) {
        if !condition {
            violations.append("step \(trace.count) [assert]: \(message)")
        }
    }
}

/// A short label for an action, used in trace output.
public func describe(_ action: AppAction) -> String {
    switch action {
    case let .bootLoaded(url): return "bootLoaded(\(url ?? "nil"))"
    case .openSettings: return "openSettings"
    case .closeSettings: return "closeSettings"
    case let .setDraft(url): return "setDraft(\(url))"
    case .saveCoordinator: return "saveCoordinator"
    case .resetCoordinator: return "resetCoordinator"
    case .reload: return "reload"
    case .goHome: return "goHome"
    case .goBack: return "goBack"
    case .webViewLoadStart: return "loadStart"
    case .webViewLoadEnd: return "loadEnd"
    case let .webViewLoadProgress(p): return "loadProgress(\(p))"
    case .webViewError: return "error"
    case let .webViewNavState(url, _, back): return "navState(\(url), back:\(back))"
    case let .shouldStartLoad(url): return "shouldStartLoad(\(url))"
    }
}
