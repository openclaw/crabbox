import Foundation

/// Persisted-storage key for the chosen coordinator URL.
public let coordinatorStorageKey = "crabbox.mobile.coordinator-url"

private let httpsError =
    "Enter an HTTPS URL. HTTP is available only for localhost in development builds."

/// The complete, framework-free state of the app. The SwiftUI view renders this
/// and the headless simulator drives it through the exact same `reduce`.
public struct AppState: Equatable {
    public var booting: Bool
    public var homeURL: String          // web view source; persisted
    public var currentURL: String
    public var draftURL: String
    public var settingsVisible: Bool
    public var isLoading: Bool
    public var loadFailed: Bool
    public var canGoBack: Bool
    public var pageTitle: String
    public var progress: Double         // 0...1
    public var urlError: String         // "" == none
    public var persistedCoordinator: String?  // sync mirror of stored value
    public var webViewKey: Int          // bump forces a web-view remount

    public init(
        booting: Bool = true,
        homeURL: String = defaultCoordinatorURL,
        currentURL: String = defaultCoordinatorURL,
        draftURL: String = defaultCoordinatorURL,
        settingsVisible: Bool = false,
        isLoading: Bool = true,
        loadFailed: Bool = false,
        canGoBack: Bool = false,
        pageTitle: String = "Crabbox",
        progress: Double = 0,
        urlError: String = "",
        persistedCoordinator: String? = nil,
        webViewKey: Int = 0
    ) {
        self.booting = booting
        self.homeURL = homeURL
        self.currentURL = currentURL
        self.draftURL = draftURL
        self.settingsVisible = settingsVisible
        self.isLoading = isLoading
        self.loadFailed = loadFailed
        self.canGoBack = canGoBack
        self.pageTitle = pageTitle
        self.progress = progress
        self.urlError = urlError
        self.persistedCoordinator = persistedCoordinator
        self.webViewKey = webViewKey
    }
}

/// Environment that does not change during a session. `allowLocalHTTP` is wired
/// to a debug build flag in the app (loopback HTTP coordinators in dev only).
public struct AppEnv: Equatable {
    public var allowLocalHTTP: Bool
    public init(allowLocalHTTP: Bool = false) { self.allowLocalHTTP = allowLocalHTTP }
}

/// Every input the app or simulator can dispatch.
public enum AppAction: Equatable {
    case bootLoaded(storedURL: String?)
    case openSettings
    case closeSettings
    case setDraft(url: String)
    case saveCoordinator
    case resetCoordinator
    case reload
    case goHome
    case goBack
    case webViewLoadStart
    case webViewLoadEnd
    case webViewLoadProgress(progress: Double)
    case webViewError
    case webViewNavState(url: String, title: String, canGoBack: Bool)
    case shouldStartLoad(url: String)
}

/// Side effects the host (app or simulator) must carry out. The reducer stays
/// pure; effects are data.
public enum AppEffect: Equatable {
    case persistCoordinator(value: String)
    case webViewReload
    case webViewGoBack
    case openExternal(url: String)
}

/// The result of a single `reduce`: the next state, the effects to run, and —
/// for `shouldStartLoad` — whether the navigation loads or is handed off.
public struct ReduceResult: Equatable {
    public var state: AppState
    public var effects: [AppEffect]
    public var loadDecision: LoadDecision?

    public init(state: AppState, effects: [AppEffect] = [], loadDecision: LoadDecision? = nil) {
        self.state = state
        self.effects = effects
        self.loadDecision = loadDecision
    }
}

public func initialState() -> AppState { AppState() }

/// The single source of truth for app behavior. Pure: `(state, action, env) ->
/// (state, effects)`.
public func reduce(_ state: AppState, _ action: AppAction, _ env: AppEnv) -> ReduceResult {
    var s = state

    switch action {
    case let .bootLoaded(storedURL):
        let stored = normalizeCoordinatorURL(storedURL ?? "", allowLocalHTTP: env.allowLocalHTTP)
        let next = stored ?? defaultCoordinatorURL
        s.homeURL = next
        s.currentURL = next
        s.draftURL = next
        s.booting = false
        s.persistedCoordinator = stored
        return ReduceResult(state: s)

    case .openSettings:
        s.draftURL = s.homeURL
        s.urlError = ""
        s.settingsVisible = true
        return ReduceResult(state: s)

    case .closeSettings:
        s.settingsVisible = false
        return ReduceResult(state: s)

    case let .setDraft(url):
        s.draftURL = url
        s.urlError = ""
        return ReduceResult(state: s)

    case .saveCoordinator:
        guard let normalized = normalizeCoordinatorURL(s.draftURL, allowLocalHTTP: env.allowLocalHTTP) else {
            s.urlError = httpsError
            return ReduceResult(state: s)
        }
        s.urlError = ""
        s.homeURL = normalized
        s.currentURL = normalized
        s.loadFailed = false
        s.settingsVisible = false
        s.webViewKey += 1
        s.persistedCoordinator = normalized
        return ReduceResult(state: s, effects: [.persistCoordinator(value: normalized)])

    case .resetCoordinator:
        s.draftURL = defaultCoordinatorURL
        s.homeURL = defaultCoordinatorURL
        s.currentURL = defaultCoordinatorURL
        s.urlError = ""
        s.loadFailed = false
        s.settingsVisible = false
        s.webViewKey += 1
        s.persistedCoordinator = defaultCoordinatorURL
        return ReduceResult(state: s, effects: [.persistCoordinator(value: defaultCoordinatorURL)])

    case .reload:
        s.loadFailed = false
        s.isLoading = true
        return ReduceResult(state: s, effects: [.webViewReload])

    case .goHome:
        s.loadFailed = false
        s.currentURL = s.homeURL
        s.webViewKey += 1
        return ReduceResult(state: s)

    case .goBack:
        return ReduceResult(state: s, effects: state.canGoBack ? [.webViewGoBack] : [])

    case .webViewLoadStart:
        s.progress = 0
        s.isLoading = true
        s.loadFailed = false
        return ReduceResult(state: s)

    case .webViewLoadEnd:
        s.isLoading = false
        return ReduceResult(state: s)

    case let .webViewLoadProgress(progress):
        s.progress = min(max(progress, 0), 1)
        return ReduceResult(state: s)

    case .webViewError:
        s.loadFailed = true
        s.isLoading = false
        return ReduceResult(state: s)

    case let .webViewNavState(url, title, canGoBack):
        s.canGoBack = canGoBack
        s.currentURL = url.isEmpty ? s.homeURL : url
        s.pageTitle = title.isEmpty ? "Crabbox" : title
        return ReduceResult(state: s)

    case let .shouldStartLoad(url):
        if shouldOpenExternally(url) {
            return ReduceResult(state: s, effects: [.openExternal(url: url)], loadDecision: .external)
        }
        return ReduceResult(state: s, loadDecision: .load)
    }
}

// MARK: - Selectors

public func selectCurrentHost(_ state: AppState) -> String {
    hostLabel(state.currentURL.isEmpty ? state.homeURL : state.currentURL)
}

public func selectLoadingWidthPct(_ state: AppState) -> Int {
    max(6, Int((state.progress * 100).rounded()))
}

public enum StatusLabel: String { case offline = "Offline", loading = "Loading", live = "Live" }

public func selectStatusLabel(_ state: AppState) -> StatusLabel {
    if state.loadFailed { return .offline }
    return state.isLoading ? .loading : .live
}
