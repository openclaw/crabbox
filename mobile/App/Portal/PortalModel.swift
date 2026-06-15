import Foundation
import WebKit
import CrabboxKit

/// The Portal's view-model: a thin SwiftUI-facing shell around CrabboxKit's pure
/// `reduce`. It owns the single source of truth (`AppState`), translates every
/// UI gesture and `WKWebView` callback into an `AppAction`, runs the reducer, and
/// then *performs* the resulting `AppEffect`s (persistence, web-view commands,
/// external links).
///
/// Design rules this enforces:
///  - The view never mutates `AppState` directly — it only calls `dispatch`.
///  - All decision logic lives in CrabboxKit; this class is pure plumbing.
///  - The `WKWebView` is created once and held weakly-ish here so effects like
///    `.webViewReload` / `.webViewGoBack` can drive it imperatively.
@MainActor
final class PortalModel: ObservableObject {
    /// The rendered state. `@Published` so SwiftUI recomputes on every change.
    @Published private(set) var state: AppState = initialState()

    /// Session-constant environment. `allowLocalHTTP` is on only in debug builds
    /// so developers can point at a loopback coordinator over plain HTTP.
    private let env: AppEnv

    /// The live web view. Owned by `WebView`/its coordinator at creation time and
    /// registered here so imperative effects can reach it. Kept `weak` to avoid a
    /// retain cycle with the coordinator that points back at this model.
    weak var webView: WKWebView?

    /// Backing store for the persisted coordinator URL.
    private let defaults: UserDefaults

    init(env: AppEnv = .makeDefault(), defaults: UserDefaults = .standard) {
        self.env = env
        self.defaults = defaults
    }

    /// Read the persisted coordinator (if any) and fold it into state. Call once
    /// when the Portal first appears.
    func boot() {
        guard state.booting else { return }
        let stored = defaults.string(forKey: coordinatorStorageKey)
        dispatch(.bootLoaded(storedURL: stored))
    }

    /// The single entry point for changing state. Runs the reducer, publishes the
    /// next state, and performs every emitted effect.
    @discardableResult
    func dispatch(_ action: AppAction) -> ReduceResult {
        let result = reduce(state, action, env)
        state = result.state
        for effect in result.effects { perform(effect) }
        return result
    }

    // MARK: - Effect interpreter

    private func perform(_ effect: AppEffect) {
        switch effect {
        case let .persistCoordinator(value):
            defaults.set(value, forKey: coordinatorStorageKey)

        case .webViewReload:
            webView?.reload()

        case .webViewGoBack:
            webView?.goBack()

        case let .openExternal(url):
            guard let target = URL(string: url) else { return }
            UIApplication.shared.open(target)
        }
    }
}

extension AppEnv {
    /// The environment the shipping app uses. Loopback HTTP coordinators are
    /// permitted in debug builds only.
    static func makeDefault() -> AppEnv {
        #if DEBUG
        return AppEnv(allowLocalHTTP: true)
        #else
        return AppEnv(allowLocalHTTP: false)
        #endif
    }
}
