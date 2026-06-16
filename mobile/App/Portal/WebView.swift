import SwiftUI
import WebKit
import CrabboxKit

/// A `WKWebView` bridged into SwiftUI and wired to a `PortalModel`. All of the
/// browser-y concerns live here:
///
///  - Persistent data store (`.default()`) so a GitHub OAuth session survives
///    relaunches.
///  - Back/forward edge-swipe gestures.
///  - KVO on `estimatedProgress`, `title`, `canGoBack`, and `url` → mapped to
///    `webViewLoadProgress` / `webViewNavState`.
///  - Navigation-delegate lifecycle (`start` / `finish` / `fail`) → load actions.
///  - `decidePolicyFor` gating: external schemes are handed to the OS, off-origin
///    HTTPS is cancelled, in-whitelist navigation loads. The pure decision lives
///    in CrabboxKit (`shouldOpenExternally` + `isAllowedNavigation`).
///  - `createWebViewWith` returns `nil` so OAuth popups load *in place* instead of
///    spawning an orphan web view.
///  - Pull-to-refresh via a `UIRefreshControl` on the scroll view.
///
/// The web view is created once. SwiftUI's `updateUIView` only re-points it at
/// `state.homeURL` when the home URL actually changed (e.g. after switching
/// coordinators, which also bumps `webViewKey`), comparing against the currently
/// loaded URL to avoid reload loops.
struct WebView: UIViewRepresentable {
    @ObservedObject var model: PortalModel

    func makeCoordinator() -> Coordinator { Coordinator(model: model) }

    func makeUIView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        // Persist cookies / local storage so GitHub OAuth stays signed in.
        config.websiteDataStore = .default()

        let webView = WKWebView(frame: .zero, configuration: config)
        webView.navigationDelegate = context.coordinator
        webView.uiDelegate = context.coordinator
        webView.allowsBackForwardNavigationGestures = true
        webView.isOpaque = false
        webView.backgroundColor = UIColor(Theme.bg)
        webView.scrollView.backgroundColor = UIColor(Theme.bg)

        // Register with the model so imperative effects (reload / goBack) and the
        // coordinator can reach this exact instance.
        model.webView = webView
        context.coordinator.observe(webView)

        // Pull-to-refresh.
        let refresh = UIRefreshControl()
        refresh.tintColor = UIColor(Theme.accent)
        refresh.addTarget(context.coordinator,
                          action: #selector(Coordinator.handleRefresh(_:)),
                          for: .valueChanged)
        webView.scrollView.refreshControl = refresh
        context.coordinator.refreshControl = refresh

