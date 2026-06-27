#if os(macOS)
import AppKit
import CrabboxKit
import SwiftUI
import WebKit

// crabbox-mac — a tiny native macOS preview harness for the Crabbox web client.
//
//   swift run crabbox-mac
//
// It opens a real WKWebView pointed at https://crabbox.sh with native chrome,
// reusing the EXACT CrabboxKit navigation policy the iOS app uses. It exists so
// the native web-view experience can be exercised on a Mac that only has the
// Command Line Tools (no full Xcode / iOS Simulator). The shippable artifact is
// the iOS app target; this is a developer convenience.

final class WebModel: ObservableObject {
    @Published var homeURL: String = defaultCoordinatorURL
    @Published var currentHost: String = hostLabel(defaultCoordinatorURL)
    @Published var progress: Double = 0
    @Published var isLoading: Bool = true
    @Published var canGoBack: Bool = false
    @Published var title: String = "Crabbox"
    weak var webView: WKWebView?

    func load(_ urlString: String) {
        guard let normalized = normalizeCoordinatorURL(urlString, allowLocalHTTP: true),
              let url = URL(string: normalized) else { return }
        homeURL = normalized
        currentHost = hostLabel(normalized)
        webView?.load(URLRequest(url: url))
    }
}

struct WebView: NSViewRepresentable {
    @ObservedObject var model: WebModel
    func makeCoordinator() -> Coordinator { Coordinator(model) }

    func makeNSView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .default() // persist the GitHub/portal session
        config.preferences.javaScriptCanOpenWindowsAutomatically = false
        let webView = WKWebView(frame: .zero, configuration: config)
        webView.navigationDelegate = context.coordinator
        webView.uiDelegate = context.coordinator
        webView.allowsBackForwardNavigationGestures = true
        context.coordinator.observe(webView)
        model.webView = webView
        if let url = URL(string: model.homeURL) { webView.load(URLRequest(url: url)) }
        return webView
    }

    func updateNSView(_ webView: WKWebView, context: Context) {}

    final class Coordinator: NSObject, WKNavigationDelegate, WKUIDelegate {
        let model: WebModel
        private var observations: [NSKeyValueObservation] = []
        init(_ model: WebModel) { self.model = model }

        func observe(_ webView: WKWebView) {
            observations = [
                webView.observe(\.estimatedProgress, options: .new) { w, _ in
                    DispatchQueue.main.async { self.model.progress = w.estimatedProgress }
                },
                webView.observe(\.canGoBack, options: .new) { w, _ in
                    DispatchQueue.main.async { self.model.canGoBack = w.canGoBack }
                },
                webView.observe(\.title, options: .new) { w, _ in
                    DispatchQueue.main.async { self.model.title = w.title ?? "Crabbox" }
                },
                webView.observe(\.url, options: .new) { w, _ in
                    DispatchQueue.main.async { if let u = w.url?.absoluteString { self.model.currentHost = hostLabel(u) } }
                },
            ]
        }

        // The same security policy CrabboxKit enforces for the iOS app: external
        // schemes go to the OS, everything else must be inside the whitelist.
        func webView(
            _ webView: WKWebView,
            decidePolicyFor action: WKNavigationAction,
            decisionHandler: @escaping (WKNavigationActionPolicy) -> Void
        ) {
            guard let url = action.request.url?.absoluteString else { decisionHandler(.cancel); return }
            if shouldOpenExternally(url) {
                if let u = action.request.url { NSWorkspace.shared.open(u) }
                decisionHandler(.cancel)
                return
            }
            if isAllowedNavigation(url, homeURL: model.homeURL) {
                decisionHandler(.allow)
            } else if action.navigationType == .linkActivated, let u = action.request.url {
                NSWorkspace.shared.open(u) // off-origin tapped link opens in the browser
                decisionHandler(.cancel)
            } else {
                decisionHandler(.cancel)
            }
        }

        func webView(_ webView: WKWebView, didStartProvisionalNavigation n: WKNavigation!) {
            DispatchQueue.main.async { self.model.isLoading = true }
        }
        func webView(_ webView: WKWebView, didFinish n: WKNavigation!) {
            DispatchQueue.main.async { self.model.isLoading = false }
        }
        func webView(_ webView: WKWebView, didFail n: WKNavigation!, withError e: Error) {
            DispatchQueue.main.async { self.model.isLoading = false }
        }

        // Never spawn an uncontrolled window (GitHub OAuth popups load in-place).
        func webView(
            _ webView: WKWebView,
            createWebViewWith configuration: WKWebViewConfiguration,
            for action: WKNavigationAction,
            windowFeatures: WKWindowFeatures
        ) -> WKWebView? {
            if let url = action.request.url?.absoluteString, isAllowedNavigation(url, homeURL: model.homeURL) {
                webView.load(action.request)
            } else if let u = action.request.url {
                NSWorkspace.shared.open(u)
            }
            return nil
        }
    }
}

struct RootView: View {
    @StateObject var model = WebModel()
    @State private var draft = defaultCoordinatorURL

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                Text("🦀 Crabbox").font(.headline)
                Text(model.currentHost).foregroundColor(.secondary).font(.subheadline)
                Spacer()
                HStack(spacing: 6) {
                    Circle().frame(width: 7, height: 7).foregroundColor(model.isLoading ? .yellow : .green)
                    Text(model.isLoading ? "Loading" : "Live").font(.caption).foregroundColor(.secondary)
                }
                Button { model.webView?.goBack() } label: { Image(systemName: "chevron.left") }
                    .disabled(!model.canGoBack)
                Button { model.load(model.homeURL) } label: { Image(systemName: "house") }
                Button { model.webView?.reload() } label: { Image(systemName: "arrow.clockwise") }
            }
            .padding(10)

            if model.isLoading {
                ProgressView(value: model.progress).progressViewStyle(.linear).frame(height: 2)
            } else {
                Color.clear.frame(height: 2)
            }

            WebView(model: model)

            HStack(spacing: 8) {
                TextField("https://crabbox.sh", text: $draft)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit { model.load(draft) }
                Button("Connect") { model.load(draft) }
            }
            .padding(10)
        }
        .frame(minWidth: 420, minHeight: 640)
    }
}

// Reliable CLI launch: explicit NSApplication, regular activation policy, a real
// window hosting the SwiftUI view. (@main App lifecycle can fail to foreground
// when run as a bare `swift run` executable.)
let app = NSApplication.shared
app.setActivationPolicy(.regular)

let window = NSWindow(
    contentRect: NSRect(x: 0, y: 0, width: 430, height: 720),
    styleMask: [.titled, .closable, .miniaturizable, .resizable],
    backing: .buffered,
    defer: false
)
window.title = "Crabbox (macOS preview)"
window.center()
window.contentView = NSHostingView(rootView: RootView())
window.makeKeyAndOrderFront(nil)
app.activate(ignoringOtherApps: true)
app.run()
#endif