        // Initial load.
        if let url = URL(string: model.state.homeURL) {
            webView.load(URLRequest(url: url))
            context.coordinator.loadedHomeURL = model.state.homeURL
        }
        return webView
    }

    func updateUIView(_ webView: WKWebView, context: Context) {
        // Re-point only when the coordinator URL genuinely changed. `webViewKey`
        // bumps alongside `homeURL` changes (saveCoordinator/resetCoordinator/
        // goHome), but comparing the home URL against what's loaded is the robust
        // guard against redundant reloads.
        let home = model.state.homeURL
        guard home != context.coordinator.loadedHomeURL,
              let url = URL(string: home) else { return }
        context.coordinator.loadedHomeURL = home
        webView.load(URLRequest(url: url))
    }

    static func dismantleUIView(_ webView: WKWebView, coordinator: Coordinator) {
        coordinator.stopObserving(webView)
    }

    // MARK: - Coordinator

    /// Bridges UIKit/WebKit callbacks into `AppAction`s on the model.
    final class Coordinator: NSObject, WKNavigationDelegate, WKUIDelegate {
        private let model: PortalModel
        weak var refreshControl: UIRefreshControl?

        /// The home URL we last asked the web view to load. Prevents
        /// `updateUIView` from reloading on every SwiftUI pass.
        var loadedHomeURL: String?

        /// KVO tokens, retained for the lifetime of the observation.
        private var observations: [NSKeyValueObservation] = []

        init(model: PortalModel) { self.model = model }

        // MARK: KVO

        func observe(_ webView: WKWebView) {
            observations = [
                webView.observe(\.estimatedProgress, options: [.new]) { [weak self] wv, _ in
                    Task { @MainActor in
                        self?.model.dispatch(.webViewLoadProgress(progress: wv.estimatedProgress))
                    }
                },
                // Title and canGoBack and url all feed the same nav-state action.
                webView.observe(\.title, options: [.new]) { [weak self] wv, _ in
                    Task { @MainActor in self?.pushNavState(wv) }
                },
                webView.observe(\.canGoBack, options: [.new]) { [weak self] wv, _ in
                    Task { @MainActor in self?.pushNavState(wv) }
                },
                webView.observe(\.url, options: [.new]) { [weak self] wv, _ in
                    Task { @MainActor in self?.pushNavState(wv) }
                },
            ]
        }

        func stopObserving(_ webView: WKWebView) {
            observations.forEach { $0.invalidate() }
            observations.removeAll()
        }

        @MainActor
        private func pushNavState(_ webView: WKWebView) {
            model.dispatch(.webViewNavState(
                url: webView.url?.absoluteString ?? "",
                title: webView.title ?? "",
                canGoBack: webView.canGoBack
            ))
        }

        // MARK: Pull-to-refresh

        @MainActor
        @objc func handleRefresh(_ sender: UIRefreshControl) {
            model.dispatch(.reload)
        }

        // MARK: WKNavigationDelegate lifecycle

        func webView(_ webView: WKWebView, didStartProvisionalNavigation navigation: WKNavigation!) {
            Task { @MainActor in model.dispatch(.webViewLoadStart) }
        }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            Task { @MainActor in
                model.dispatch(.webViewLoadEnd)
                refreshControl?.endRefreshing()
            }
        }

        func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
            Task { @MainActor in
                model.dispatch(.webViewError)
                refreshControl?.endRefreshing()
            }
        }

        func webView(_ webView: WKWebView,
                     didFailProvisionalNavigation navigation: WKNavigation!,
                     withError error: Error) {
            Task { @MainActor in
                model.dispatch(.webViewError)
                refreshControl?.endRefreshing()
            }
        }

        // MARK: Navigation policy

        func webView(_ webView: WKWebView,
                     decidePolicyFor navigationAction: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            guard let requestURL = navigationAction.request.url else {
                decisionHandler(.cancel)
                return
            }
            let url = requestURL.absoluteString

            // External schemes (mailto/tel/sms/App Store) → hand off to the OS.
            if shouldOpenExternally(url) {
                decisionHandler(.cancel)
                UIApplication.shared.open(requestURL)
                return
            }

            // HTTPS-only + whitelist gate. Off-origin web links are cancelled so
            // the Portal stays a focused view of the coordinator (Guideline 4.2).
            if isAllowedNavigation(url, homeURL: model.state.homeURL) {
                decisionHandler(.allow)
            } else {
                decisionHandler(.cancel)
            }
        }

        // MARK: WKUIDelegate — popups

        /// Returning `nil` makes WebKit load the target navigation in the current
        /// web view rather than spawning a detached one. Critical for GitHub's
        /// OAuth popup flow, which otherwise opens a window we can't see.
        func webView(_ webView: WKWebView,
                     createWebViewWith configuration: WKWebViewConfiguration,
                     for navigationAction: WKNavigationAction,
                     windowFeatures: WKWindowFeatures) -> WKWebView? {
            guard let requestURL = navigationAction.request.url else { return nil }
            let url = requestURL.absoluteString
            if shouldOpenExternally(url) {
                UIApplication.shared.open(requestURL)
            } else if isAllowedNavigation(url, homeURL: model.state.homeURL) {
                webView.load(navigationAction.request)
            }
            return nil
        }
    }
}
